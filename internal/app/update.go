package app

// Manual update check and self-update. The binary never touches the network
// on its own: both the check and the download run only when the user taps
// the corresponding control in the burger menu ("no cloud" holds by
// construction). The update is notify-and-confirm, never silent: the UI asks
// before /api/update is called, and the handler verifies the downloaded
// binary against the release's SHA256SUMS before the atomic rename + re-exec.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"codeberg.org/chrberger/scimux/legal"
)

// The binary carries its own legal notices (MPL-2.0 §3.2 asks executable
// distributions to say where source and license live; the About sheet does).
//
// The set below is every module linked into the binary, not every module in
// go.mod: pion is one direct require that pulls a family of transitive ones,
// and a notice that named only the require would under-report what ships.
// Regenerate from `go list -deps ./cmd/scimux` if the transport's dependency
// set changes.
var (
	ownLicense     = mustEmbeddedLicense("scimux.LICENSE")
	acpLicense     = mustEmbeddedLicense("acp-go-sdk.LICENSE")
	goLicense      = mustEmbeddedLicense("go.LICENSE")
	pionLicense    = mustEmbeddedLicense("pion.LICENSE")
	golangXLicense = mustEmbeddedLicense("golang-x.LICENSE")
	uuidLicense    = mustEmbeddedLicense("uuid.LICENSE")
	anetLicense    = mustEmbeddedLicense("anet.LICENSE")
	// Not a Go module: web/js/qrcodegen.js is vendored browser source,
	// embedded and served like the rest of the web tree. `go list -deps`
	// cannot see it, so it is listed here by hand.
	qrcodegenLicense = mustEmbeddedLicense("qrcodegen.LICENSE")
)

func mustEmbeddedLicense(name string) string {
	b, err := legal.Files.ReadFile(name)
	if err != nil {
		panic("embedded " + name + ": " + err.Error())
	}
	return string(b)
}

func handleLicenses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"scimux":    ownLicense,
		"acp":       acpLicense,
		"go":        goLicense,
		"pion":      pionLicense,
		"golangx":   golangXLicense,
		"uuid":      uuidLicense,
		"anet":      anetLicense,
		"qrcodegen": qrcodegenLicense,
	})
}

// releaseAPIBase is a var only so tests can point it at a fake Forgejo.
// The release *metadata* API host is not the asset download boundary: binary
// and SHA256SUMS downloads use releaseDownloadPolicy (production: HTTPS
// codeberg.org only). Tests may point this at httptest without weakening
// production asset policy.
var releaseAPIBase = "https://codeberg.org/api/v1/repos/chrberger/scimux"

// maxUpdateBinaryBytes is the production maximum size of a self-update binary
// download (256 MiB). Comfortably above current release artifacts; finite so
// a hostile or misconfigured release cannot fill the disk before the checksum
// runs. Metadata/SHA256SUMS text retains its own smaller 1 MiB cap in fetchText.
const maxUpdateBinaryBytes int64 = 256 << 20

// releaseAssetHost is the only hostname production will download update
// binaries and SHA256SUMS from. Exact match — no suffix tricks, no userinfo,
// HTTPS only, port empty or 443.
const releaseAssetHost = "codeberg.org"

// releaseDownloadPolicy bounds and authorizes self-update asset downloads.
// Production uses allowCodebergAssetURL + maxUpdateBinaryBytes. Tests inject a
// narrow policy (and optional client for private TLS) via setReleasePolicyForTest;
// production policy is never globally rewritten to allow arbitrary HTTP.
type releaseDownloadPolicy struct {
	// AllowURL validates an asset URL (initial and every redirect). Required.
	AllowURL func(*url.URL) error
	// MaxBytes caps the binary payload written to the temp file. Zero means
	// maxUpdateBinaryBytes. SHA256SUMS text uses fetchText's fixed 1 MiB cap.
	MaxBytes int64
	// Client, if non-nil, is used for asset GETs (tests: httptest TLS client).
	// When nil, a client with CheckRedirect tied to AllowURL is built.
	// http.DefaultClient is never mutated.
	Client *http.Client
}

