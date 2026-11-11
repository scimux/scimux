package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// InstallationID is the public label of one data directory. It is not a
// credential and must never be accepted as proof of identity. A copied data
// directory is the same installation by definition.
type InstallationID string

// InstallationFile is the name of the installation record inside the data
// directory.
const InstallationFile = "installation.json"

// installationFileLimit is the most of an installation file that is read.
// A larger file is damaged, not a bigger record.
const installationFileLimit = 4096

// ErrInvalidInstallation means the installation file is present but damaged
// or uses an unsupported shape. Callers must not replace that file.
var ErrInvalidInstallation = errors.New("identity: installation file is damaged or unsupported")

// installationEntropy supplies the 16 bytes of a new installation ID. Tests
// replace it with a reader that fails. There is no timestamp fallback.
var installationEntropy io.Reader = rand.Reader

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

var installationOps = installationIO{
	createTemp: func(dir string) (*os.File, error) {
		return os.CreateTemp(dir, ".installation-*.tmp")
	},
	chmod:    func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) },
	write:    func(f *os.File, b []byte) (int, error) { return f.Write(b) },
	syncFile: func(f *os.File) error { return f.Sync() },
	close:    func(f *os.File) error { return f.Close() },
	link:     os.Link,
	remove:   os.Remove,
}

// syncDirectory fsyncs dir so a newly published installation file's directory
// entry survives a crash.
var syncDirectory = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readInstallationBytes reads one installation file, one byte past the limit
// so a larger file can be rejected without reading the rest of it.
var readInstallationBytes = func(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, installationFileLimit+1))
}

// ParseInstallationID accepts exactly 32 lowercase hexadecimal characters.
func ParseInstallationID(s string) (InstallationID, error) {
	if len(s) != 32 {
		return "", ErrInvalidInstallation
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", ErrInvalidInstallation
		}
	}
	return InstallationID(s), nil
}

// LoadInstallation reads the installation file. A missing file returns an
// error wrapping fs.ErrNotExist. A damaged file returns ErrInvalidInstallation
// and is left untouched.
func LoadInstallation(dataDir string) (InstallationID, error) {
	f, err := os.Open(filepath.Join(dataDir, InstallationFile))
	if err != nil {
		return "", fmt.Errorf("identity: open installation: %w", err)
	}
	defer f.Close()
	b, err := readInstallationBytes(f)
	if err != nil {
		return "", fmt.Errorf("identity: read installation: %w", err)
	}
	if len(b) > installationFileLimit {
		return "", ErrInvalidInstallation
	}
	return parseInstallationBytes(b)
}

// LoadOrCreateInstallation returns the installation ID, creating the file
// only when it is absent. It never replaces, repairs, or regenerates an
// existing file. The ID is not a credential and must never be accepted as
// proof of identity.
func LoadOrCreateInstallation(dataDir string) (InstallationID, error) {
	id, err := LoadInstallation(dataDir)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if err := createInstallation(dataDir); err != nil {
		return "", err
	}
	return LoadInstallation(dataDir)
}

type installationRecord struct {
	V         int    `json:"v"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func parseInstallationBytes(b []byte) (InstallationID, error) {
	var doc installationRecord
	if err := json.Unmarshal(b, &doc); err != nil || doc.V != 1 {
		return "", ErrInvalidInstallation
	}
	id, err := ParseInstallationID(doc.ID)
	if err != nil {
		return "", ErrInvalidInstallation
	}
	return id, nil
}

// createInstallation publishes a new installation file. The ID returned to
// the caller is read back from the final name, including when another
// process wins the race to create it.
func createInstallation(dataDir string) error {
	var entropy [16]byte
	if _, err := io.ReadFull(installationEntropy, entropy[:]); err != nil {
		return fmt.Errorf("identity: mint installation id: %w", err)
	}
	// Both values are drawn from a closed ASCII set (lowercase hex and
	// RFC3339 UTC), so concatenation cannot produce broken JSON.
	id := hex.EncodeToString(entropy[:])
	created := time.Now().UTC().Format(time.RFC3339)
	body := []byte(`{"v":1,"id":"` + id + `","created_at":"` + created + `"}` + "\n")

	f, err := installationOps.createTemp(dataDir)
	if err != nil {
		return fmt.Errorf("identity: create installation file: %w", err)
	}
	tmpName := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = installationOps.close(f)
		}
		if tmpName != "" {
			_ = installationOps.remove(tmpName)
		}
	}()
	if err := installationOps.chmod(f, 0o600); err != nil {
		return fmt.Errorf("identity: chmod installation file: %w", err)
	}
	n, err := installationOps.write(f, body)
	if err != nil {
		return fmt.Errorf("identity: write installation file: %w", err)
	}
	if n != len(body) {
		return fmt.Errorf("identity: write installation file: %w", io.ErrShortWrite)
	}
	if err := installationOps.syncFile(f); err != nil {
		return fmt.Errorf("identity: sync installation file: %w", err)
	}
	if err := installationOps.close(f); err != nil {
		return fmt.Errorf("identity: close installation file: %w", err)
	}
	closed = true
	if beforeInstallationLink != nil {
		beforeInstallationLink(dataDir)
	}
	finalName := filepath.Join(dataDir, InstallationFile)
	if err := installationOps.link(tmpName, finalName); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("identity: publish installation file: %w", err)
	}
	if err := installationOps.remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("identity: remove installation temp: %w", err)
	}
	tmpName = ""
	if err := syncDirectory(dataDir); err != nil {
		return fmt.Errorf("identity: sync installation directory: %w", err)
	}
	return nil
}
