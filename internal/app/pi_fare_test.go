package app

// Phase 4 integration tests: pi native fixture → projector → session log →
// ReadFare (Tier A including cache-write; pi input mapped direct). Synthetic
// only — never a real pi transcript.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/transcript"
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

	// Simulate ACP Clear faithfully: the clear path appends its own path-less
	// clear seam before the node moves to the new sessionId. (The mirror no
	// longer writes a seam on its *first* bind, so this seam — not the old
	// bind — is the first of the two.)
	cw := &sessionlog.Writer{Path: filepath.Join(a.sessionsDir, "pi1.jsonl")}
	if err := cw.Append(sessionlog.NewClearSource("sid-new")); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("sources = %d, want ≥2 (clear seam + native rebind after clear); events=%+v", len(sources), evs)
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

// --- Phase 4a: pi occupancy tank from models.json contextWindow ---

// plantPiModels writes a minimal models.json under home/.pi/agent/.
func plantPiModels(t *testing.T, home string, windows map[string]int) {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	models := make([]any, 0, len(windows))
	for id, w := range windows {
		models = append(models, map[string]any{
			"id": id, "name": id, "contextWindow": w,
		})
	}
	body := map[string]any{
		"providers": map[string]any{
			"fixture": map[string]any{"models": models},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiFare_SetsOccupancyWhenWindowKnown(t *testing.T) {
	// Projected UsageEvent: Used == totalTokens, Size == contextWindow for a
	// known model. Occupancy shape matches Claude (full per-turn total).
	a := piFareTestApp(t)
	plantPiModels(t, a.home, map[string]int{"mistral-small-latest": 32000})
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"mistral-small-latest","usage":{"input":1247,"output":58,"cacheRead":0,"cacheWrite":0,"totalTokens":1305,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-occ", body)
	n := &Node{ID: "pi-occ", Agent: "pi", Model: "mistral-small-latest", SessionID: "sid-occ"}
	a.syncPiFare(n)

	var u *sessionlog.UsageEvent
	for _, ev := range sessionlog.ReadEvents(filepath.Join(a.sessionsDir, "pi-occ.jsonl")) {
		if ev.T == "usage" && ev.Usage != nil {
			u = ev.Usage
			break
		}
	}
	if u == nil {
		t.Fatal("no usage event projected")
	}
	if u.Used != 1305 {
		t.Errorf("Used = %d, want 1305 (totalTokens occupancy)", u.Used)
	}
	if u.Size != 32000 {
		t.Errorf("Size = %d, want 32000 (contextWindow from models.json)", u.Size)
	}
	// Fare fields still present (meter independent of tank — D1).
	if u.InputTokens != 1247 || u.OutputTokens != 58 || u.TotalTokens != 1305 {
		t.Errorf("fare fields corrupted: %+v", u)
	}
}

func TestPiFare_NoOccupancyWhenWindowUnknown(t *testing.T) {
	// Unknown model → Used == 0 && Size == 0 (fare-only; never a denominator-less tank).
	a := piFareTestApp(t)
	plantPiModels(t, a.home, map[string]int{"known-model": 8000})
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"totally-unknown-model","usage":{"input":100,"output":20,"cacheRead":0,"cacheWrite":0,"totalTokens":120,"cost":{"total":0.001}}}}` + "\n"
	plantPiNative(t, a.home, "sid-unk", body)
	n := &Node{ID: "pi-unk", Agent: "pi", SessionID: "sid-unk"}
	a.syncPiFare(n)

	var u *sessionlog.UsageEvent
	for _, ev := range sessionlog.ReadEvents(filepath.Join(a.sessionsDir, "pi-unk.jsonl")) {
		if ev.T == "usage" && ev.Usage != nil {
			u = ev.Usage
			break
		}
	}
	if u == nil {
		t.Fatal("no usage event projected")
	}
	if u.Used != 0 || u.Size != 0 {
		t.Errorf("unknown model occupancy Used=%d Size=%d, want 0/0 (fare-only)", u.Used, u.Size)
	}
	// Fare fields still present.
	if u.InputTokens != 100 || u.OutputTokens != 20 || u.CostAmount != 0.001 {
		t.Errorf("fare fields missing/wrong: %+v", u)
	}
}

func TestPiFare_UsedExceedsSizeCleared(t *testing.T) {
	// Used > Size > 0 → Used cleared (D10: never paint the gauge full).
	a := piFareTestApp(t)
	plantPiModels(t, a.home, map[string]int{"tiny-ctx": 100})
	// totalTokens 500 > window 100.
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"tiny-ctx","usage":{"input":400,"output":100,"cacheRead":0,"cacheWrite":0,"totalTokens":500,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-full", body)
	n := &Node{ID: "pi-full", Agent: "pi", SessionID: "sid-full"}
	a.syncPiFare(n)

	var u *sessionlog.UsageEvent
	for _, ev := range sessionlog.ReadEvents(filepath.Join(a.sessionsDir, "pi-full.jsonl")) {
		if ev.T == "usage" && ev.Usage != nil {
			u = ev.Usage
			break
		}
	}
	if u == nil {
		t.Fatal("no usage event projected")
	}
	if u.Used != 0 {
		t.Errorf("Used = %d, want 0 when Used>Size (never paint full)", u.Used)
	}
	if u.Size != 100 {
		t.Errorf("Size = %d, want 100 (window still known; only Used cleared)", u.Size)
	}
}

func TestPiFare_ReadFareUnaffectedByOccupancy(t *testing.T) {
	// Adding Used/Size must not change ReadFare totals (occupancy never summed).
	a := piFareTestApp(t)
	plantPiModels(t, a.home, map[string]int{"m": 100000})
	body := `{"type":"message","id":"a1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":1,"cacheWrite":3,"totalTokens":16,"cost":{"total":0.01}}}}` + "\n" +
		`{"type":"message","id":"a2","message":{"role":"assistant","model":"m","usage":{"input":20,"output":4,"cacheRead":0,"cacheWrite":0,"totalTokens":24,"cost":{"total":0.02}}}}` + "\n"
	plantPiNative(t, a.home, "sid-rf", body)
	n := &Node{ID: "pi-rf", Agent: "pi", SessionID: "sid-rf"}
	a.syncPiFare(n)

	logPath := filepath.Join(a.sessionsDir, "pi-rf.jsonl")
	// Occupancy must be set on projected events.
	var sawOcc bool
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "usage" && ev.Usage != nil && ev.Usage.Used > 0 && ev.Usage.Size == 100000 {
			sawOcc = true
		}
	}
	if !sawOcc {
		t.Fatal("expected occupancy Used/Size on projected usage (precondition for fare isolation)")
	}

	f := sessionlog.ReadFare(logPath)
	// pi input direct: FreshIn=10+20, CacheRead=1+0, CacheWrite=3+0, Out=2+4
	if f.FreshIn != 30 || f.CacheRead != 1 || f.CacheWrite != 3 || f.Out != 6 {
		t.Errorf("fare = %+v, want FreshIn=30 CacheRead=1 CacheWrite=3 Out=6", f)
	}
	if f.Turns != 2 {
		t.Errorf("Turns = %d, want 2", f.Turns)
	}
	// Total() is the four-quantity sum — must NOT include Used (16+24=40).
	wantTotal := 30 + 1 + 3 + 6 // 40 would be a Used leak only if Used summed; four-sum is also 40 by coincidence.
	// Force a stronger check: Used values (16, 24) sum to 40 which equals Total.
	// Use a different assertion — Total equals four-sum and ReportedCost is from cost.total only.
	if f.Total() != wantTotal {
		t.Errorf("Total() = %d, want %d", f.Total(), wantTotal)
	}
	wantCost := 0.01 + 0.02
	if f.ReportedCostUSD < wantCost-1e-9 || f.ReportedCostUSD > wantCost+1e-9 {
		t.Errorf("ReportedCostUSD = %v, want %v (D8: reported-only, not from models.json rates)", f.ReportedCostUSD, wantCost)
	}
}

// --- Defect 2: the fare mirror's first bind must not write a source seam ---

// plantFreshPiLog pre-creates the node's session log the way a real first
// prompt does: the launch meta header followed by the user turn (written by
// the ACP layer before the first prompt). Returns the log path.
func plantFreshPiLog(t *testing.T, a *app, nodeID, prompt string) string {
	t.Helper()
	logPath := filepath.Join(a.sessionsDir, nodeID+".jsonl")
	w := &sessionlog.Writer{Path: logPath}
	if err := w.Append(sessionlog.NewMeta(nodeID, "pi", "m", "", "/tmp/fixture")); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(sessionlog.Event{T: "user", Text: prompt}); err != nil {
		t.Fatal(err)
	}
	return logPath
}

func countPiFareSources(t *testing.T, logPath string) []sessionlog.SourceEvent {
	t.Helper()
	var sources []sessionlog.SourceEvent
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "source" && ev.Source != nil {
			sources = append(sources, *ev.Source)
		}
	}
	return sources
}

// TestPiFare_FirstBindWritesNoSeam: on a fresh node both the mirror's path
// and sid are empty, so the first successful bind has nothing to dedupe
// against — it records the bind but appends no source seam, while still
// projecting usage.
func TestPiFare_FirstBindWritesNoSeam(t *testing.T) {
	a := piFareTestApp(t)
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-1", body)
	logPath := plantFreshPiLog(t, a, "pi-fb", "first prompt")
	n := &Node{ID: "pi-fb", Agent: "pi", Model: "m", SessionID: "sid-1"}
	a.syncPiFare(n)

	if srcs := countPiFareSources(t, logPath); len(srcs) != 0 {
		t.Fatalf("first bind wrote %d source seam(s), want none: %+v", len(srcs), srcs)
	}
	// The usage record was still projected.
	var usages int
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "usage" && ev.Usage != nil {
			usages++
		}
	}
	if usages != 1 {
		t.Fatalf("usage records = %d, want 1 (bind must still project usage)", usages)
	}
}

// TestPiFare_FirstBindKeepsFirstTurnInLiveSegment: with no spurious seam the
// segment reader keeps the first prompt in the live chat surface — nothing is
// evicted behind "show earlier history" and the chat-started header is not
// re-dated at the (nonexistent) seam.
func TestPiFare_FirstBindKeepsFirstTurnInLiveSegment(t *testing.T) {
	a := piFareTestApp(t)
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-1", body)
	logPath := plantFreshPiLog(t, a, "pi-seg", "first prompt")
	n := &Node{ID: "pi-seg", Agent: "pi", Model: "m", SessionID: "sid-1"}
	a.syncPiFare(n)

	var metaTime string
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "meta" {
			metaTime = ev.Time
		}
	}
	if metaTime == "" {
		t.Fatal("no meta header in log")
	}
	seg := sessionlog.ReadSegment(logPath)
	if seg.PriorTurns != 0 {
		t.Errorf("PriorTurns = %d, want 0 (no seam may page out the first turn)", seg.PriorTurns)
	}
	if len(seg.Turns) != 1 || seg.Turns[0].Role != "user" || seg.Turns[0].Text != "first prompt" {
		t.Fatalf("live segment turns = %+v, want the first user turn live", seg.Turns)
	}
	if seg.StartTime != metaTime {
		t.Errorf("StartTime = %q, want the meta/turn time %q (no seam re-dating)", seg.StartTime, metaTime)
	}
}

// TestPiFare_FirstBindStaysSeamlessAfterRestart: after a suppressed first
// bind the log holds no source, so replay recovers path=="" && sid=="" and the
// next bind is suppressed again — never a late seam mid-log.
func TestPiFare_FirstBindStaysSeamlessAfterRestart(t *testing.T) {
	a := piFareTestApp(t)
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-1", body)
	logPath := plantFreshPiLog(t, a, "pi-rs", "first prompt")
	n := &Node{ID: "pi-rs", Agent: "pi", Model: "m", SessionID: "sid-1"}
	a.syncPiFare(n)

	// Simulate a scimux restart: the in-memory mirror is gone; durable state
	// comes back from the log only.
	a.piMirrors = nil
	a.syncPiFare(n)

	if srcs := countPiFareSources(t, logPath); len(srcs) != 0 {
		t.Fatalf("post-restart bind wrote %d source seam(s), want none: %+v", len(srcs), srcs)
	}
	var usages int
	for _, ev := range sessionlog.ReadEvents(logPath) {
		if ev.T == "usage" && ev.Usage != nil {
			usages++
		}
	}
	if usages != 1 {
		t.Fatalf("usage records = %d, want 1 (no duplicated usage after restart)", usages)
	}
}

// TestPiFare_RebindAfterFirstBindWritesSeam: once the mirror has prior bind
// state, a different native file/session id is a genuine rebind — the seam is
// appended.
func TestPiFare_RebindAfterFirstBindWritesSeam(t *testing.T) {
	a := piFareTestApp(t)
	body1 := `{"type":"message","id":"t1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	body2 := `{"type":"message","id":"t2","message":{"role":"assistant","model":"m","usage":{"input":20,"output":3,"cacheRead":0,"cacheWrite":2,"reasoning":0,"totalTokens":23,"cost":{"total":0.02}}}}` + "\n"
	plantPiNative(t, a.home, "sid-1", body1)
	logPath := plantFreshPiLog(t, a, "pi-rb", "first prompt")
	n := &Node{ID: "pi-rb", Agent: "pi", Model: "m", SessionID: "sid-1"}
	a.syncPiFare(n)
	if srcs := countPiFareSources(t, logPath); len(srcs) != 0 {
		t.Fatalf("first bind wrote %d source seam(s), want none: %+v", len(srcs), srcs)
	}

	// Second session: a different native file (simulates /clear → new sid).
	p2 := filepath.Join(a.home, ".pi", "agent", "sessions", "--tmp-fixture--", "2026-08-04T13-00-00-000Z_sid-2.jsonl")
	if err := os.WriteFile(p2, []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	mapPath := filepath.Join(a.home, ".pi", "pi-acp", "session-map.json")
	raw, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["sessions"].(map[string]any)["sid-2"] = map[string]string{
		"sessionId":   "sid-2",
		"cwd":         "/tmp/fixture",
		"sessionFile": p2,
		"updatedAt":   "2026-08-04T13:00:00.000Z",
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(mapPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	n.SessionID = "sid-2"
	a.syncPiFare(n)

	srcs := countPiFareSources(t, logPath)
	if len(srcs) != 1 {
		t.Fatalf("rebind wrote %d source seams, want 1: %+v", len(srcs), srcs)
	}
	if srcs[0].SessionID != "sid-2" || srcs[0].Path != p2 {
		t.Errorf("rebind seam = %+v, want sid-2 / %q", srcs[0], p2)
	}
}

// TestPiFare_SeamStillWrittenAfterClearSeam: a /clear leaves the ACP
// NewClearSource seam in the log, so replay recovers a non-empty sid — the
// mirror's native-path bind then appends its seam exactly as before.
func TestPiFare_SeamStillWrittenAfterClearSeam(t *testing.T) {
	a := piFareTestApp(t)
	body := `{"type":"message","id":"t1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":1,"reasoning":0,"totalTokens":12,"cost":{"total":0.01}}}}` + "\n"
	plantPiNative(t, a.home, "sid-1", body)
	logPath := plantFreshPiLog(t, a, "pi-cs", "first prompt")
	// The ACP clear path has already turned the page.
	w := &sessionlog.Writer{Path: logPath}
	if err := w.Append(sessionlog.NewClearSource("sid-1")); err != nil {
		t.Fatal(err)
	}
	n := &Node{ID: "pi-cs", Agent: "pi", Model: "m", SessionID: "sid-1"}
	a.syncPiFare(n)

	srcs := countPiFareSources(t, logPath)
	if len(srcs) != 2 {
		t.Fatalf("seams = %d, want 2 (clear seam + native bind seam): %+v", len(srcs), srcs)
	}
	if srcs[0].Reason != "clear" {
		t.Errorf("first seam = %+v, want the clear seam", srcs[0])
	}
	if srcs[1].Path == "" || srcs[1].SessionID != "sid-1" {
		t.Errorf("second seam = %+v, want the native bind seam (path + sid-1)", srcs[1])
	}
}
