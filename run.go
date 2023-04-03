// Package fsrun provides the ability to concurrently traverse a filesystem.
package fsrun

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"

	"go.abhg.dev/container/ring"
)

// SkipDir indicates that the children of a directory should be skipped
// during a traversal.
//
// If the path being inspected is not a directory,
// the value is ignored.
var SkipDir = fs.SkipDir

// ignoreSkipDir returns the provided error back
// if it's not a SkipDir error.
func ignoreSkipDir(err error) error {
	if errors.Is(err, SkipDir) {
		return nil
	}
	return err
}

// Func is called during a traversal
// with the path of a file and its directory entry.
// It may be called by multiple goroutines concurrently.
//
// path is the path to the file joined with the root
// provided at the start of the traversal.
//
// The provided err is non-nil if an error was encountered
// while inspecting this path.
// If err is non-nil, ent may be nil.
//
// The function may return [SkipDir] to skip a directory
// during the traversal.
type Func func(path string, ent fs.DirEntry, err error) error

var (
	_ Visitor = Func(nil)
	_         = Func(fs.WalkDirFunc(nil))
)

// Visit implements [Visitor] for Func.
// Use this implementation to pass a Func into [Config.Run].
//
//	cfg := Config{...}
//	cfg.Run(fsys, root, fsrun.Func(
//		func(path string, ent fs.DirEntry, err error) error {
//			// ...
//		}))
func (f Func) Visit(path string, ent fs.DirEntry, err error) error {
	return f(path, ent, err)
}

// WalkDir walks the file tree starting at root concurrently,
// calling fn for each file or directory in the tree (including root).
//
// There are no ordering guarantees except that
// fn is called for a directory before its descendants.
//
// fn may return [SkipDir] to skip a directory's descendants.
// SkipDir will be ignored if the entry is a file.
//
// fn may return any other non-nil error to stop the traversal.
//
// This is a near drop-in concurrent replacement for [fs.WalkDir]
// with one difference:
// SkipDir is respected only if the entry is a directory,
// and it's ignored for files.
//
// See also [Config.Run].
func WalkDir(fsys fs.FS, root string, fn fs.WalkDirFunc) error {
	var cfg Config
	return cfg.Run(fsys, root, Func(fn))
}

// Visitor visits files and directories during a concurrent traversal.
// It is invoked with the path and DirEntry for each file or directory
// in a file tree starting at the root provided to [Config.Run].
// It may be called by multiple goroutines concurrently.
//
// Visitor is passed a non-nil error if an error was encountered
// while inspecting a path.
// In such a scenario, be aware that:
//
//   - Visitor may be called twice: once to visit the path,
//     and once to report the error in inspecting that path.
//   - If the error is non-nil, the accompanying DirEntry may be nil.
type Visitor interface {
	// Visit visits a single file or directory in a traversal.
	//
	// path is the path to the file, joined with the root directory.
	// err is non-nil if there was an error inspecting a file or directory.
	Visit(path string, ent fs.DirEntry, err error) error
}

// Config specifies the configuration for a concurrent filesystem traversal.
//
// For simple use cases, you may prefer to use [WalkDir].
type Config struct {
	// Concurrency specifies the maximum number of concurrent visitors
	// in a traversal.
	//
	// Defaults to GOMAXPROCS.
	Concurrency int
}