func (p releaseDownloadPolicy) maxBytes() int64 {
	if p.MaxBytes > 0 {
		return p.MaxBytes
	}
	return maxUpdateBinaryBytes
}

func (p releaseDownloadPolicy) validateURL(raw string) error {
	if p.AllowURL == nil {
		return errors.New("update: missing asset URL policy")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("update: bad asset URL: %w", err)
	}
	if err := p.AllowURL(u); err != nil {
		return fmt.Errorf("update: asset URL rejected: %w", err)
	}
	return nil
}

func (p releaseDownloadPolicy) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("update: too many redirects")
	}
	if p.AllowURL == nil {
		return errors.New("update: missing asset URL policy")
	}
	// Validate before any redirected response body is read.
	if err := p.AllowURL(req.URL); err != nil {
		return fmt.Errorf("update: redirect rejected: %w", err)
	}
	return nil
}

func (p releaseDownloadPolicy) httpClient() *http.Client {
	// Always enforce p.checkRedirect. An injected client's CheckRedirect must
	// not bypass AllowURL: run the security policy first, then any caller
	// callback. Shallow-copy so we never mutate the caller's client
	// (e.g. httptest.Server.Client()).
	wrap := func(caller func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
		return func(req *http.Request, via []*http.Request) error {
			if err := p.checkRedirect(req, via); err != nil {
				return err
			}
			if caller != nil {
				return caller(req, via)
			}
			return nil
		}
	}
	if p.Client != nil {
		c := *p.Client
		c.CheckRedirect = wrap(c.CheckRedirect)
		return &c
	}
	return &http.Client{CheckRedirect: wrap(nil)}
}

// allowCodebergAssetURL is the fixed production asset URL policy: HTTPS,
// hostname exactly codeberg.org, no userinfo, port empty or 443.
func allowCodebergAssetURL(u *url.URL) error {
	if u == nil {
		return errors.New("nil URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme %q not https", u.Scheme)
	}
	if u.User != nil {
		return errors.New("userinfo not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host != releaseAssetHost {
		return fmt.Errorf("host %q not %s", host, releaseAssetHost)
	}
	switch u.Port() {
	case "", "443":
		return nil
	default:
		return fmt.Errorf("port %q not allowed", u.Port())
	}
}

func productionReleasePolicy() releaseDownloadPolicy {
	return releaseDownloadPolicy{
		AllowURL: allowCodebergAssetURL,
		MaxBytes: maxUpdateBinaryBytes,
	}
}

// activeReleasePolicy is the policy handleUpdateApply uses for asset downloads.
// Tests replace it with setReleasePolicyForTest; production stays Codeberg-only.
var activeReleasePolicy = productionReleasePolicy()

// setReleasePolicyForTest replaces the active download policy; restore via the
// returned func (register with t.Cleanup). For tests only.
func setReleasePolicyForTest(p releaseDownloadPolicy) (restore func()) {
	prev := activeReleasePolicy
	activeReleasePolicy = p
	return func() { activeReleasePolicy = prev }
}

// Filesystem seams for failure-path tests. Restored with t.Cleanup.
var (
	updateCreateTemp = os.CreateTemp
	updateCloseTemp  = func(f *os.File) error { return f.Close() }
	// updateTempWriter wraps the temp file for the payload copy only (not
	// close/remove). Tests instrument write counts or inject write errors.
	updateTempWriter = func(f *os.File) io.Writer { return f }
	updateChmod      = os.Chmod
	updateRename     = os.Rename
)

type release struct {
	Tag    string `json:"tag_name"`
	URL    string `json:"html_url"`
	Notes  string `json:"body"`
	Assets []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (rel *release) asset(name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.DownloadURL
		}
	}
	return ""
}

