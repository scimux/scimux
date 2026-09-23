package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/acp"
	"github.com/scimux/scimux/internal/acp/codex"
)

// fakeGitHub serves /releases/latest plus the named assets, mimicking the
// GitHub release API shape the updater consumes.
func fakeGitHub(t *testing.T, tag string, assets map[string][]byte) *httptest.Server {
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

// allowTestServerPolicy trusts only the given httptest server's host (HTTP or
// TLS). Production GitHub policy is never weakened globally.
func allowTestServerPolicy(t *testing.T, srv *httptest.Server) releaseDownloadPolicy {
	t.Helper()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return releaseDownloadPolicy{
		AllowURL: func(u *url.URL) error {
			if u == nil {
				return fmt.Errorf("nil URL")
			}
			if u.Scheme != base.Scheme {
				return fmt.Errorf("scheme %q", u.Scheme)
			}
			if u.User != nil {
				return fmt.Errorf("userinfo")
			}
			if u.Host != base.Host {
				return fmt.Errorf("host %q", u.Host)
			}
			return nil
		},
		MaxBytes: maxUpdateBinaryBytes,
		Client:   srv.Client(),
	}
}

func withUpdateSeams(t *testing.T, apiBase, ver string) {
	t.Helper()
	oldBase, oldVer := releaseAPIBase, version
	releaseAPIBase, version = apiBase, ver
	t.Cleanup(func() { releaseAPIBase, version = oldBase, oldVer })
}

// withTestAssetPolicy installs a narrow policy for the fake asset server and
// restores production policy via t.Cleanup.
func withTestAssetPolicy(t *testing.T, srv *httptest.Server) {
	t.Helper()
	restore := setReleasePolicyForTest(allowTestServerPolicy(t, srv))
	t.Cleanup(restore)
}

func TestUpdateCheck(t *testing.T) {
	srv := fakeGitHub(t, "v9.9.9", nil)
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
	srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
		name: newBin, "SHA256SUMS": []byte(sums),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")
	withTestAssetPolicy(t, srv)

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

func TestUpdateApplyUsesActiveWebVersion(t *testing.T) {
	srv := fakeGitHub(t, "v9.9.9", nil)
	withUpdateSeams(t, srv.URL, "v1.0.0")
	a := newUpdateTestApp(t)
	a.runtimeStatus = &muxerRuntimeStatus{version: "v9.9.9"}

	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already up to date") {
		t.Fatalf("active web version guard = %d %q, want 409 already up to date", rec.Code, rec.Body.String())
	}
}

func TestUpdateApplyPreparesThenCommitsWebOnly(t *testing.T) {
	newBin := []byte("new split web binary")
	name := "scimux-" + goosArch()
	srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
		name: newBin, "SHA256SUMS": []byte(shaSums(name, newBin)),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")
	withTestAssetPolicy(t, srv)

	exe := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe, oldExec := executablePath, execSelf
	executablePath = func() (string, error) { return exe, nil }
	execSelf = func(string) error { t.Error("split update must not exec the muxer"); return nil }
	t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })

	prepared := make(chan string, 1)
	committed := make(chan struct{}, 1)
	a := newUpdateTestApp(t)
	a.prepareWebUpdate = func(_ context.Context, path string) (webUpdateHandoff, error) {
		prepared <- path
		return webUpdateHandoff{commit: func() error { committed <- struct{}{}; return nil }}, nil
	}
	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	select {
	case path := <-prepared:
		if filepath.Dir(path) != filepath.Dir(exe) || !strings.HasPrefix(filepath.Base(path), ".scimux-update-") {
			t.Fatalf("prepared path %q is not the verified temporary executable beside %q", path, exe)
		}
	default:
		t.Fatal("success response was written before replacement preparation")
	}
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared web generation was not committed")
	}
	if got, _ := os.ReadFile(exe); string(got) != string(newBin) {
		t.Fatalf("installed binary = %q", got)
	}
}

