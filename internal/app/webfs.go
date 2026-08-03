package app

import (
	"io"
	"io/fs"
	"strings"
	"time"
)

// webPrefixFS presents web.Files below a virtual web/ directory. Keeping that
// boundary lets the HTTP layer and its characterization tests describe the
// same asset paths that are present in source control.
type webPrefixFS struct {
	fsys fs.FS
}

func (w webPrefixFS) Open(name string) (fs.File, error) {
	if name == "web" {
		entries, err := fs.ReadDir(w.fsys, ".")
		if err != nil {
			return nil, err
		}
		return &virtualWebDir{entries: entries}, nil
	}
	if !strings.HasPrefix(name, "web/") {
		return nil, fs.ErrNotExist
	}
	return w.fsys.Open(strings.TrimPrefix(name, "web/"))
}

func (w webPrefixFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == "web" {
		return fs.ReadDir(w.fsys, ".")
	}
	if !strings.HasPrefix(name, "web/") {
		return nil, fs.ErrNotExist
	}
	return fs.ReadDir(w.fsys, strings.TrimPrefix(name, "web/"))
}

func (w webPrefixFS) ReadFile(name string) ([]byte, error) {
	if !strings.HasPrefix(name, "web/") {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(w.fsys, strings.TrimPrefix(name, "web/"))
}

type virtualWebDir struct {
	entries []fs.DirEntry
	offset  int
}

func (d *virtualWebDir) Stat() (fs.FileInfo, error) { return virtualWebDirInfo{}, nil }
func (d *virtualWebDir) Read([]byte) (int, error)   { return 0, io.EOF }
func (d *virtualWebDir) Close() error               { return nil }

func (d *virtualWebDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.offset == len(d.entries) && n > 0 {
		return nil, io.EOF
	}
	end := len(d.entries)
	if n > 0 && d.offset+n < end {
		end = d.offset + n
	}
	entries := d.entries[d.offset:end]
	d.offset = end
	return entries, nil
}

type virtualWebDirInfo struct{}

func (virtualWebDirInfo) Name() string       { return "web" }
func (virtualWebDirInfo) Size() int64        { return 0 }
func (virtualWebDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (virtualWebDirInfo) ModTime() time.Time { return time.Time{} }
func (virtualWebDirInfo) IsDir() bool        { return true }
func (virtualWebDirInfo) Sys() any           { return nil }
