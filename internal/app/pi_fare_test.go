package app

// Phase 4 integration tests: pi native fixture → projector → session log →
// ReadFare (Tier A including cache-write; pi input mapped direct). Synthetic
// only — never a real pi transcript.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

func piFareTestApp(t *testing.T) *app {
	t.Helper()
	home := t.TempDir()
	return &app{
		byID:        map[string]*Node{},
		mirrors:     map[string]*mirror{},
		home:        home,
		sessionsDir: t.TempDir(),
	}
}

// plantPiNative writes a session-map entry under home and a native JSONL file,
// returning the session id and the native path.
func plantPiNative(t *testing.T, home, sessionID, nativeBody string) (nativePath string) {
	t.Helper()
	nativeDir := filepath.Join(home, ".pi", "agent", "sessions", "--tmp-fixture--")
	if err := os.MkdirAll(nativeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	nativePath = filepath.Join(nativeDir, "2026-08-04T12-00-00-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(nativePath, []byte(nativeBody), 0o600); err != nil {
		t.Fatal(err)
	}
	mapDir := filepath.Join(home, ".pi", "pi-acp")
	if err := os.MkdirAll(mapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"version": 1,
		"sessions": map[string]any{
			sessionID: map[string]string{
				"sessionId":   sessionID,
				"cwd":         "/tmp/fixture",
				"sessionFile": nativePath,
				"updatedAt":   "2026-08-04T12:00:00.000Z",
			},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mapDir, "session-map.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return nativePath
}

func loadPiFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "transcript", "testdata", "pi-native.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPiNative_ClearStartsNewSession(t *testing.T) {
	// O1: /clear yields a new sessionId + new native file. Projector must
	// append a new source seam when the session id changes.
	a := piFareTestApp(t)
	body1 := `{"type":"message","id":"a1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	body2 := `{"type":"message","id":"a2","message":{"role":"assistant","model":"m","usage":{"input":20,"output":3,"cacheRead":0,"cacheWrite":2,"reasoning":0,"totalTokens":23,"cost":{"total":0.02}}}}` + "\n"
	p1 := plantPiNative(t, a.home, "sid-old", body1)

	// Second session: extend the map with a new entry (and a new file).
	p2 := filepath.Join(filepath.Dir(p1), "2026-08-04T13-00-00-000Z_sid-new.jsonl")
	if err := os.WriteFile(p2, []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(a.home, ".pi", "pi-acp", "session-map.json")
	// Re-read and add sid-new.
	raw, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	sessions := m["sessions"].(map[string]any)
	sessions["sid-new"] = map[string]string{
		"sessionId":   "sid-new",
		"cwd":         "/tmp/fixture",
		"sessionFile": p2,
		"updatedAt":   "2026-08-04T13:00:00.000Z",
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(mapPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	n := &Node{ID: "pi1", Agent: "pi", Model: "m", SessionID: "sid-old"}
	a.syncPiFare(n)

	// Switch to the new session (simulates ACP Clear + new sessionId).
	n.SessionID = "sid-new"
	a.syncPiFare(n)

	evs := sessionlog.ReadEvents(filepath.Join(a.sessionsDir, "pi1.jsonl"))
	var sources []sessionlog.SourceEvent
	for _, ev := range evs {
		if ev.T == "source" && ev.Source != nil {
			sources = append(sources, *ev.Source)
		}
	}
	if len(sources) < 2 {
		t.Fatalf("sources = %d, want ≥2 (old bind + new session after clear); events=%+v", len(sources), evs)
	}
	// Last source must bind the new session id (and its native path).
	last := sources[len(sources)-1]
	if last.SessionID != "sid-new" {
		t.Errorf("last source sessionId = %q, want sid-new", last.SessionID)
	}
	if last.Path != p2 {
		t.Errorf("last source path = %q, want %q", last.Path, p2)
	}
	// Both usage turns counted after the session change.
	fare := sessionlog.ReadFare(filepath.Join(a.sessionsDir, "pi1.jsonl"))
	if fare.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (both segments additive for fare)", fare.Turns)
	}
	if fare.CacheWrite != 1+2 {
		t.Errorf("CacheWrite = %d, want 3", fare.CacheWrite)
	}
}

func TestPiNative_ProjectorReadFareIntegration(t *testing.T) {
	// fixture → projector → ReadFare: Tier-A pi totals including cache-write;
	// pi input mapped direct (not subtracted). Reasoning stored but excluded
	// from the four (D7).
	a := piFareTestApp(t)
	plantPiNative(t, a.home, "sess-fix", loadPiFixture(t))
	n := &Node{ID: "pi-int", Agent: "pi", Model: "grok-4.5", SessionID: "sess-fix"}
	a.syncPiFare(n)

	logPath := filepath.Join(a.sessionsDir, "pi-int.jsonl")
	// Spot-check projected UsageEvent fields.
	var usages []*sessionlog.UsageEvent
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "usage" && ev.Usage != nil {
			u := *ev.Usage
			usages = append(usages, &u)
		}
	}
	if len(usages) != 2 {
		t.Fatalf("usage events = %d, want 2", len(usages))
	}
	u0 := usages[0]
	if u0.InputTokens != 3808 || u0.CacheCreationTokens != 512 || u0.ReasoningTokens != 77 {
		t.Errorf("usage0 = %+v", u0)
	}
	if u0.CostAmount != 0.0046 || u0.CostCurrency != "USD" {
		t.Errorf("usage0 cost = %v %q", u0.CostAmount, u0.CostCurrency)
	}
	if u0.Model != "grok-4.5" || u0.TurnID != "msg-asst-1" {
		t.Errorf("usage0 model/turn = %q / %q", u0.Model, u0.TurnID)
	}

	// Canonical fare via ReadFare (agent=pi → direct fresh input).
	// t1: fresh=3808, cacheRead=128, cacheWrite=512, out=202
	// t2: fresh=100,  cacheRead=0,   cacheWrite=20,  out=50
	// → FreshIn=3908, CacheRead=128, CacheWrite=532, Out=252
	// Reasoning 77+5 is NOT in the four (D7).
	f := sessionlog.ReadFare(logPath)
	if f.FreshIn != 3908 {
		t.Errorf("FreshIn = %d, want 3908 (pi input direct, not subtracted)", f.FreshIn)
	}
	if f.CacheRead != 128 {
		t.Errorf("CacheRead = %d, want 128", f.CacheRead)
	}
	if f.CacheWrite != 532 {
		t.Errorf("CacheWrite = %d, want 532 (cache-write present)", f.CacheWrite)
	}
	if f.Out != 252 {
		t.Errorf("Out = %d, want 252 (reasoning excluded from out)", f.Out)
	}
	if f.Turns != 2 {
		t.Errorf("Turns = %d, want 2", f.Turns)
	}
	// Reported cost summed from cost.total.
	wantCost := 0.0046 + 0.00035
	if f.ReportedCostUSD < wantCost-1e-9 || f.ReportedCostUSD > wantCost+1e-9 {
		t.Errorf("ReportedCostUSD = %v, want %v", f.ReportedCostUSD, wantCost)
	}
	if !f.ReportedCostComplete {
		t.Error("ReportedCostComplete = false, want true")
	}

	// Idle re-sync must not double-count.
	a.syncPiFare(n)
	f2 := sessionlog.ReadFare(logPath)
	if f2.Turns != 2 || f2.FreshIn != 3908 {
		t.Errorf("after idle re-sync Turns=%d FreshIn=%d (double-count?)", f2.Turns, f2.FreshIn)
	}
}

func TestPiNative_MissingMapDegrades(t *testing.T) {
	// No session-map / wrong session id: no crash, no log, no fare.
	a := piFareTestApp(t)
	n := &Node{ID: "pi-miss", Agent: "pi", SessionID: "nope"}
	a.syncPiFare(n) // must not panic
	if _, err := os.Stat(filepath.Join(a.sessionsDir, "pi-miss.jsonl")); !os.IsNotExist(err) {
		t.Fatal("missing map must not create a session log")
	}
	// Map exists but file missing: degrade, no panic.
	mapDir := filepath.Join(a.home, ".pi", "pi-acp")
	if err := os.MkdirAll(mapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"version": 1,
		"sessions": map[string]any{
			"sid": map[string]string{
				"sessionId":   "sid",
				"sessionFile": filepath.Join(a.home, "does-not-exist.jsonl"),
			},
		},
	}
	b, _ := json.Marshal(body)
	if err := os.WriteFile(filepath.Join(mapDir, "session-map.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	n2 := &Node{ID: "pi-gone", Agent: "pi", SessionID: "sid"}
	a.syncPiFare(n2)
	// May or may not create a log with only meta/source; must not panic and
	// ReadFare must stay zero (no usage projected).
	if got := sessionlog.ReadFare(filepath.Join(a.sessionsDir, "pi-gone.jsonl")); got.Turns != 0 {
		t.Errorf("missing native file projected turns=%d, want 0", got.Turns)
	}
}

// Compile-time guard: PiUsage fields used by the projector stay aligned with
// sessionlog.UsageEvent projection (this test fails to compile if renamed).
func TestPiNative_PiUsageShape(t *testing.T) {
	var u transcript.PiUsage
	_ = sessionlog.UsageEvent{
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CachedReadTokens:    u.CachedReadTokens,
		CacheCreationTokens: u.CacheCreationTokens,
		ReasoningTokens:     u.ReasoningTokens,
		TotalTokens:         u.TotalTokens,
		CostAmount:          u.CostAmount,
		CostCurrency:        u.CostCurrency,
		Model:               u.Model,
		TurnID:              u.TurnID,
	}
}
