package main

// Manual update check and self-update. The binary never touches the network
// on its own: both the check and the download run only when the user taps
// the corresponding control in the burger menu ("no cloud" holds by
// construction). The update is notify-and-confirm, never silent: the UI asks
// before /api/update is called, and the handler verifies the downloaded
// binary against the release's SHA256SUMS before the atomic rename + re-exec.

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The binary carries its own legal notices (MPL-2.0 §3.2 asks executable
// distributions to say where source and license live; the About sheet does).
//
//go:embed LICENSE
var ownLicense string

//go:embed legal/acp-go-sdk.LICENSE
var acpLicense string

//go:embed legal/go.LICENSE
var goLicense string

func handleLicenses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{
		"scimux": ownLicense,
		"acp":    acpLicense,
		"go":     goLicense,
	})
}

// releaseAPIBase is a var only so tests can point it at a fake Forgejo.
var releaseAPIBase = "https://codeberg.org/api/v1/repos/chrberger/scimux"

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

// downloadVerified streams url into a temp file next to dest (same
// filesystem, so the final rename is atomic) and returns the temp path only
// if the SHA-256 digest matches the sums entry for name.
func downloadVerified(ctx context.Context, url, sums, name, dest string) (string, error) {
	want := ""
	for _, line := range strings.Split(sums, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return "", fmt.Errorf("SHA256SUMS has no entry for %q", name)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download %s: %s", name, resp.Status)
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".scimux-update-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("checksum mismatch for %s", name)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func fetchText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
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
	if err := os.Chmod(tmp, 0o755); err == nil {
		err = os.Rename(tmp, exe)
	}
	if err != nil {
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
