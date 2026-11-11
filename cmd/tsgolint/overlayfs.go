package main

import (
	"time"

	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs"
)

type overlayFS struct {
	underlying vfs.FS
	overrides  map[string]string
}

func newOverlayFS(underlying vfs.FS, overrides map[string]string) vfs.FS {
	fs := &overlayFS{
		underlying: underlying,
		overrides:  make(map[string]string, len(overrides)),
	}
	for path, content := range overrides {
		fs.overrides[fs.canonicalPath(path)] = content
	}
	return fs
}

func (o *overlayFS) canonicalPath(path string) string {
	return o.underlying.CaseSensitivity().Canonicalize(tspath.NormalizePath(path))
}

func (o *overlayFS) CaseSensitivity() tspath.CaseSensitivity {
	return o.underlying.CaseSensitivity()
}

func (o *overlayFS) FileExists(path tspath.RootedFilePath) bool {
	if _, ok := o.overrides[o.canonicalPath(path.AsString())]; ok {
		return true
	}
	return o.underlying.FileExists(path)
}

func (o *overlayFS) ReadFile(path tspath.RootedFilePath) (string, bool) {
	if content, ok := o.overrides[o.canonicalPath(path.AsString())]; ok {
		return content, true
	}
	return o.underlying.ReadFile(path)
}

func (o *overlayFS) WriteFile(path tspath.RootedFilePath, data string) error {
	return o.underlying.WriteFile(path, data)
}

func (o *overlayFS) AppendFile(path tspath.RootedFilePath, data string) error {
	return o.underlying.AppendFile(path, data)
}

func (o *overlayFS) Remove(path tspath.RootedPath) error {
	return o.underlying.Remove(path)
}

func (o *overlayFS) Chtimes(path tspath.RootedPath, aTime time.Time, mTime time.Time) error {
	return o.underlying.Chtimes(path, aTime, mTime)
}

func (o *overlayFS) DirectoryExists(path tspath.RootedDirectoryPath) bool {
	return o.underlying.DirectoryExists(path)
}

func (o *overlayFS) GetAccessibleEntries(path tspath.RootedDirectoryPath) vfs.Entries {
	return o.underlying.GetAccessibleEntries(path)
}

func (o *overlayFS) Stat(path tspath.RootedPath) vfs.FileInfo {
	return o.underlying.Stat(path)
}

func (o *overlayFS) Realpath(path tspath.RootedPath) tspath.RootedPath {
	return o.underlying.Realpath(path)
}
