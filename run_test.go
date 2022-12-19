package fsrun

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
)

func TestIgnoreSkipDir(t *testing.T) {
	t.Run("SkipDir", func(t *testing.T) {
		assert.NoError(t, ignoreSkipDir(SkipDir))
	})

	t.Run("fs.SkipDir", func(t *testing.T) {
		assert.NoError(t, ignoreSkipDir(fs.SkipDir))
	})

	t.Run("wrapped SkipDir", func(t *testing.T) {
		err := fmt.Errorf("great sadness: %w", SkipDir)
		assert.NoError(t, ignoreSkipDir(err))
	})

	t.Run("other", func(t *testing.T) {
		err := errors.New("great sadness")
		assert.ErrorIs(t, ignoreSkipDir(err), err)
	})
}

func TestWalkDir(t *testing.T) {
	fsys := fstest.MapFS{
		"a/b/c.txt":           {},
		"foo/bar/baz/qux.txt": {},
	}

	t.Run("simple", func(t *testing.T) {
		var rec walkRecorder
		err := WalkDir(fsys, ".", rec.Visit)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{
			".",
			"a", "a/b", "a/b/c.txt",
			"foo", "foo/bar", "foo/bar/baz", "foo/bar/baz/qux.txt",
		}, rec.Paths)
	})

	t.Run("SkipDir", func(t *testing.T) {
		var rec walkRecorder
		err := WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if path == "a" {
				return fs.SkipDir
			}
			return rec.Visit(path, d, err)
		})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{
			".",
			"foo", "foo/bar", "foo/bar/baz", "foo/bar/baz/qux.txt",
		}, rec.Paths)
	})
}

func TestVisit_file(t *testing.T) {
	fsys := fstest.MapFS{"a/b/c.txt": {}}
	var (
		rec walkRecorder
		cfg Config
	)
	require.NoError(t, cfg.Run(fsys, "a/b/c.txt", &rec))
	assert.Equal(t, []string{"a/b/c.txt"}, rec.Paths)
}

func TestVisit_fileError(t *testing.T) {
	giveErr := errors.New("great sadness")
	fsys := patchedFS{
		FS: fstest.MapFS{"a/b/c.txt": {}},
		StatFunc: func(s string) (fs.FileInfo, error) {
			return nil, giveErr
		},
	}

	var rec walkRecorder
	err := WalkDir(fsys, "a/b/c.txt", func(path string, d fs.DirEntry, err error) error {
		assert.ErrorIs(t, err, giveErr)
		return rec.Visit(path, d, err)
	})
	assert.ErrorIs(t, err, giveErr)
	assert.Equal(t, []string{"a/b/c.txt"}, rec.Paths)
}

func TestVisit_error(t *testing.T) {
	fsys := fstest.MapFS{
		"a/b/c.txt": {},
		"a/b/d.txt": {},
		"a/b/e.txt": {},
		"a/b/f.txt": {},

		"foo/bar/baz.txt":  {},
		"foo/bar/qux.txt":  {},
		"foo/bar/quux.txt": {},
	}

	var rec walkRecorder
	giveErr := errors.New("great sadness")
	err := WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if path == "a/b/e.txt" {
			return giveErr
		}
		return rec.Visit(path, d, err)
	})
	assert.ErrorIs(t, err, giveErr)

	// The only guarantee is that a/b/e.txt's ancestors are present.
	assert.Contains(t, rec.Paths, ".")
	assert.Contains(t, rec.Paths, "a")
	assert.Contains(t, rec.Paths, "a/b")
}

func TestVisit_skipDir(t *testing.T) {
	fsys := fstest.MapFS{
		"a/b/c.txt": {},
		"a/b/d.txt": {},
		"a/b/e.txt": {},
		"a/b/f.txt": {},

		"foo/bar/baz.txt":  {},
		"foo/bar/qux.txt":  {},
		"foo/bar/quux.txt": {},
	}

	var rec walkRecorder
	err := WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if path == "a/b" || path == "foo/bar" {
			return SkipDir
		}
		return rec.Visit(path, d, err)
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{".", "a", "foo"}, rec.Paths)
}

