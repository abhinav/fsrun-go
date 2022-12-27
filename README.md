# fsrun

[![Go Reference](https://pkg.go.dev/badge/go.abhg.dev/fsrun.svg)](https://pkg.go.dev/go.abhg.dev/fsrun)
[![Go](https://github.com/abhinav/fsrun-go/actions/workflows/go.yml/badge.svg)](https://github.com/abhinav/fsrun-go/actions/workflows/go.yml)
[![codecov](https://codecov.io/gh/abhinav/fsrun-go/branch/main/graph/badge.svg?token=W98KYF8SPE)](https://codecov.io/gh/abhinav/fsrun-go)

fsrun is a library for Go
providing the ability to traverse a file tree concurrently.

It is almost a drop-in replacement for
[`fs.WalkDir`](https://pkg.go.dev/io/fs#WalkDir).
See [Replacing fs.WalkDir](#replacing-fswalkdir)
for information on when it cannot replace fs.WalkDir.

## Installation

Use Go modules to install the package.

```bash
go get go.abhg.dev/fsrun@latest
```

## Usage

Import the `fsrun` package:

```go
import "go.abhg.dev/fsrun"
```

Then use `fsrun.WalkDir`:

```go
fsrun.WalkDir(fsys, ".", func(path string, ent fs.DirEntry, err error) error {
    if err != nil {
        // There was an error inspecting the path.
        // Fail the traversal.
        return err
        // Alternatively, return fsrun.SkipDir to ignore the failure.
    }

    fmt.Println(path)
    return nil
})
```

You can use any `fs.FS` instance here.
This includes
[os.DirFS](https://pkg.go.dev/os#DirFS),
[embed.FS](https://pkg.go.dev/embed), and
[fs.Sub](https://pkg.go.dev/io/fs#Sub).

### Controlling concurrency

By default, fsrun will use GOMAXPROCS goroutines for its traversal.
You can change this by using `fsrun.Config`:

```go
cfg := fsrun.Config{
    Concurrency: 8,
}

err := cfg.Run(fsys, root, visitor)
```

`Config.Run` expects an implementation of the `Visitor` interface,
instead of a function reference.

### Replacing fs.WalkDir

If you're using `fs.WalkDir`,
you should be able to switch to `fsrun.WalkDir`
for most use cases without any changes.

```diff
-fs.WalkDir(
+fsrun.WalkDir(
     fsys,
     root,
     func(path string, d fs.DirEntry, err error) error {
         // ...
     },
)
```

There is one behavioral difference between the two:
fsrun respects [fs.SkipDir](https://pkg.go.dev/io/fs#SkipDir)
only for directories.
Returning SkipDir for files has no effect.

## License

This software is available under the MIT License.
See LICENSE for more details.