func TestUpdateApplyPreparationFailureLeavesRunningWebAlone(t *testing.T) {
	newBin := []byte("verified but incompatible web binary")
	name := "scimux-" + goosArch()
	srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
		name: newBin, "SHA256SUMS": []byte(shaSums(name, newBin)),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")
	withTestAssetPolicy(t, srv)
	exe := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = oldExe })

	a := newUpdateTestApp(t)
	a.prepareWebUpdate = func(context.Context, string) (webUpdateHandoff, error) {
		return webUpdateHandoff{}, errors.New("major mismatch")
	}
	rec := httptest.NewRecorder()
	a.handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "major mismatch") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("failed preparation replaced installed binary with %q", got)
	}
	assertNoUpdateTemps(t, filepath.Dir(exe))
}

func TestUpdateApplyChecksumMismatchLeavesBinary(t *testing.T) {
	name := "scimux-" + goosArch()
	srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
		name:         []byte("tampered payload"),
		"SHA256SUMS": []byte(strings.Repeat("0", 64) + "  " + name + "\n"),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")
	withTestAssetPolicy(t, srv)

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
	srv := fakeGitHub(t, "v9.9.9", nil)
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
	srv := fakeGitHub(t, "v9.9.9", nil)
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
		"scimux":      "Mozilla Public License Version 2.0",
		"acp":         "Apache License",
		"go":          "The Go Authors",
		"qrcodegen":   "Project Nayuki",
		"fontawesome": "Fonticons, Inc.",
	} {
		if !strings.Contains(got[key], marker) {
			t.Errorf("license %q does not contain %q", key, marker)
		}
	}
}

// TestAboutSheetNamesEveryEmbeddedLicense joins the two halves that can
// drift apart silently: a notice embedded but never offered, and an About
// row whose button asks for a key the handler does not serve (the sheet
// shows an empty document, which reads as "no license" rather than as a
// bug). Vendoring web/js/qrcodegen.js is what made this reachable — it is
// the first third-party notice that is not a Go module, so the "regenerate
// from go list" habit in update.go would not have caught it.
func TestAboutSheetNamesEveryEmbeddedLicense(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLicenses(rec, httptest.NewRequest("GET", "/api/licenses", nil))
	var served map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-lic="([^"]+)"`).FindAllStringSubmatch(string(index), -1) {
		shown[m[1]] = true
	}
	for key := range served {
		if !shown[key] {
			t.Errorf("license %q is embedded and served but the About sheet has no row for it", key)
		}
	}
	for key := range shown {
		if _, ok := served[key]; !ok {
			t.Errorf("About sheet offers license %q but /api/licenses does not serve it; the sheet would open empty", key)
		}
	}
}

func TestAboutSheetDisclosesAIEnabledEngineeringUnderAuthor(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	author := strings.Index(html, "Christian Berger")
	disclosure := strings.Index(html, "AI-enabled Software Engineering")
	source := strings.Index(html, `<span>Source</span>`)
	if author < 0 || disclosure < 0 || source < 0 || !(author < disclosure && disclosure < source) {
		t.Fatalf("About disclosure must appear directly after the author and before Source")
	}
	for _, agent := range []string{"Claude", "Codex", "Grok"} {
		if !strings.Contains(html[disclosure:source], agent) {
			t.Errorf("About AI-enabled engineering disclosure does not name %s", agent)
		}
	}
}

// --- P3: bounded download, URL policy, failure cleanup ---

func shaSums(name string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
}

func assertNoUpdateTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scimux-update-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func assertBinaryUnchanged(t *testing.T, exe, want string) {
	t.Helper()
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("installed binary changed: got %q want %q", got, want)
	}
}

// Positive Content-Length greater than the cap is rejected before temp-file
// creation or executable mutation.
func TestDownloadRejectsOversizedContentLength(t *testing.T) {
	const capBytes int64 = 64
	var temps int32
	oldCreate := updateCreateTemp
	updateCreateTemp = func(dir, pattern string) (*os.File, error) {
		atomic.AddInt32(&temps, 1)
		return oldCreate(dir, pattern)
	}
	t.Cleanup(func() { updateCreateTemp = oldCreate })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		// Body should not matter; rejection is before read/temp.
		w.Write(bytesN(1000, 'x'))
	}))
	t.Cleanup(srv.Close)

	policy := allowTestServerPolicy(t, srv)
	policy.MaxBytes = capBytes

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "scimux-bin"
	_, err := downloadVerifiedWith(context.Background(), policy, srv.URL, shaSums(name, bytesN(10, 'a')), name, exe)
	if err == nil || !strings.Contains(err.Error(), "Content-Length") {
		t.Fatalf("err = %v, want Content-Length rejection", err)
	}
	if atomic.LoadInt32(&temps) != 0 {
		t.Errorf("CreateTemp called %d times; want 0 before oversize CL reject", temps)
	}
	assertBinaryUnchanged(t, exe, "old")
	assertNoUpdateTemps(t, dir)
}

