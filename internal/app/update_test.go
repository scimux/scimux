package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/acp/codex"
)

// fakeForgejo serves /releases/latest plus the named assets, mimicking the
// Codeberg release API shape the updater consumes.
func fakeForgejo(t *testing.T, tag string, assets map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		type asset struct {
			Name        string `json:"name"`
			DownloadURL string `json:"browser_download_url"`
		}
		var list []asset
		for name := range assets {
			list = append(list, asset{Name: name, DownloadURL: srv.URL + "/assets/" + name})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag, "html_url": srv.URL + "/rel", "body": "notes for " + tag,
			"assets": list,
		})
	})
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := assets[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	return srv
}

func withUpdateSeams(t *testing.T, apiBase, ver string) {
	t.Helper()
	oldBase, oldVer := releaseAPIBase, version
	releaseAPIBase, version = apiBase, ver
	t.Cleanup(func() { releaseAPIBase, version = oldBase, oldVer })
}

func TestUpdateCheck(t *testing.T) {
	srv := fakeForgejo(t, "v9.9.9", nil)
	for _, tc := range []struct {
		current   string
		available bool
	}{
		{"v1.0.0", true},  // older release installed
		{"v9.9.9", false}, // same tag
		{"dev", false},    // unstamped builds never offer an update
	} {
		withUpdateSeams(t, srv.URL, tc.current)
		rec := httptest.NewRecorder()
		handleUpdateCheck(rec, httptest.NewRequest("GET", "/api/update/check", nil))
		if rec.Code != 200 {
			t.Fatalf("current=%s: status %d: %s", tc.current, rec.Code, rec.Body)
		}
		var got struct {
			Current, Latest, URL, Notes string
			Available                   bool
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Current != tc.current || got.Latest != "v9.9.9" || got.Available != tc.available {
			t.Errorf("current=%s: got %+v, want available=%v", tc.current, got, tc.available)
		}
		if got.Notes == "" || got.URL == "" {
			t.Errorf("current=%s: notes/url missing: %+v", tc.current, got)
		}
	}
}

func TestUpdateCheckAPIDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	withUpdateSeams(t, srv.URL, "v1.0.0")
	rec := httptest.NewRecorder()
	handleUpdateCheck(rec, httptest.NewRequest("GET", "/api/update/check", nil))
	if rec.Code != 502 {
		t.Fatalf("status %d, want 502", rec.Code)
	}
}

func newUpdateTestApp(t *testing.T) *app {
	t.Helper()
	dir := t.TempDir()
	return &app{
		acp:   acpManager{acp.NewManager(dir)},
		codex: codexManager{codex.NewManager(dir)},
	}
}

// goosArch mirrors the handler's asset naming so the tests are arch-independent.
func goosArch() string { return runtime.GOOS + "-" + runtime.GOARCH }

func TestUpdateApplyInstallsVerifiedBinary(t *testing.T) {
	newBin := []byte("#!/bin/true fake new scimux binary")
	name := "scimux-" + goosArch()
	sum := sha256.Sum256(newBin)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	srv := fakeForgejo(t, "v9.9.9", map[string][]byte{
		name: newBin, "SHA256SUMS": []byte(sums),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")

	exe := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	execed := make(chan string, 1)
	oldExe, oldExec := executablePath, execSelf
	executablePath = func() (string, error) { return exe, nil }
	execSelf = func(path string) error { execed <- path; return nil }
	t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })

	a := newUpdateTestApp(t)
	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	select {
	case p := <-execed:
		if p != exe {
			t.Errorf("exec path %q, want %q", p, exe)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execSelf was never called")
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newBin) {
		t.Errorf("binary not replaced: %q", got)
	}
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary not executable: %v %v", fi, err)
	}
}

func TestUpdateApplyChecksumMismatchLeavesBinary(t *testing.T) {
	name := "scimux-" + goosArch()
	srv := fakeForgejo(t, "v9.9.9", map[string][]byte{
		name:         []byte("tampered payload"),
		"SHA256SUMS": []byte(strings.Repeat("0", 64) + "  " + name + "\n"),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe, oldExec := executablePath, execSelf
	executablePath = func() (string, error) { return exe, nil }
	execSelf = func(path string) error { t.Error("exec must not run on mismatch"); return nil }
	t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })

	a := newUpdateTestApp(t)
	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "checksum mismatch") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old binary" {
		t.Errorf("binary was replaced despite checksum mismatch: %q", got)
	}
	// The failed download must not litter temp files beside the binary.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scimux-update-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestUpdateApplyRefusesDevAndCurrent(t *testing.T) {
	srv := fakeForgejo(t, "v9.9.9", nil)
	a := newUpdateTestApp(t)
	for _, current := range []string{"dev", "v9.9.9"} {
		withUpdateSeams(t, srv.URL, current)
		rec := httptest.NewRecorder()
		a.handleUpdateApply(rec, updateReq("v9.9.9"))
		if rec.Code != 409 {
			t.Errorf("current=%s: status %d, want 409", current, rec.Code)
		}
	}
}

// The apply must refuse when no expected_tag is sent, and when the tag the user
// confirmed no longer matches the latest release (pinned, intentional update).
func TestUpdateApplyPinsExpectedTag(t *testing.T) {
	srv := fakeForgejo(t, "v9.9.9", nil)
	a := newUpdateTestApp(t)
	withUpdateSeams(t, srv.URL, "v1.0.0")

	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, httptest.NewRequest("POST", "/api/update", nil))
	if rec.Code != 400 {
		t.Errorf("missing expected_tag: status %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.8"))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "v9.9.9") {
		t.Errorf("stale expected_tag: status %d body %q, want 409", rec.Code, rec.Body)
	}
}

// updateReq builds a POST /api/update request pinned to tag.
func updateReq(tag string) *http.Request {
	return httptest.NewRequest("POST", "/api/update",
		strings.NewReader(`{"expected_tag":`+strconv.Quote(tag)+`}`))
}

func TestLicensesEmbedded(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLicenses(rec, httptest.NewRequest("GET", "/api/licenses", nil))
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for key, marker := range map[string]string{
		"scimux": "Mozilla Public License Version 2.0",
		"acp":    "Apache License",
		"go":     "The Go Authors",
	} {
		if !strings.Contains(got[key], marker) {
			t.Errorf("license %q does not contain %q", key, marker)
		}
	}
}
