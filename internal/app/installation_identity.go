package app

import (
	"fmt"
	"io"

	"github.com/scimux/scimux/internal/identity"
)

// ensureInstallationIdentity records the data directory's installation ID.
//
// The ID is a public label, not a credential. This function does not store
// it, log it, or return it. A missing file is created. An existing file,
// including a damaged one, is left unchanged. On error, one line is written
// to errOut when errOut is non-nil, and the caller continues.
func ensureInstallationIdentity(dataDir string, errOut io.Writer) {
	if _, err := identity.LoadOrCreateInstallation(dataDir); err != nil && errOut != nil {
		fmt.Fprintln(errOut, "scimux: installation identity is unavailable")
	}
}