// Chunked/unknown-length body exceeding the cap is detected by the first extra
// byte and rejected — not accepted merely because the copy truncated at cap.
func TestDownloadRejectsOversizedChunkedBody(t *testing.T) {
	const capBytes int64 = 32
	payload := bytesN(int(capBytes)+8, 'z')
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length → chunked/unknown.
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	policy := allowTestServerPolicy(t, srv)
	policy.MaxBytes = capBytes

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Matching checksum of a truncated-at-cap slice must still not succeed.
	truncated := payload[:capBytes]
	name := "scimux-bin"
	_, err := downloadVerifiedWith(context.Background(), policy, srv.URL, shaSums(name, truncated), name, exe)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want body exceeds limit", err)
	}
	assertBinaryUnchanged(t, exe, "old")
	assertNoUpdateTemps(t, dir)
}

// No more than the configured cap reaches the temp writer (lightweight small
// cap). Observes the transient write count through updateTempWriter — cleanup
// alone is not enough coverage (removing LimitReader could still leave no temp).
func TestDownloadWritesAtMostCapBytes(t *testing.T) {
	const capBytes int64 = 40
	var maxWritten int64
	oldWriter := updateTempWriter
	updateTempWriter = func(f *os.File) io.Writer {
		return &countingTempWriter{w: f, n: &maxWritten}
	}
	t.Cleanup(func() { updateTempWriter = oldWriter })

	// Chunked/unknown length so we exercise the write path (a large
	// Content-Length is rejected before CreateTemp and would not write).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		// Force chunked by flushing progressive writes without CL.
		flusher, _ := w.(http.Flusher)
		chunk := bytesN(int(capBytes)+20, 'q')
		for i := 0; i < len(chunk); i += 8 {
			end := i + 8
			if end > len(chunk) {
				end = len(chunk)
			}
			w.Write(chunk[i:end])
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	policy := allowTestServerPolicy(t, srv)
	policy.MaxBytes = capBytes

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	name := "bin"
	// Use a checksum that would match if truncation were accepted.
	_, err := downloadVerifiedWith(context.Background(), policy, srv.URL,
		shaSums(name, bytesN(int(capBytes), 'q')), name, exe)
	if err == nil {
		t.Fatal("expected oversize rejection")
	}
	if maxWritten > capBytes {
		t.Errorf("wrote %d bytes to temp, cap is %d", maxWritten, capBytes)
	}
	if maxWritten != capBytes {
		t.Errorf("wrote %d bytes, want exactly cap %d before overflow probe", maxWritten, capBytes)
	}
	assertNoUpdateTemps(t, dir)
	assertBinaryUnchanged(t, exe, "old")
}

// countingTempWriter records cumulative bytes written through the temp-writer seam.
type countingTempWriter struct {
	w io.Writer
	n *int64
}

func (c *countingTempWriter) Write(p []byte) (int, error) {
	nw, err := c.w.Write(p)
	*c.n += int64(nw)
	return nw, err
}

// failAfterTempWriter writes up to after bytes successfully, then fails.
type failAfterTempWriter struct {
	w     io.Writer
	after int
	wrote int
}

func (f *failAfterTempWriter) Write(p []byte) (int, error) {
	if f.wrote >= f.after {
		return 0, fmt.Errorf("simulated write failure")
	}
	remain := f.after - f.wrote
	if len(p) > remain {
		p = p[:remain]
	}
	nw, err := f.w.Write(p)
	f.wrote += nw
	if err != nil {
		return nw, err
	}
	if f.wrote >= f.after {
		return nw, fmt.Errorf("simulated write failure after %d bytes", f.wrote)
	}
	return nw, nil
}

// zeroThenExtraReader returns the capped payload, then a (0,nil) short-read,
// then one extra byte — legal io.Reader behavior that a single Read treating
// (0,nil) as EOF would miss.
type zeroThenExtraReader struct {
	payload []byte
	phase   int // 0=payload, 1=(0,nil), 2=extra, 3=EOF
}

func (r *zeroThenExtraReader) Read(p []byte) (int, error) {
	switch r.phase {
	case 0:
		if len(r.payload) == 0 {
			r.phase = 1
			return 0, nil
		}
		n := copy(p, r.payload)
		r.payload = r.payload[n:]
		if len(r.payload) == 0 {
			r.phase = 1
		}
		return n, nil
	case 1:
		r.phase = 2
		return 0, nil
	case 2:
		r.phase = 3
		if len(p) == 0 {
			return 0, nil
		}
		p[0] = 'X'
		return 1, nil
	default:
		return 0, io.EOF
	}
}

// Exact-cap payload with matching checksum succeeds.
func TestDownloadExactCapSucceeds(t *testing.T) {
	const capBytes int64 = 48
	payload := bytesN(int(capBytes), 'e')
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	policy := allowTestServerPolicy(t, srv)
	policy.MaxBytes = capBytes

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	name := "bin"
	tmp, err := downloadVerifiedWith(context.Background(), policy, srv.URL, shaSums(name, payload), name, exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(tmp) })
	got, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != int(capBytes) || string(got) != string(payload) {
		t.Errorf("tmp len=%d content mismatch", len(got))
	}
	assertBinaryUnchanged(t, exe, "old") // downloadVerified does not install
}