func fetchLatestRelease(ctx context.Context) (*release, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", releaseAPIBase+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	// Metadata API may be httptest in tests; asset policy is separate.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("release API: %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Tag == "" {
		return nil, fmt.Errorf("release API: response has no tag_name")
	}
	return &rel, nil
}

func handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := fetchLatestRelease(r.Context())
	if err != nil {
		http.Error(w, "update check: "+err.Error(), 502)
		return
	}
	writeJSON(w, map[string]any{
		"current": version,
		"latest":  rel.Tag,
		"url":     rel.URL,
		"notes":   rel.Notes,
		// "dev" builds have no release identity to compare against; the
		// check still reports the latest tag, but never offers an update.
		"available": version != "dev" && rel.Tag != version,
	})
}

// downloadVerified streams url into a temp file next to dest under the active
// release policy. Production path: productionReleasePolicy().
func downloadVerified(ctx context.Context, rawURL, sums, name, dest string) (string, error) {
	return downloadVerifiedWith(ctx, activeReleasePolicy, rawURL, sums, name, dest)
}

// downloadVerifiedWith streams rawURL into a same-directory temp file next to
// dest and returns the temp path only if every check succeeds:
//   - initial URL and redirects pass policy.AllowURL
//   - positive Content-Length > max is rejected before temp creation
//   - at most max payload bytes are written; one extra byte probes overflow
//   - SHA-256 matches the sums entry for name
//
// Any failure after temp creation closes and removes the temp file. The
// installed executable at dest is never truncated, deleted, or renamed here.
func downloadVerifiedWith(ctx context.Context, policy releaseDownloadPolicy, rawURL, sums, name, dest string) (string, error) {
	want := ""
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("SHA256SUMS has no entry for %q", name)
	}
	if err := policy.validateURL(rawURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := policy.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", name, resp.Status)
	}
	max := policy.maxBytes()
	// Reject oversized Content-Length before creating a temp file.
	if resp.ContentLength > max {
		return "", fmt.Errorf("download %s: Content-Length %d exceeds %d byte limit", name, resp.ContentLength, max)
	}
	f, err := updateCreateTemp(filepath.Dir(dest), ".scimux-update-*")
	if err != nil {
		return "", err
	}
	tmpName := f.Name()
	cleanup := func() {
		updateCloseTemp(f)
		os.Remove(tmpName)
	}
	h := sha256.New()
	// Write at most max bytes to the temp file and hash. The temp-writer seam
	// wraps only the file side so tests can count writes or inject failures
	// without altering the hash path.
	n, err := io.Copy(io.MultiWriter(updateTempWriter(f), h), io.LimitReader(resp.Body, max))
	if err != nil {
		cleanup()
		return "", err
	}
	// Probe one extra byte: truncation at max must not be treated as success
	// without proving the body ended. io.ReadFull continues past (0, nil)
	// short-reads until it gets one byte, EOF, or a real error — a single
	// Read treating (0, nil) as EOF-equivalent is insufficient. Consumes at
	// most max+1 from the response; the extra byte is never written to disk.
	if n == max {
		var extra [1]byte
		_, rerr := io.ReadFull(resp.Body, extra[:])
		switch {
		case rerr == nil:
			cleanup()
			return "", fmt.Errorf("download %s: body exceeds %d byte limit", name, max)
		case rerr == io.EOF || rerr == io.ErrUnexpectedEOF:
			// Body ended with no extra byte (EOF) or a short final read
			// that still did not yield a full extra byte.
		default:
			cleanup()
			return "", rerr
		}
	}
	if err := updateCloseTemp(f); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		os.Remove(tmpName)
		return "", fmt.Errorf("checksum mismatch for %s", name)
	}
	return tmpName, nil
}

// fetchText downloads a small text asset (SHA256SUMS) under the active policy.
// Cap is 1 MiB — independent of the binary size limit.
func fetchText(ctx context.Context, rawURL string) (string, error) {
	return fetchTextWith(ctx, activeReleasePolicy, rawURL)
}