// Run traverses the file tree rooted at root concurrently,
// calling the visitor on each file and directory in the tree
// including root.
//
// There are no ordering guarantees except that
// we will visit a directory before its descendants.
//
// If a Visitor returns [SkipDir],
// Run will not visit that directory's descendants.
// SkipDir will be ignored if the path visited was not a directory.
//
// If Run encounters an error inspecting a path after visiting it,
// it may invoke Visitor again for that path with the failure.
// The Visitor may return SkipDir at this point to ignore the error.
//
// If a Visitor returns any other error, Run will halt the traversal
// and return that error.
func (cfg *Config) Run(fsys fs.FS, root string, v Visitor) error {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = runtime.GOMAXPROCS(0)
	}

	rootInfo, err := fs.Stat(fsys, root)
	if err != nil {
		err = v.Visit(root, nil /* entry */, err)
		return ignoreSkipDir(err)
	}

	w := runner{
		fs:   fsys,
		v:    v,
		reqc: make(chan workerRequest),
		resc: make(chan workerResponse, concurrency),
	}

	w.wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go w.Worker()
	}
	defer w.wg.Wait()

	return w.Main(root, fs.FileInfoToDirEntry(rootInfo))
}

// runner implements the core traversal logic.
// It implements two kinds of goroutines:
//
//   - Worker: Calls Visitors on paths as needed.
//     There may be many workers.
//   - Main: Orchestrates the workers and manages state.
//     There must be exactly one Main.
type runner struct {
	fs fs.FS
	v  Visitor
	wg sync.WaitGroup

	// reqc and resc provide communication between
	// Main and Worker(s).
	//
	// Main writes to reqc, reads from resc.
	// Workers read from resc, write to reqc.
	reqc chan workerRequest
	resc chan workerResponse
}

func (w *runner) Main(rootPath string, rootEntry fs.DirEntry) error {
	defer close(w.reqc) // signal stop to workers

	var (
		// Number of items in the queue,
		// or sent off to workers.
		outstanding int

		// Requests that remain to be sent to workers.
		queue ring.Q[workerRequest]
	)

	// Workers are already running by this point so we can start.
	w.reqc <- workerRequest{
		Path:  rootPath,
		Entry: rootEntry,
	}
	outstanding++

	for outstanding > 0 {
		var (
			// This is non-nil only if there's an item in the
			// queue that needs to be sent to workers.
			//
			// The 'reqc <- value' below will never resolve
			// if reqc is nil.
			reqc  chan workerRequest
			value workerRequest
		)
		if !queue.Empty() {
			reqc = w.reqc
			value = queue.Peek()
		}

		select {
		case reqc <- value:
			// Remove from the queue only if we successfully handed
			// this off to a worker.
			queue.Pop()

		case r := <-w.resc:
			outstanding--
			if err := r.Err; err != nil {
				// ignoreSkipDir not applied outside
				// because *any* error causes directory skips.
				// SkipDir is just a no-op error.
				if err := ignoreSkipDir(err); err != nil {
					return err
				}
			} else if len(r.Children) > 0 {
				children := make([]workerRequest, len(r.Children))
				for i, c := range r.Children {
					children[i] = workerRequest{
						Path:  filepath.Join(r.Path, c.Name()),
						Entry: c,
					}
					queue.Push(children[i])
				}
				outstanding += len(r.Children)
			}
		}
	}

	return nil
}

type workerRequest struct {
	Path  string
	Entry fs.DirEntry
}

type workerResponse struct {
	Path string
	// Err is non-nil only if Visit returned a non-nil error
	// from visitng a Path or while handling its ReadDir error.
	Err      error
	Children []fs.DirEntry
}

// Worker reads visit requests off reqc,
// runs them, and puts responses back into resc.
func (w *runner) Worker() {
	defer w.wg.Done()

	for req := range w.reqc {
		res := workerResponse{
			Path: req.Path,
			// TODO: Should we handle unexpected worker termination
			// from runtime.Goexit?
			Err: w.v.Visit(req.Path, req.Entry, nil),
		}

		// If the visit succeeded and we're looking at a directory,
		// inspect the directory.
		// If that fails, visit it again to report the failure.
		if res.Err == nil && req.Entry != nil && req.Entry.IsDir() {
			var err error
			res.Children, err = fs.ReadDir(w.fs, req.Path)
			if err != nil {
				res.Err = w.v.Visit(req.Path, req.Entry, err)
			}
		}

		w.resc <- res
	}
}