// A body that returns exactly cap bytes, then (0,nil), then one extra byte
// must be rejected — (0,nil) is not EOF.
func TestDownloadRejectsZeroNilThenExtraByte(t *testing.T) {
	const capBytes int64 = 16
	payload := bytesN(int(capBytes), 'z')
	// Serve through a custom RoundTrip is heavy; instead exercise the overflow
	// probe via an httptest body is hard to inject (0,nil). Use a local helper
	// path: wrap download by installing a transport... Simpler: call the
	// probe contract through downloadVerifiedWith against a server that uses
	// chunked writes with Flush. Go's HTTP server may coalesce, so use an
	// io.Pipe body via httptest and a custom handler that hijacks...
	// Narrow approach: unit-test the reader against downloadVerifiedWith by
	// swapping http via a policy Client with a custom Transport.
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(&zeroThenExtraReader{payload: append([]byte(nil), payload...)}),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})
	policy := releaseDownloadPolicy{
		AllowURL: func(u *url.URL) error { return nil },
		MaxBytes: capBytes,
		Client:   &http.Client{Transport: rt},
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	name := "bin"
	// Checksum of the capped prefix alone must not make this succeed.
	_, err := downloadVerifiedWith(context.Background(), policy, "https://example.test/bin",
		shaSums(name, payload), name, exe)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want body exceeds limit", err)
	}
	assertBinaryUnchanged(t, exe, "old")
	assertNoUpdateTemps(t, dir)
}

// Injected client with a permissive CheckRedirect must still reject a
// disallowed redirect target; the sink must never be hit.
func TestDownloadInjectedClientCannotBypassRedirectPolicy(t *testing.T) {
	var sinkHits int32
	httpSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sinkHits, 1)
		w.Write([]byte("should-not-download"))
	}))
	t.Cleanup(httpSink.Close)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpSink.URL+"/bin", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	originURL, _ := url.Parse(origin.URL)

	// Client whose callback would allow any redirect (returns nil always).
	permissive := origin.Client()
	permissive.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return nil // would follow HTTP sink without security wrap
	}

	policy := releaseDownloadPolicy{
		AllowURL: func(u *url.URL) error {
			if u.Scheme != "https" {
				return fmt.Errorf("not https")
			}
			if u.Host != originURL.Host {
				return fmt.Errorf("untrusted host %q", u.Host)
			}
			return nil
		},
		MaxBytes: 64,
		Client:   permissive,
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	_, err := downloadVerifiedWith(context.Background(), policy, origin.URL+"/start",
		shaSums("bin", []byte("x")), "bin", exe)
	if err == nil {
		t.Fatal("want redirect rejection despite permissive client CheckRedirect")
	}
	if atomic.LoadInt32(&sinkHits) != 0 {
		t.Errorf("sink hits = %d, want 0", sinkHits)
	}
	assertBinaryUnchanged(t, exe, "old")
	assertNoUpdateTemps(t, dir)
}

