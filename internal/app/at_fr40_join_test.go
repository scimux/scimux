package app

// FR-41 — the end-to-end join for FR-40.
//
// Two halves of the bootstrap seam were built separately and had never met:
// remote_manifest.go produces the manifest, web/js/bootstrap.js consumes it,
// and every test on either side supplied the other side itself. The Go tests
// compared the producer against a Go walker; the web tests fed bootstrap.js a
// manifest a JS walker built from disk. Both can be green while the two
// disagree.
//
// This test removes the translator. It runs the *embedded* bootstrap.js under
// Node against a live server running the real handlers, and lets bootstrap.js
// fetch the manifest from the real route. Nothing in between restates what
// either side believes.
//
// The sharp assertion is the index. remote_manifest.go hashes the
// CSRF-substituted page rather than web/index.html on disk, and there is no
// way to notice a mistake there from inside either suite: the Go side would
// hash and serve the same wrong bytes, and the JS side never sees a real
// server at all. Here a mismatch aborts the whole boot as
// "bootstrap-integrity", which is exactly how it would present in a browser.

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type bootstrapJoinResult struct {
	Error string `json:"error"`

	ManifestEntries int    `json:"manifestEntries"`
	ManifestSource  string `json:"manifestSource"`
	ManifestEntry   string `json:"manifestEntry"`

	ChannelCalls  int      `json:"channelCalls"`
	ChannelExtras []string `json:"channelExtras"`

	Imported            []string `json:"imported"`
	ObjectURLs          int      `json:"objectURLs"`
	ImportMapsInstalled int      `json:"importMapsInstalled"`
	Unresolvable        []string `json:"unresolvable"`
	Rewritten           int      `json:"rewritten"`
	Stylesheets         int      `json:"stylesheets"`
	Assets              int      `json:"assets"`
	Entry               string   `json:"entry"`

	IndexLength         int  `json:"indexLength"`
	IndexHasToken       bool `json:"indexHasToken"`
	IndexHasPlaceholder bool `json:"indexHasPlaceholder"`
}

// runBootstrapJoin serves the real handlers, then drives the embedded
// bootstrap.js against them under Node.
func runBootstrapJoin(t *testing.T) bootstrapJoinResult {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping the FR-40 bootstrap join")
	}

	// The embedded bytes, not the working copy: this is the file the binary
	// serves, and the only one a paired device can ever receive.
	src, err := webFS.ReadFile("web/js/bootstrap.js")
	if err != nil {
		t.Fatalf("read embedded web/js/bootstrap.js: %v", err)
	}
	mod := filepath.Join(t.TempDir(), "bootstrap.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatalf("write bootstrap module: %v", err)
	}

	srv := httptest.NewServer(newTestHandler(t, newTestApp(t, &fakeTmux{})))
	defer srv.Close()

	cmd := exec.Command(node, "testdata/bootstrap_join.mjs", "file://"+mod, srv.URL, csrfToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap join driver failed: %v\n%s", err, out)
	}

	// The driver prints one JSON line; anything before it is Node noise worth
	// showing rather than swallowing.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var res bootstrapJoinResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("driver output is not JSON: %v\n%s", err, out)
	}
	return res
}

// TestAT_FR_40_JoinRealManifestBootsRealBootstrap is the join itself.
func TestAT_FR_40_JoinRealManifestBootsRealBootstrap(t *testing.T) {
	const at = "AT-FR-40-join"
	res := runBootstrapJoin(t)

	if res.Error != "" {
		t.Fatalf("%s: the real manifest did not boot the real bootstrap.js: %s", at, res.Error)
	}

	// Anti-vacuity. A driver that silently did nothing would satisfy every
	// assertion below that is phrased as an absence.
	want := len(servedAssetInventory(t))
	if res.ManifestEntries != want {
		t.Fatalf("%s: manifest served %d entries, servedAssetInventory has %d",
			at, res.ManifestEntries, want)
	}
	if res.ChannelCalls != want {
		t.Errorf("%s: bootstrap fetched %d URLs over the channel, manifest names %d",
			at, res.ChannelCalls, want)
	}
	if len(res.ChannelExtras) != 0 {
		t.Errorf("%s: bootstrap fetched URLs the manifest does not name: %v; FR-40 requires the manifest to name exactly what is fetched",
			at, res.ChannelExtras)
	}

	if res.ManifestSource != "computer" || res.ManifestEntry != "/js/app.js" {
		t.Errorf("%s: manifest source=%q entry=%q, want computer and /js/app.js",
			at, res.ManifestSource, res.ManifestEntry)
	}

	// One entry module started, and it is the manifest's entry blob.
	if len(res.Imported) != 1 {
		t.Fatalf("%s: %d modules imported, want exactly the entry", at, len(res.Imported))
	}
	if res.Entry == "" || res.Imported[0] != res.Entry {
		t.Errorf("%s: imported %q but the entry blob is %q", at, res.Imported[0], res.Entry)
	}

	// Everything except the index gets exactly one object URL; the index does
	// not. One per asset is also one module instance per module: a second blob
	// for the same URL would be a second copy in the browser's module map.
	if res.ObjectURLs != want-1 {
		t.Errorf("%s: %d object URLs, want %d (every served asset but the index)",
			at, res.ObjectURLs, want-1)
	}

	// The defect that kept a real device on the failure screen with both
	// suites green. A blob: URL has an opaque path, so nothing relative or
	// root-absolute resolves against it and the specifier never reaches an
	// import-map key — the map was unreachable, not late. Every import in a
	// minted module must therefore already be an absolute blob: URL, and no
	// map may be installed to stand in for one that is not.
	if len(res.Unresolvable) != 0 {
		t.Errorf("%s: minted modules still import %v; a blob: base resolves none of those",
			at, res.Unresolvable)
	}
	if res.Rewritten < 20 {
		t.Errorf("%s: only %d specifiers were rewritten to object URLs; the graph check is vacuous",
			at, res.Rewritten)
	}
	if res.ImportMapsInstalled != 0 {
		t.Errorf("%s: %d import maps installed; a blob: module cannot consult one",
			at, res.ImportMapsInstalled)
	}
	if res.Stylesheets != 10 || res.Assets != 9 {
		t.Errorf("%s: stylesheets=%d assets=%d, want 10 and 9", at, res.Stylesheets, res.Assets)
	}
}

// TestAT_FR_40_JoinIndexIsTheServedPage pins the one fact neither suite can
// see alone: the manifest's "/" digest is over the page GET / returns, so the
// index bootstrap recovers carries this process's CSRF token. Hashing
// web/index.html from disk instead fails the whole boot on integrity.
func TestAT_FR_40_JoinIndexIsTheServedPage(t *testing.T) {
	const at = "AT-FR-40-join-index"
	res := runBootstrapJoin(t)

	if res.Error != "" {
		t.Fatalf("%s: boot failed before the index could be checked: %s", at, res.Error)
	}
	if res.IndexLength == 0 {
		t.Fatalf("%s: bootstrap recovered no index", at)
	}
	if res.IndexHasPlaceholder {
		t.Errorf("%s: the index still contains %s; the manifest described the unsubstituted file",
			at, csrfPlaceholder)
	}
	if !res.IndexHasToken {
		t.Errorf("%s: the index does not carry this process's CSRF token; the manifest did not describe the served page",
			at)
	}
}