func TestVisit_readDirError(t *testing.T) {
	giveErr := errors.New("great sadness")

	innerFS := fstest.MapFS{
		"a/b/c.txt":   {},
		"a/b/d.txt":   {},
		"a/b/e.txt":   {},
		"a/b/f.txt":   {},
		"foo/bar.txt": {},
	}
	fsys := patchedFS{
		FS: innerFS,
		ReadDirFunc: func(path string) ([]fs.DirEntry, error) {
			if path == "a/b" {
				return nil, giveErr
			}
			return fs.ReadDir(innerFS, path)
		},
	}

	var (
		rec           walkRecorder
		badDirCounter atomic.Int32
	)
	err := WalkDir(&fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if path == "a/b" && badDirCounter.Add(1) == 2 {
			assert.ErrorIs(t, err, giveErr)
			return SkipDir
		}
		return rec.Visit(path, d, err)
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		".",
		"a", "a/b",
		"foo", "foo/bar.txt",
	}, rec.Paths)
	assert.Equal(t, int32(2), badDirCounter.Load(),
		"bad directory should be visited twice")
}

func TestVisit_concurrency(t *testing.T) {
	const (
		N     = 8
		Count = 100
	)

	fsys := make(fstest.MapFS)
	wantPaths := make([]string, 0, Count)
	for i := 0; i < Count; i++ {
		path := fmt.Sprintf("foo/bar-%d.txt", i)
		fsys[path] = &fstest.MapFile{
			Data: strconv.AppendInt(nil, int64(i), 10),
		}
		wantPaths = append(wantPaths, path)
	}

	var (
		ready atomic.Bool
		wg    sync.WaitGroup
		rec   walkRecorder
	)
	wg.Add(N)
	visit := func(path string, ent fs.DirEntry, err error) error {
		// There won't be 8 tasks to handle concurrently
		// until we've already visited "foo"
		// and are ready to look at its children.
		if path == "foo" {
			return nil
		}

		// Inform the WaitGroup that this goroutine is ready
		// and wait for its friends.
		//
		// This will deadlock if there aren't 8 concurrent workers.
		if !ready.Load() {
			wg.Done()
			wg.Wait()
			ready.CompareAndSwap(false, true)
		}

		return rec.Visit(path, ent, err)
	}

	cfg := Config{Concurrency: N}
	require.NoError(t, cfg.Run(fsys, "foo", Func(visit)))
	assert.ElementsMatch(t, wantPaths, rec.Paths)
}

func TestVisit_bigDirFS(t *testing.T) {
	// This test generates a large file tree
	// inside a temporary directory
	// and traverses that with fsrun.
	const (
		Depth   = 10
		Breadth = 100
	)

	var (
		populate  func(dir string, parts []string)
		wantDirs  []string
		wantFiles []string
	)
	populate = func(dir string, parts []string) {
		wantDirs = append(wantDirs, dir)
		for i := 0; i < Breadth; i++ {
			file := filepath.Join(dir, fmt.Sprintf("file-%d.txt", i))
			require.NoError(t, os.WriteFile(file, nil, 0o644))
			wantFiles = append(wantFiles, file)
		}

		if len(parts) < Depth {
			subdir := filepath.Join(dir, fmt.Sprintf("dir-%d", len(parts)))
			require.NoError(t, os.Mkdir(subdir, 0o1755))
			populate(subdir, append(parts, subdir))
		}
	}

	root := t.TempDir()
	populate(root, nil)

	var (
		mu       sync.Mutex
		gotDirs  []string
		gotFiles []string
	)
	err := WalkDir(os.DirFS(root), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		path = filepath.Join(root, path)

		mu.Lock()
		if d.IsDir() {
			gotDirs = append(gotDirs, path)
		} else {
			gotFiles = append(gotFiles, path)
		}
		mu.Unlock()

		return nil
	})
	require.NoError(t, err)

	assert.ElementsMatch(t, wantDirs, gotDirs)
	assert.ElementsMatch(t, wantFiles, gotFiles)
}

type walkRecorder struct {
	mu    sync.Mutex
	Paths []string
}

func (r *walkRecorder) Visit(path string, ent fs.DirEntry, err error) error {
	r.mu.Lock()
	r.Paths = append(r.Paths, path)
	r.mu.Unlock()
	return err
}

type patchedFS struct {
	fs.FS

	StatFunc    func(string) (fs.FileInfo, error)
	ReadDirFunc func(string) ([]fs.DirEntry, error)
}

func (f patchedFS) Stat(name string) (fs.FileInfo, error) {
	if f.StatFunc != nil {
		return f.StatFunc(name)
	}
	return fs.Stat(f.FS, name)
}

func (f patchedFS) ReadDir(dir string) ([]fs.DirEntry, error) {
	if f.ReadDirFunc != nil {
		return f.ReadDirFunc(dir)
	}
	return fs.ReadDir(f.FS, dir)
}
