package utils

import (
	"io/fs"
	"strings"
	"time"

	"github.com/microsoft/TypeScript/tsc/shim/bundled"
	"github.com/microsoft/TypeScript/tsc/shim/tspath"
	"github.com/microsoft/TypeScript/tsc/shim/vfs"
	"github.com/microsoft/TypeScript/tsc/shim/vfs/osvfs"
)

type OverlayVFS struct {
	fs           vfs.FS
	VirtualFiles map[string]string
}

var _ vfs.FS = (*OverlayVFS)(nil)

func (vfs *OverlayVFS) CaseSensitivity() tspath.CaseSensitivity {
	return vfs.fs.CaseSensitivity()
}

func (vfs *OverlayVFS) FileExists(path tspath.RootedFilePath) bool {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		return ok
	}
	return vfs.fs.FileExists(path)
}

func (vfs *OverlayVFS) ReadFile(path tspath.RootedFilePath) (contents string, ok bool) {
	if src, ok := vfs.VirtualFiles[path.AsString()]; ok {
		return src, ok
	}
	return vfs.fs.ReadFile(path)
}

func (vfs *OverlayVFS) DirectoryExists(path tspath.RootedDirectoryPath) bool {
	normalizedPath := path.AsString()
	if !strings.HasSuffix(normalizedPath, "/") {
		normalizedPath = normalizedPath + "/"
	}
	for virtualFilePath := range vfs.VirtualFiles {
		if strings.HasPrefix(virtualFilePath, normalizedPath) {
			return true
		}
	}

	return vfs.fs.DirectoryExists(path)
}

func (vfs *OverlayVFS) GetAccessibleEntries(path tspath.RootedDirectoryPath) (result vfs.Entries) {
	result = vfs.fs.GetAccessibleEntries(path)

	normalizedPath := path.AsString()
	if !strings.HasSuffix(normalizedPath, "/") {
		normalizedPath = normalizedPath + "/"
	}

	for virtualFilePath := range vfs.VirtualFiles {
		withoutPrefix, found := strings.CutPrefix(virtualFilePath, normalizedPath)
		if !found {
			continue
		}

		if before, _, ok := strings.Cut(withoutPrefix, "/"); ok {
			result.Directories = append(result.Directories, before)
		} else {
			result.Files = append(result.Files, withoutPrefix)
		}
	}

	return result
}

type overlayVFSFileInfo struct {
	mode fs.FileMode
	name string
	size int64
}

var (
	_ fs.FileInfo = (*overlayVFSFileInfo)(nil)
	_ fs.DirEntry = (*overlayVFSFileInfo)(nil)
)

func (fi *overlayVFSFileInfo) IsDir() bool {
	return fi.mode.IsDir()
}

func (fi *overlayVFSFileInfo) ModTime() time.Time {
	return time.Time{}
}

func (fi *overlayVFSFileInfo) Mode() fs.FileMode {
	return fi.mode
}

func (fi *overlayVFSFileInfo) Name() string {
	return fi.name
}

func (fi *overlayVFSFileInfo) Size() int64 {
	return fi.size
}

func (fi *overlayVFSFileInfo) Sys() any {
	return nil
}

func (fi *overlayVFSFileInfo) Info() (fs.FileInfo, error) {
	return fi, nil
}

func (fi *overlayVFSFileInfo) Type() fs.FileMode {
	return fi.mode.Type()
}

func (vfs *OverlayVFS) Stat(path tspath.RootedPath) vfs.FileInfo {
	if src, ok := vfs.VirtualFiles[path.AsString()]; ok {
		return &overlayVFSFileInfo{
			name: path.AsString(),
			size: int64(len(src)),
		}
	}
	return vfs.fs.Stat(path)
}

func (vfs *OverlayVFS) Realpath(path tspath.RootedPath) tspath.RootedPath {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		return path
	}
	return vfs.fs.Realpath(path)
}

func (vfs *OverlayVFS) WriteFile(path tspath.RootedFilePath, data string) error {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		panic("not implemented: cannot write to overlay file system")
	}
	return vfs.fs.WriteFile(path, data)
}

func (vfs *OverlayVFS) AppendFile(path tspath.RootedFilePath, data string) error {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		panic("not implemented: cannot append to overlay file system")
	}
	return vfs.fs.AppendFile(path, data)
}

func (vfs *OverlayVFS) Remove(path tspath.RootedPath) error {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		panic("not implemented: cannot remove from overlay file system")
	}
	return vfs.fs.Remove(path)
}

func (vfs *OverlayVFS) Chtimes(path tspath.RootedPath, aTime time.Time, mTime time.Time) error {
	if _, ok := vfs.VirtualFiles[path.AsString()]; ok {
		panic("not implemented: cannot change times on overlay file system")
	}
	return vfs.fs.Chtimes(path, aTime, mTime)
}

func NewOverlayVFS(baseFS vfs.FS, virtualFiles map[string]string) vfs.FS {
	return &OverlayVFS{
		baseFS,
		virtualFiles,
	}
}
func NewOverlayVFSForFile(filePath string, source string) vfs.FS {
	virtualFiles := make(map[string]string, 1)
	virtualFiles[filePath] = source
	return &OverlayVFS{
		bundled.WrapFS(osvfs.FS()),
		virtualFiles,
	}
}
