package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// releaseUploadServer is a GitHub release API just real enough to answer the
// script: it hands out a release id, records what was uploaded, and lets a
// test decide what the asset endpoint says.
type releaseUploadServer struct {
	mu sync.Mutex
	// uploaded is what the server actually accepted, in arrival order.
	uploaded []string
	// rejectUpload names an asset the server answers with 403, the way a
	// revoked or under-scoped token would be answered.
	rejectUpload string
	// hideFromListing names an asset the server accepts but then omits from
	// its listing — a write that reported success and did not land.
	hideFromListing string
}

func (s *releaseUploadServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/releases/tags/v1.2.3"):
			w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(path, "/releases/42/assets") && r.Method == http.MethodPost:
			name := r.URL.Query().Get("name")
			s.mu.Lock()
			defer s.mu.Unlock()
			if name == s.rejectUpload {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"message":"token does not have write:package scope"}`))
				return
			}
			s.uploaded = append(s.uploaded, name)
			w.Write([]byte(fmt.Sprintf(`{"id": 1, "name": %q}`, name)))
		case strings.HasSuffix(path, "/releases/42/assets") && r.Method == http.MethodGet:
			s.mu.Lock()
			defer s.mu.Unlock()
			var out []map[string]any
			for _, n := range s.uploaded {
				if n == s.hideFromListing {
					continue
				}
				out = append(out, map[string]any{"id": 1, "name": n})
			}
			if out == nil {
				out = []map[string]any{}
			}
			json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// runReleaseScript runs the real release script against srv with two assets,
// returning its combined output and exit status.
func runReleaseScript(t *testing.T, s *releaseUploadServer) (string, error) {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required to exercise the release script", tool)
		}
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	for _, name := range []string{"scimux-linux-amd64", "SHA256SUMS"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(repoRootFromTest(t), ".github", "github-release.sh")
	cmd := exec.Command("bash", script, "v1.2.3", dir)
	cmd.Env = append(os.Environ(),
		"GITHUB_RELEASE_TOKEN=test-token",
		"GITHUB_REPOSITORY=scimux/scimux",
		"GITHUB_API_URL="+srv.URL,
		"GITHUB_UPLOAD_URL="+srv.URL,
	)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// A publish step that cannot fail is not a publish step. Uploading an asset is
// exactly where a token expires or loses a scope, and the server says so in
// the HTTP status — which plain `curl -s` discards, returning 0 for a 403 it
// printed nothing about. The release then completes, green, with assets
// missing, and the first person to find out is a user following a download
// link.
func TestReleaseUploadFailsWhenTheServerRejectsAnAsset(t *testing.T) {
	out, err := runReleaseScript(t, &releaseUploadServer{rejectUpload: "SHA256SUMS"})
	if err == nil {
		t.Fatalf("a rejected upload left the release script reporting success:\n%s", out)
	}
	if !strings.Contains(out, "SHA256SUMS") {
		t.Fatalf("the failure does not name the asset that was rejected:\n%s", out)
	}
}

// The status line is the server's claim about one request; the asset listing
// is what the release actually holds. Only the second one is what users
// download, so it is the one worth believing.
func TestReleaseUploadFailsWhenAnAssetNeverLanded(t *testing.T) {
	out, err := runReleaseScript(t, &releaseUploadServer{hideFromListing: "scimux-linux-amd64"})
	if err == nil {
		t.Fatalf("an asset absent from the published release left the script reporting success:\n%s", out)
	}
	if !strings.Contains(out, "scimux-linux-amd64") {
		t.Fatalf("the failure does not name the missing asset:\n%s", out)
	}
}

// The other half of the contract, and not redundant: "fail on rejection" and
// "fail on a missing asset" are both satisfied by a script that always fails.
func TestReleaseUploadSucceedsWhenEveryAssetLands(t *testing.T) {
	out, err := runReleaseScript(t, &releaseUploadServer{})
	if err != nil {
		t.Fatalf("a release whose assets all landed was reported as a failure: %v\n%s", err, out)
	}
}
