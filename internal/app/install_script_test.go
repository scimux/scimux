package app

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Inert bytes: these tests install a file and inspect it, and nothing here
// ever executes what was downloaded.
const inertRelease = "inert bytes standing in for a release binary; never executed\n"

// installServer is a release host just real enough for the bootstrap
// installer: the latest-release pointer, the tag page it points at, and the
// two download URLs.
type installServer struct {
	tag     string
	payload []byte
	// badSum publishes a checksum that does not match the payload.
	badSum bool
	// assetStatus answers the binary download with this status instead.
	assetStatus int
}

func (s *installServer) handler(asset string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/releases/latest"):
			http.Redirect(w, r, "/chrberger/scimux/releases/tag/"+s.tag, http.StatusSeeOther)
		case strings.Contains(p, "/releases/tag/"):
			fmt.Fprintf(w, "release %s", s.tag)
		case strings.HasSuffix(p, "/SHA256SUMS"):
			sum := sha256.Sum256(s.payload)
			if s.badSum {
				sum = sha256.Sum256(append(s.payload, 'x'))
			}
			fmt.Fprintf(w, "%x  %s\n", sum, asset)
		case strings.Contains(p, "/releases/download/"):
			if s.assetStatus != 0 {
				w.WriteHeader(s.assetStatus)
				return
			}
			w.Write(s.payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// installAsset names the artifact this machine would download, and skips when
// the machine is outside the released matrix or cannot fetch at all.
func installAsset(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the installer only resolves released linux and darwin binaries")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("the installer only resolves released amd64 and arm64 binaries")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required to exercise the installer")
	}
	return "scimux-" + runtime.GOOS + "-" + runtime.GOARCH
}

// runInstallScript runs the real bootstrap installer against srv, installing
// into dir.
func runInstallScript(t *testing.T, s *installServer, dir string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(s.handler(installAsset(t)))
	t.Cleanup(srv.Close)
	cmd := exec.Command("sh", filepath.Join(repoRootFromTest(t), "scripts", "install.sh"))
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"SCIMUX_INSTALL_DIR="+dir,
		"SCIMUX_REPO_URL="+srv.URL+"/chrberger/scimux",
	)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func assertInstalled(t *testing.T, dir string, want []byte) {
	t.Helper()
	dest := filepath.Join(dir, "scimux")
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("nothing at the advertised command path: %v", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s is not an executable file: %v", dest, st.Mode())
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != string(want) {
		t.Fatalf("the installed file is not the release that was downloaded (err %v)", err)
	}
	assertNoStagingResidue(t, dir)
}

// A staged file left behind is a half-installed release nobody is told about:
// dot-named, executable, sitting next to the real command.
func assertNoStagingResidue(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "scimux" {
			t.Fatalf("the installer left %s behind in %s", e.Name(), dir)
		}
	}
}

// Turning "latest" into a release is the installer's whole job on a fresh
// machine. Release metadata is ordinary, valid data whose shape is the host's
// business, not the installer's, and a resolver that accepts only one spelling
// of it leaves a first-time user reading "could not resolve the latest
// release" against a host that is working perfectly.
func TestInstallResolvesTheLatestRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	out, err := runInstallScript(t, &installServer{tag: "v-review", payload: []byte(inertRelease)}, dir)
	if err != nil {
		t.Fatalf("installing the latest release failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "v-review") {
		t.Fatalf("the installer does not say which release it resolved:\n%s", out)
	}
	assertInstalled(t, dir, []byte(inertRelease))
}

// mv into an existing directory succeeds by moving the file *inside* it, so
// the installer printed "installed: <path>" and exited zero with no runnable
// file at the path it had just advertised.
func TestInstallRejectsADestinationThatIsNotAFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "scimux")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runInstallScript(t, &installServer{tag: "v-review", payload: []byte(inertRelease)}, dir)
	if err == nil {
		t.Fatalf("the installer reported success with nothing runnable installed:\n%s", out)
	}
	if !strings.Contains(out, dest) {
		t.Fatalf("the failure does not say which path is in the way:\n%s", out)
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 0 {
		t.Fatalf("the installer wrote inside the destination instead: %v", ents)
	}
}

// refusal is what the installer said in its own voice. Matching anywhere in
// the output would accept the word "download" out of the URL it prints before
// every run, which a silently aborting script passes too.
func refusal(out string) string {
	var said []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "scimux install: "); ok {
			said = append(said, rest)
		}
	}
	return strings.Join(said, "\n")
}

// Three ordinary ways to fail, one contract for all of them: the scimux the
// user already has keeps working, nothing half-written is left in its
// directory, and the message names the step that gave up.
func TestFailedInstallPreservesThePreviousExecutable(t *testing.T) {
	previous := []byte("the scimux that was already installed\n")
	cases := []struct {
		name   string
		server installServer
		setup  func(t *testing.T, dir string)
		says   string
	}{
		{
			name:   "the release host is broken",
			server: installServer{assetStatus: http.StatusInternalServerError},
			says:   "download",
		},
		{
			name:   "the download does not match its checksum",
			server: installServer{badSum: true},
			says:   "checksum",
		},
		{
			name: "the install directory is not writable",
			setup: func(t *testing.T, dir string) {
				if os.Geteuid() == 0 {
					t.Skip("root ignores the directory mode this case turns on")
				}
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0o755) })
			},
			says: "write",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "scimux")
			if err := os.WriteFile(dest, previous, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			s := tc.server
			s.tag, s.payload = "v-review", []byte(inertRelease)
			out, err := runInstallScript(t, &s, dir)
			if err == nil {
				t.Fatalf("a failed install reported success:\n%s", out)
			}
			if !strings.Contains(strings.ToLower(refusal(out)), tc.says) {
				t.Fatalf("the failure does not name the step that gave up (want %q):\n%s", tc.says, out)
			}
			if got, err := os.ReadFile(dest); err != nil || string(got) != string(previous) {
				t.Fatalf("the scimux that was already installed did not survive (err %v)", err)
			}
			assertNoStagingResidue(t, dir)
		})
	}
}

// Two installs at once is ordinary — an upgrade and a fresh shell, a script
// and a person. A fixed staging name in the shared destination means each
// installer is writing the file the other is about to rename, so one of them
// renames bytes it never downloaded, or finds its own file already gone.
func TestConcurrentInstallsDoNotInterfere(t *testing.T) {
	// Pre-flight on the test goroutine: runInstallScript must not reach a Skip
	// from a worker.
	installAsset(t)
	dir := t.TempDir()
	const n = 6
	payloads := make([][]byte, n)
	outs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		payloads[i] = fmt.Appendf(nil, "inert release %d; never executed\n", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = runInstallScript(t, &installServer{tag: "v-review", payload: payloads[i]}, dir)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("installer %d failed while another was running: %v\n%s", i, errs[i], outs[i])
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "scimux"))
	if err != nil {
		t.Fatalf("nothing at the advertised command path: %v", err)
	}
	for _, want := range payloads {
		if string(got) == string(want) {
			assertInstalled(t, dir, want)
			return
		}
	}
	t.Fatalf("the installed file is a blend of concurrent downloads, not any one release: %q", got)
}
