//go:build !linux && !darwin && !freebsd

package asset

import "os"

// scimux's shipped targets are covered by open_nonblocking_unix.go. Keep the
// package buildable on other development hosts without claiming their special
// file semantics are identical.
func openNonblocking(path string) (*os.File, error) {
	return os.Open(path)
}