// Smaller verified payload still installs via the apply handler.
func TestUpdateApplySmallVerifiedPayload(t *testing.T) {
	newBin := []byte("tiny-ok")
	name := "scimux-" + goosArch()
	srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
		name: newBin, "SHA256SUMS": []byte(shaSums(name, newBin)),
	})
	withUpdateSeams(t, srv.URL, "v1.0.0")
	withTestAssetPolicy(t, srv)

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	execed := make(chan string, 1)
	oldExe, oldExec := executablePath, execSelf
	executablePath = func() (string, error) { return exe, nil }
	execSelf = func(path string) error { execed <- path; return nil }
	t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })

	rec := httptest.NewRecorder()
	newUpdateTestApp(t).handleUpdateApply(rec, updateReq("v9.9.9"))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	select {
	case <-execed:
	case <-time.After(5 * time.Second):
		t.Fatal("execSelf not called")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(newBin) {
		t.Errorf("got %q", got)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o111 == 0 {
		t.Error("not executable")
	}
	assertNoUpdateTemps(t, dir)
}

// Failure matrix: preserve installed binary, no execSelf, no leftover temps.
func TestUpdateFailureCleanupMatrix(t *testing.T) {
	// Shared: old binary must remain "old binary".
	type caseFn struct {
		name string
		run  func(t *testing.T, dir, exe string)
	}
	cases := []caseFn{
		{
			name: "oversized Content-Length",
			run: func(t *testing.T, dir, exe string) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Length", "999")
					w.Write(bytesN(999, 'x'))
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				p.MaxBytes = 16
				name := "bin"
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL, shaSums(name, []byte("x")), name, exe)
				if err == nil {
					t.Fatal("want error")
				}
			},
		},
		{
			name: "oversized chunked",
			run: func(t *testing.T, dir, exe string) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(bytesN(100, 'y'))
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				p.MaxBytes = 16
				name := "bin"
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL, shaSums(name, bytesN(16, 'y')), name, exe)
				if err == nil {
					t.Fatal("want error")
				}
			},
		},
		{
			name: "checksum mismatch",
			run: func(t *testing.T, dir, exe string) {
				body := []byte("payload")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(body)
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				name := "bin"
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL,
					strings.Repeat("0", 64)+"  "+name+"\n", name, exe)
				if err == nil || !strings.Contains(err.Error(), "checksum") {
					t.Fatalf("err=%v", err)
				}
			},
		},
		{
			name: "invalid initial URL",
			run: func(t *testing.T, dir, exe string) {
				p := productionReleasePolicy()
				p.MaxBytes = 64
				_, err := downloadVerifiedWith(context.Background(), p,
					"http://github.com/assets/bin", shaSums("bin", []byte("x")), "bin", exe)
				if err == nil {
					t.Fatal("want URL rejection")
				}
			},
		},
		{
			name: "redirect to HTTP",
			run: func(t *testing.T, dir, exe string) {
				// TLS origin redirects to plain HTTP — production policy rejects.
				httpSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte("should-not-download"))
				}))
				t.Cleanup(httpSink.Close)
				tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, httpSink.URL+"/bin", http.StatusFound)
				}))
				t.Cleanup(tlsSrv.Close)
				p := releaseDownloadPolicy{
					AllowURL: func(u *url.URL) error {
						// Mimic production: HTTPS only.
						if u.Scheme != "https" {
							return fmt.Errorf("not https")
						}
						return nil
					},
					MaxBytes: 64,
					Client:   tlsSrv.Client(),
				}
				_, err := downloadVerifiedWith(context.Background(), p, tlsSrv.URL+"/bin",
					shaSums("bin", []byte("x")), "bin", exe)
				if err == nil {
					t.Fatal("want redirect rejection")
				}
			},
		},
		{
			name: "redirect untrusted host",
			run: func(t *testing.T, dir, exe string) {
				evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte("evil"))
				}))
				t.Cleanup(evil.Close)
				good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, evil.URL+"/bin", http.StatusFound)
				}))
				t.Cleanup(good.Close)
				goodURL, _ := url.Parse(good.URL)
				p := releaseDownloadPolicy{
					AllowURL: func(u *url.URL) error {
						if u.Scheme != "https" || u.Host != goodURL.Host {
							return fmt.Errorf("untrusted %s", u.Host)
						}
						return nil
					},
					MaxBytes: 64,
					Client:   good.Client(),
				}
				_, err := downloadVerifiedWith(context.Background(), p, good.URL+"/bin",
					shaSums("bin", []byte("x")), "bin", exe)
				if err == nil {
					t.Fatal("want untrusted redirect rejection")
				}
			},
		},
		{
			name: "redirect hostname-suffix trick",
			run: func(t *testing.T, dir, exe string) {
				p := productionReleasePolicy()
				// Direct validation of the hostile URL (suffix host).
				err := p.validateURL("https://github.com.evil.example/assets/bin")
				if err == nil {
					t.Fatal("suffix host must be rejected")
				}
			},
		},
		{
			name: "temp create failure",
			run: func(t *testing.T, dir, exe string) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte("data-for-create-fail"))
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				oldCreate := updateCreateTemp
				updateCreateTemp = func(d, pattern string) (*os.File, error) {
					return nil, fmt.Errorf("simulated create failure")
				}
				t.Cleanup(func() { updateCreateTemp = oldCreate })
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL,
					shaSums("bin", []byte("data-for-create-fail")), "bin", exe)
				if err == nil || !strings.Contains(err.Error(), "create") {
					t.Fatalf("err=%v, want create failure", err)
				}
			},
		},
		{
			name: "temp write failure",
			run: func(t *testing.T, dir, exe string) {
				body := []byte("partial-write-then-fail-body-long-enough")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(body)
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				oldWriter := updateTempWriter
				updateTempWriter = func(f *os.File) io.Writer {
					return &failAfterTempWriter{w: f, after: 8}
				}
				t.Cleanup(func() { updateTempWriter = oldWriter })
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL, shaSums("bin", body), "bin", exe)
				if err == nil || !strings.Contains(err.Error(), "write failure") {
					t.Fatalf("err=%v, want write failure after partial write", err)
				}
			},
		},
		{
			name: "temp close failure",
			run: func(t *testing.T, dir, exe string) {
				body := []byte("close-fail-body")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Write(body)
				}))
				t.Cleanup(srv.Close)
				p := allowTestServerPolicy(t, srv)
				oldClose := updateCloseTemp
				updateCloseTemp = func(f *os.File) error {
					f.Close() // real close so remove works
					return fmt.Errorf("simulated close failure")
				}
				t.Cleanup(func() { updateCloseTemp = oldClose })
				_, err := downloadVerifiedWith(context.Background(), p, srv.URL, shaSums("bin", body), "bin", exe)
				if err == nil || !strings.Contains(err.Error(), "close") {
					t.Fatalf("err=%v, want close failure", err)
				}
			},
		},
		{
			name: "chmod failure",
			run: func(t *testing.T, dir, exe string) {
				newBin := []byte("chmod-fail-bin")
				name := "scimux-" + goosArch()
				srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
					name: newBin, "SHA256SUMS": []byte(shaSums(name, newBin)),
				})
				withUpdateSeams(t, srv.URL, "v1.0.0")
				withTestAssetPolicy(t, srv)
				oldExe, oldExec := executablePath, execSelf
				executablePath = func() (string, error) { return exe, nil }
				execSelf = func(path string) error { t.Error("execSelf on chmod fail"); return nil }
				t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })
				oldChmod := updateChmod
				chmodCalls := 0
				updateChmod = func(string, os.FileMode) error {
					chmodCalls++
					return fmt.Errorf("simulated chmod failure")
				}
				t.Cleanup(func() { updateChmod = oldChmod })
				rec := httptest.NewRecorder()
				newUpdateTestApp(t).handleUpdateApply(rec, updateReq("v9.9.9"))
				if chmodCalls == 0 {
					t.Fatalf("updateChmod never called; status %d body %s", rec.Code, rec.Body)
				}
				if rec.Code != 500 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			},
		},
		{
			name: "rename failure",
			run: func(t *testing.T, dir, exe string) {
				newBin := []byte("rename-fail-bin")
				name := "scimux-" + goosArch()
				srv := fakeGitHub(t, "v9.9.9", map[string][]byte{
					name: newBin, "SHA256SUMS": []byte(shaSums(name, newBin)),
				})
				withUpdateSeams(t, srv.URL, "v1.0.0")
				withTestAssetPolicy(t, srv)
				oldExe, oldExec := executablePath, execSelf
				executablePath = func() (string, error) { return exe, nil }
				execSelf = func(path string) error { t.Error("execSelf on rename fail"); return nil }
				t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })
				oldRename := updateRename
				updateRename = func(string, string) error { return fmt.Errorf("simulated rename failure") }
				t.Cleanup(func() { updateRename = oldRename })
				aborted, committed := false, false
				a := newUpdateTestApp(t)
				a.prepareWebUpdate = func(context.Context, string) (webUpdateHandoff, error) {
					return webUpdateHandoff{
						commit: func() error { committed = true; return nil },
						abort:  func() { aborted = true },
					}, nil
				}
				rec := httptest.NewRecorder()
				a.handleUpdateApply(rec, updateReq("v9.9.9"))
				if rec.Code != 500 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
				if !aborted || committed {
					t.Fatalf("failed install handoff: aborted=%v committed=%v", aborted, committed)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "scimux")
			const old = "old binary"
			if err := os.WriteFile(exe, []byte(old), 0o755); err != nil {
				t.Fatal(err)
			}
			execCalled := false
			oldExec := execSelf
			execSelf = func(string) error { execCalled = true; return nil }
			t.Cleanup(func() { execSelf = oldExec })

			tc.run(t, dir, exe)

			assertBinaryUnchanged(t, exe, old)
			if execCalled {
				t.Error("execSelf must not run on failure")
			}
			assertNoUpdateTemps(t, dir)
		})
	}
}

