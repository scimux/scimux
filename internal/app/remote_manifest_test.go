package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// S8-manifest R1 — the computer half of FR-40. bootstrap.js is the consumer
// (web/js/bootstrap.js); these tests pin the producer against the served
// inventory rather than against a second walker.

const bootstrapConsumer = "web/js/bootstrap.js"

type bootstrapManifestWire struct {
	Source  string                       `json:"source"`
	Entry   string                       `json:"entry"`
	Entries []bootstrapManifestEntryWire `json:"entries"`
}

type bootstrapManifestEntryWire struct {
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	Size      int    `json:"size"`
	Integrity string `json:"integrity"`
}

func getBootstrapManifest(t *testing.T, h http.Handler) bootstrapManifestWire {
	t.Helper()
	rec := getCharacterization(t, h, "/api/remote/bootstrap")
	var m bootstrapManifestWire
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/bootstrap status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("GET /api/remote/bootstrap JSON: %v; body=%q", err, rec.Body.String())
	}
	return m
}

func independentSRI(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func jsonObjectKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("JSON object: %v (%q)", err, raw)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinKeys(keys []string) string {
	return strings.Join(keys, ",")
}

// TestRemoteBootstrapManifestDerivesFromServedInventory is the assertion that
// makes a newly added module impossible to miss: the producer's entries —
// URL and kind — are exactly servedAssetInventory, not a second walker.
func TestRemoteBootstrapManifestDerivesFromServedInventory(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	m := getBootstrapManifest(t, h)
	want := servedAssetInventory(t)
	if len(m.Entries) != len(want) {
		t.Fatalf("manifest entries=%d, servedAssetInventory=%d; FR-40 forbids a partial boot",
			len(m.Entries), len(want))
	}
	for i, a := range want {
		got := m.Entries[i]
		if got.URL != a.URL || got.Kind != a.Kind {
			t.Errorf("entries[%d] = {%s %s}, want {%s %s} from servedAssetInventory",
				i, got.URL, got.Kind, a.URL, a.Kind)
		}
	}
}

// TestRemoteBootstrapManifestIntegrityAndSizeMatchServedBytes recomputes
// sha256 and length from the bytes the handlers actually serve. Deriving
// both from the same call under test would prove nothing.
func TestRemoteBootstrapManifestIntegrityAndSizeMatchServedBytes(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	m := getBootstrapManifest(t, h)
	if len(m.Entries) == 0 {
		t.Fatal("manifest entries is empty")
	}
	for _, e := range m.Entries {
		rec := getCharacterization(t, h, e.URL)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d, want 200 (served path for integrity check)", e.URL, rec.Code)
		}
		body := rec.Body.Bytes()
		wantSRI := independentSRI(body)
		if e.Integrity != wantSRI {
			t.Errorf("%s integrity=%q, independently recomputed from served bytes %q",
				e.URL, e.Integrity, wantSRI)
		}
		if e.Size != len(body) {
			t.Errorf("%s size=%d, served byte length=%d", e.URL, e.Size, len(body))
		}
	}
}

// TestRemoteBootstrapManifestAntiVacuityCounts pins exact per-kind counts.
// FR-40's failure mode is a missing entry; len > 0 cannot see it.
func TestRemoteBootstrapManifestAntiVacuityCounts(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	m := getBootstrapManifest(t, h)
	if len(m.Entries) == 0 {
		t.Fatal("manifest entries is empty; FR-40's failure mode is a missing entry")
	}
	byKind := map[string]int{}
	for _, e := range m.Entries {
		byKind[e.Kind]++
	}
	for kind, want := range map[string]int{
		"index": 1,
		"js":    33,
		"css":   10,
		"asset": 18,
	} {
		if byKind[kind] != want {
			t.Errorf("manifest holds %d %s entries, want %d", byKind[kind], kind, want)
		}
	}
}

// TestRemoteBootstrapManifestSourceAndEntry pins the two fields
// web/js/bootstrap.js refuses on before the channel is touched.
func TestRemoteBootstrapManifestSourceAndEntry(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	m := getBootstrapManifest(t, h)
	if m.Source != "computer" {
		t.Errorf("source=%q, want %q; %s refuses any other value before the channel is touched",
			m.Source, "computer", bootstrapConsumer)
	}
	if m.Entry != "/js/app.js" {
		t.Errorf("entry=%q, want /js/app.js; %s uses that as the module graph entry",
			m.Entry, bootstrapConsumer)
	}
}

// TestRemoteBootstrapManifestJSONShape marshals the producer JSON and
// asserts the key sets bootstrap.js will read. Extra or renamed keys are
// a seam break even if Go's decoder would ignore them.
func TestRemoteBootstrapManifestJSONShape(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	rec := getCharacterization(t, h, "/api/remote/bootstrap")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/bootstrap status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	raw, err := json.Marshal(mustUnmarshalAny(t, rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	wantTop := []string{"source", "entry", "entries"}
	sort.Strings(wantTop)
	gotTop := jsonObjectKeys(t, raw)
	if joinKeys(gotTop) != joinKeys(wantTop) {
		t.Fatalf("%s consumes top-level keys exactly source, entry, entries; got %v",
			bootstrapConsumer, gotTop)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(root["entries"], &entries); err != nil {
		t.Fatalf("entries JSON: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("entries is empty")
	}
	wantEntry := []string{"url", "kind", "size", "integrity"}
	sort.Strings(wantEntry)
	for i, e := range entries {
		marshaled, err := json.Marshal(mustUnmarshalAny(t, e))
		if err != nil {
			t.Fatalf("marshal entries[%d]: %v", i, err)
		}
		got := jsonObjectKeys(t, marshaled)
		if joinKeys(got) != joinKeys(wantEntry) {
			t.Fatalf("%s entry keys are exactly url, kind, size, integrity; entries[%d] has %v",
				bootstrapConsumer, i, got)
		}
	}
}

func mustUnmarshalAny(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v (%q)", err, raw)
	}
	return v
}

// TestRemoteBootstrapManifestRoute pins delivery: a normal GET, JSON,
// round-trip into the manifest, no CSRF.
func TestRemoteBootstrapManifestRoute(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))

	rec := routeRequest(h, http.MethodGet, "/api/remote/bootstrap", "", false)
	if rec.Code == http.StatusForbidden {
		t.Fatal("GET /api/remote/bootstrap without CSRF = 403; GET must not require the header")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/bootstrap status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var m bootstrapManifestWire
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body does not round-trip into the manifest: %v (%q)", err, rec.Body.String())
	}
	if m.Source == "" || m.Entry == "" || len(m.Entries) == 0 {
		t.Fatalf("round-tripped manifest is vacant: %+v", m)
	}
}

// TestRemoteBootstrapManifestStableAcrossCalls pins compute-once without
// asserting on the caching mechanism: two GETs return equal manifests.
func TestRemoteBootstrapManifestStableAcrossCalls(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))
	a := getCharacterization(t, h, "/api/remote/bootstrap")
	b := getCharacterization(t, h, "/api/remote/bootstrap")
	if a.Code != http.StatusOK || b.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/bootstrap status a=%d b=%d, want 200", a.Code, b.Code)
	}
	if a.Body.String() != b.Body.String() {
		t.Fatalf("two calls returned unequal manifests:\n%s\n---\n%s", a.Body.String(), b.Body.String())
	}
}