func fetchTextWith(ctx context.Context, policy releaseDownloadPolicy, rawURL string) (string, error) {
	if err := policy.validateURL(rawURL); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := policy.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}

// Test seams: the apply handler must never overwrite the `go test` binary or
// exec anything during tests.
var (
	executablePath = os.Executable
	execSelf       = func(path string) error {
		return syscall.Exec(path, os.Args, os.Environ())
	}
	updateMu sync.Mutex
)

// handleUpdateApply downloads the matching release asset, verifies it against
// the release's SHA256SUMS, renames it over the running binary, and re-execs.
// Two invariants: nothing replaces the binary before the checksum matched,
// and the subprocess-owning managers shut down before exec — a plain exec
// would orphan the ACP/codex children that the signal handler normally kills
// (tmux sessions survive on purpose, exactly as on a normal restart).
func (a *app) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	// The apply is pinned to the exact tag the check displayed. Without this the
	// handler re-fetches "latest" and installs whatever it finds — so a release
	// cut between check and apply would silently install a version the operator
	// never saw, and it makes the endpoint a blind-fire CSRF target (though the
	// unsafe-method guard now also stands in front of it).
	var body struct {
		ExpectedTag string `json:"expected_tag"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		http.Error(w, "bad request: expected {\"expected_tag\":\"vX.Y.Z\"}", 400)
		return
	}
	if body.ExpectedTag == "" {
		http.Error(w, "update: expected_tag is required (the tag the check displayed)", 400)
		return
	}
	if !updateMu.TryLock() {
		http.Error(w, "update already in progress", 409)
		return
	}
	defer updateMu.Unlock()
	rel, err := fetchLatestRelease(r.Context())
	if err != nil {
		http.Error(w, "update: "+err.Error(), 502)
		return
	}
	if version == "dev" || rel.Tag == version {
		http.Error(w, "already up to date", 409)
		return
	}
	if rel.Tag != body.ExpectedTag {
		http.Error(w, fmt.Sprintf("update: latest is now %s, not the %s you confirmed — re-check before updating", rel.Tag, body.ExpectedTag), 409)
		return
	}
	exe, err := executablePath()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		http.Error(w, "update: locate binary: "+err.Error(), 500)
		return
	}
	assetName := "scimux-" + runtime.GOOS + "-" + runtime.GOARCH
	binURL, sumsURL := rel.asset(assetName), rel.asset("SHA256SUMS")
	if binURL == "" || sumsURL == "" {
		http.Error(w, fmt.Sprintf("update: release %s has no %s + SHA256SUMS assets", rel.Tag, assetName), 502)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	sums, err := fetchText(ctx, sumsURL)
	if err != nil {
		http.Error(w, "update: "+err.Error(), 502)
		return
	}
	tmp, err := downloadVerified(ctx, binURL, sums, assetName, exe)
	if err != nil {
		http.Error(w, "update: "+err.Error(), 502)
		return
	}
	// Chmod and rename use distinct err scopes: a single `if err := chmod;
	// err == nil { err = rename }` would discard both failures into a
	// block-scoped err while the outer err (from download) stayed nil and
	// the handler would report success.
	if err := updateChmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		http.Error(w, "update: install: "+err.Error(), 500)
		return
	}
	if err := updateRename(tmp, exe); err != nil {
		os.Remove(tmp)
		http.Error(w, "update: install: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "version": rel.Tag})
	go func() {
		// Let the response reach the browser before the process is replaced.
		time.Sleep(400 * time.Millisecond)
		a.acp.Shutdown()
		a.codex.Shutdown()
		if err := execSelf(exe); err != nil {
			// The new binary is installed but exec failed; the children are
			// already gone, so a half-alive server would mislead — exit and
			// let the user restart (tmux agents are untouched either way).
			fmt.Fprintln(os.Stderr, "scimux: re-exec after update failed:", err)
			os.Exit(1)
		}
	}()
}
