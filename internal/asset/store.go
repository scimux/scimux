package asset

import (
	"io"
	"os"
	"path/filepath"

	"github.com/scimux/scimux/internal/privatefs"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/storagebudget"
)

// NodeDir returns the node-scoped blob directory under assetsRoot
// (~/.scimux/assets/<node-id>/ in the running app).
func NodeDir(assetsRoot, nodeID string) string {
	return filepath.Join(assetsRoot, nodeID)
}

// WriteBlob writes an eligible asset's bytes under the node's blob directory
// and returns its store-relative path ("<node-id>/<id>-<safe-name>"), the
// value recorded as AssetEvent.BlobPath. The asset ID already guarantees
// uniqueness, so no extra random token is needed in the filename (unlike
// attachment uploads, which have no such ID); O_EXCL still guards against a
// same-ID rewrite silently clobbering existing bytes. Directory 0700, file
// 0600, fsync file then parent dir on create — the same durability contract
// as sessionlog.Writer.Append.
func WriteBlob(assetsRoot, nodeID, id, name string, data []byte) (relPath string, err error) {
	dir := NodeDir(assetsRoot, nodeID)
	if err := privatefs.EnsureDir(dir, 0o700); err != nil {
		return "", err
	}
	releaseBudget, err := storagebudget.Reserve(filepath.Dir(assetsRoot), nodeID, int64(len(data)))
	if err != nil {
		return "", err
	}
	defer releaseBudget()
	fname := id + "-" + sessionlog.SanitizeAssetName(name)
	path := filepath.Join(dir, fname)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	if err := sessionlog.SyncParentDir(path); err != nil {
		return "", err
	}
	return filepath.Join(nodeID, fname), nil
}

// ResolveBlobPath validates a recorded BlobPath against the node it claims
// to belong to and returns the absolute file path if, and only if, it
// resolves to a regular file contained within that node's own blob
// directory — never another node's, never outside assetsRoot entirely. This
// is the containment check the download endpoint relies on: BlobPath comes
// from the node's own session log, but this still closes off a corrupted or
// crafted record reaching outside its node.
func ResolveBlobPath(assetsRoot, nodeID, blobPath string) (string, error) {
	dir := filepath.Clean(NodeDir(assetsRoot, nodeID))
	full := filepath.Clean(filepath.Join(assetsRoot, blobPath))
	if !withinRoot(full, dir) {
		return "", ErrOutsideRoot
	}
	fi, err := os.Stat(full)
	if err != nil {
		return "", ErrNotFound
	}
	if !fi.Mode().IsRegular() {
		return "", ErrNotFound
	}
	return full, nil
}

// ReadBlob resolves and reads a blob's bytes in one step (used by tests and
// any caller that wants bytes rather than a path to stream from).
func ReadBlob(assetsRoot, nodeID, blobPath string) ([]byte, error) {
	full, err := ResolveBlobPath(assetsRoot, nodeID, blobPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}
