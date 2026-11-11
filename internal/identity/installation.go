package identity

import (
	"errors"
	"io"
	"os"
)

// InstallationID is the public label of one data directory. It is not a
// credential and must never be accepted as proof of identity. A copied data
// directory is the same installation by definition.
type InstallationID string

// InstallationFile is the name of the installation record inside the data
// directory.
const InstallationFile = "installation.json"

// ErrInvalidInstallation means the installation file is present but damaged
// or uses an unsupported shape. Callers must not replace that file.
var ErrInvalidInstallation = errors.New("identity: installation file is damaged or unsupported")

// installationEntropy supplies the 16 bytes of a new installation ID. Tests
// replace it with a reader that fails. There is no timestamp fallback.
var installationEntropy io.Reader

// beforeInstallationLink runs after a new installation file is durable and
// before it is linked into place. Tests plant the winning file here. It is
// nil outside tests.
var beforeInstallationLink func(dir string)

// installationIO is the filesystem seam. The defaults are the real OS calls.
// Tests replace individual functions to force failures that cannot be
// provoked portably when the tests run as root.
type installationIO struct {
	createTemp func(dir string) (*os.File, error)
	chmod      func(f *os.File, mode os.FileMode) error
	write      func(f *os.File, b []byte) (int, error)
	syncFile   func(f *os.File) error
	close      func(f *os.File) error
	link       func(oldname, newname string) error
	remove     func(name string) error
}

var installationOps installationIO

// syncDirectory fsyncs dir so a newly published installation file's directory
// entry survives a crash.
var syncDirectory = func(dir string) error { return nil }

// readInstallationBytes reads the installation file. The limit that makes a
// larger file invalid is applied by the caller.
var readInstallationBytes = func(r io.Reader) ([]byte, error) { return nil, nil }

// ParseInstallationID accepts exactly 32 lowercase hexadecimal characters.
func ParseInstallationID(s string) (InstallationID, error) {
	return "", nil
}

// LoadInstallation reads the installation file. A missing file returns an
// error wrapping fs.ErrNotExist. A damaged file returns ErrInvalidInstallation
// and is left untouched.
func LoadInstallation(dataDir string) (InstallationID, error) {
	return "", nil
}

// LoadOrCreateInstallation returns the installation ID, creating the file
// only when it is absent. It never replaces, repairs, or regenerates an
// existing file. The ID is not a credential and must never be accepted as
// proof of identity.
func LoadOrCreateInstallation(dataDir string) (InstallationID, error) {
	return "", nil
}