func TestProductionAssetURLPolicy(t *testing.T) {
	p := productionReleasePolicy()
	// Accepts canonical GitHub HTTPS asset URL (no port / :443).
	for _, ok := range []string{
		"https://github.com/scimux/scimux/releases/download/v1.0.0/scimux-linux-amd64",
		"https://github.com:443/scimux/scimux/releases/download/v1.0.0/SHA256SUMS",
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/example",
		"https://release-assets.githubusercontent.com:443/github-production-release-asset/1/example",
	} {
		if err := p.validateURL(ok); err != nil {
			t.Errorf("accept %s: %v", ok, err)
		}
	}
	// Rejects.
	for _, bad := range []string{
		"http://github.com/x",
		"https://evil.example/x",
		"https://github.com.evil.example/x",
		"https://release-assets.githubusercontent.com.evil.example/x",
		"https://user@github.com/x",
		"https://user@release-assets.githubusercontent.com/x",
		"https://github.com:8443/x",
		"https://user:pass@github.com/x",
	} {
		if err := p.validateURL(bad); err == nil {
			t.Errorf("reject %s: got nil", bad)
		}
	}
}

// Allowed same-policy redirect remains bounded and checksum-verified.
func TestDownloadAllowedRedirectBoundedAndVerified(t *testing.T) {
	const capBytes int64 = 64
	payload := []byte("redirected-ok-payload")
	var finalHits int32
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&finalHits, 1)
		w.Write(payload)
	})
	p := allowTestServerPolicy(t, srv)
	p.MaxBytes = capBytes

	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	name := "bin"
	tmp, err := downloadVerifiedWith(context.Background(), p, srv.URL+"/start", shaSums(name, payload), name, exe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(tmp) })
	got, _ := os.ReadFile(tmp)
	if string(got) != string(payload) {
		t.Errorf("got %q", got)
	}
	if atomic.LoadInt32(&finalHits) != 1 {
		t.Errorf("final hits %d", finalHits)
	}
	assertBinaryUnchanged(t, exe, "old")
}

// Private TLS server with injected client — does not mutate DefaultClient.
func TestDownloadUsesInjectedTLSClient(t *testing.T) {
	payload := []byte("tls-payload")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	// Without injected client, DefaultClient would fail TLS verify.
	p := allowTestServerPolicy(t, srv)
	dir := t.TempDir()
	exe := filepath.Join(dir, "scimux")
	os.WriteFile(exe, []byte("old"), 0o755)
	name := "bin"
	tmp, err := downloadVerifiedWith(context.Background(), p, srv.URL, shaSums(name, payload), name, exe)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(tmp)
}

func bytesN(n int, b byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
