package app

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/scimux/scimux/internal/identity"
)

// ensureInstallationIdentity records the data directory's installation ID.
//
// The ID is a public label, not a credential. This function does not store
// it, log it, or return it. A missing file is created. An existing file,
// including a damaged one, is left unchanged. On error, one line is written
// to errOut when errOut is non-nil, and the caller continues. The line names
// the file and the cause. It never contains the file's content.
func ensureInstallationIdentity(dataDir string, errOut io.Writer) {
	_, err := identity.LoadOrCreateInstallation(dataDir)
	if err == nil || errOut == nil {
		return
	}
	// The path is quoted so an unusual directory name cannot split the line.
	path := filepath.Join(dataDir, identity.InstallationFile)
	if errors.Is(err, identity.ErrInvalidInstallation) {
		fmt.Fprintf(errOut, "scimux: installation identity is unavailable: %q is damaged or unsupported and was left unchanged\n", path)
		return
	}
	// The innermost error is the bare cause. The wrappers repeat the path
	// unquoted.
	cause := err
	for next := errors.Unwrap(cause); next != nil; next = errors.Unwrap(cause) {
		cause = next
	}
	fmt.Fprintf(errOut, "scimux: installation identity is unavailable: %q: %v\n", path, cause)
}
