package fare

import "testing"

// §2.3 golden fixtures: per-agent fresh-input semantics (fare-design.md).
// These numbers are empirical; the opencode case is the negative-fresh trap.

func TestNormalize_ClaudeFreshInputDirect(t *testing.T) {
	// Claude input_tokens is already fresh-only.
	raw := RawTokens{
		Input:      2,
		Output:     410,
		CacheRead:  0,
		CacheWrite: 25437,
	}
	got := Normalize("claude", raw)
	if got.FreshIn != 2 {
		t.Errorf("FreshIn = %d, want 2 (direct, not whole-prompt)", got.FreshIn)
	}
	// Pass-through fields must land correctly so a refactor can't drop them.
	if got.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0", got.CacheRead)
	}
	if got.CacheWrite != 25437 {
		t.Errorf("CacheWrite = %d, want 25437", got.CacheWrite)
	}
	if got.Out != 410 {
		t.Errorf("Out = %d, want 410", got.Out)
	}
}

func TestNormalize_PiFreshInputDirect(t *testing.T) {
	// pi usage.input is fresh-only.
	raw := RawTokens{
		Input:      30,
		Output:     10,
		CacheRead:  80,
		CacheWrite: 0,
	}
	got := Normalize("pi", raw)
	if got.FreshIn != 30 {
		t.Errorf("FreshIn = %d, want 30 (direct)", got.FreshIn)
	}
}

func TestNormalize_OpencodeFreshInputDirect(t *testing.T) {
	// opencode tokens.input is fresh-only. Synthetic input is less than cacheRead;
	// a subtract rule would yield 30−80 = −50 (clamped to 0) — the
	// negative-fresh trap. Must stay direct: FreshIn:30, not −50 or 0.
	raw := RawTokens{
		Input:     30,
		CacheRead: 80,
		Output:    100,
	}
	got := Normalize("opencode", raw)
	if got.FreshIn != 30 {
		t.Errorf("FreshIn = %d, want 30 (direct; subtract would be −50)", got.FreshIn)
	}
}

func TestNormalize_CursorFreshInputDirect(t *testing.T) {
	// Cursor is ACP like pi/opencode. State the rule explicitly so adding a
	// usage split later cannot silently inherit the unknown-agent fallback.
	if got, ok := agentFreshRule["cursor"]; !ok || got != direct {
		t.Errorf("agentFreshRule[cursor] = %v (present=%v), want direct stated explicitly", got, ok)
	}
	raw := RawTokens{Input: 30, CacheRead: 80, Output: 100}
	if got := Normalize("cursor", raw); got.FreshIn != 30 {
		t.Errorf("FreshIn = %d, want 30 (direct; subtract would be −50)", got.FreshIn)
	}
}

func TestNormalize_CodexSubtractsCache(t *testing.T) {
	// codex inputTokens is whole-prompt → fresh_in = input − cache_read.
	raw := RawTokens{
		Input:     100,
		CacheRead: 40,
		Output:    500,
	}
	got := Normalize("codex", raw)
	if got.FreshIn != 60 {
		t.Errorf("FreshIn = %d, want 60 (100−40)", got.FreshIn)
	}
}

func TestNormalize_GrokSubtractsCache(t *testing.T) {
	// grok nested Layer-B usage.inputTokens is whole-prompt spend.
	// Pass-through fields asserted so they can't silently disappear.
	raw := RawTokens{
		Input:      100,
		CacheRead:  40,
		CacheWrite: 0,
		Output:     20,
	}
	got := Normalize("grok", raw)
	if got.FreshIn != 60 {
		t.Errorf("FreshIn = %d, want 60 (100−40)", got.FreshIn)
	}
	if got.CacheRead != 40 {
		t.Errorf("CacheRead = %d, want 40", got.CacheRead)
	}
	if got.CacheWrite != 0 {
		t.Errorf("CacheWrite = %d, want 0", got.CacheWrite)
	}
	if got.Out != 20 {
		t.Errorf("Out = %d, want 20", got.Out)
	}
}

func TestNormalize_SubtractClampsAtZero(t *testing.T) {
	// Never emit negative fresh_in for subtract agents.
	raw := RawTokens{Input: 100, CacheRead: 200}
	for _, agent := range []string{"codex", "grok"} {
		got := Normalize(agent, raw)
		if got.FreshIn != 0 {
			t.Errorf("%s FreshIn = %d, want 0 (clamped)", agent, got.FreshIn)
		}
	}
}

func TestNormalize_UnknownAgentDirectNoPanic(t *testing.T) {
	// Unrecognized agent degrades to direct; never panics.
	raw := RawTokens{Input: 1000, CacheRead: 500, Output: 50}
	var got Canonical
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Normalize panicked on unknown agent: %v", r)
			}
		}()
		got = Normalize("some-future-agent", raw)
	}()
	if got.FreshIn != 1000 {
		t.Errorf("FreshIn = %d, want 1000 (unknown agent → direct)", got.FreshIn)
	}
}

func TestNormalize_VibeInputIsUnsplit(t *testing.T) {
	// Vibe's ACP inputTokens include cached prompt tokens and carry no split.
	// A cache field on the raw record must not become a fresh or cache count.
	raw := RawTokens{Input: 30, CacheRead: 80, CacheWrite: 7, Output: 12}
	got := Normalize("vibe", raw)
	if got.FreshIn != 0 || got.CacheRead != 0 || got.CacheWrite != 0 {
		t.Fatalf("vibe presented a fresh/cache split: fresh=%d cache=%d write=%d", got.FreshIn, got.CacheRead, got.CacheWrite)
	}
	if got.Out != 12 || got.UnsplitIn != 30 {
		t.Fatalf("unsplit/out = %d/%d, want 30/12", got.UnsplitIn, got.Out)
	}
	claude := Normalize("claude", raw)
	if claude.FreshIn != 30 || claude.CacheRead != 80 || claude.CacheWrite != 7 || claude.Out != 12 || claude.UnsplitIn != 0 {
		t.Fatalf("claude fare changed: %+v", claude)
	}
	codex := Normalize("codex", RawTokens{Input: 100, CacheRead: 40, Output: 5})
	if codex.FreshIn != 60 || codex.CacheRead != 40 || codex.Out != 5 || codex.UnsplitIn != 0 {
		t.Fatalf("codex fare changed: %+v", codex)
	}
}

func TestNormalize_VibeDoesNotInheritSubtractOrDirect(t *testing.T) {
	if got, ok := agentFreshRule["vibe"]; !ok || got != unsplit {
		t.Errorf("agentFreshRule[vibe] = %v (present=%v), want unsplit", got, ok)
	}
	raw := RawTokens{Input: 30, CacheRead: 80, Output: 100}
	got := Normalize("vibe", raw)
	if got.FreshIn != 0 || got.CacheRead != 0 || got.UnsplitIn != 30 || got.Out != 100 {
		t.Fatalf("vibe = %+v, want unsplit 30 and output 100", got)
	}
}

func TestNormalize_DshFreshInputDirect(t *testing.T) {
	// dsh reports occupancy (used/size), never a per-turn split, so the fare
	// layer sees no dsh turns today. The rule is still written down rather
	// than left to the unknown-agent default: if a later dsh does emit a
	// split, "direct" must be a decision someone made, not a fallback nobody
	// noticed. Its Input would be fresh-only, as for pi/opencode.
	if got, ok := agentFreshRule["dsh"]; !ok || got != direct {
		t.Errorf("agentFreshRule[dsh] = %v (present=%v), want direct stated explicitly", got, ok)
	}
	raw := RawTokens{Input: 30, CacheRead: 80, Output: 100}
	if got := Normalize("dsh", raw); got.FreshIn != 30 {
		t.Errorf("FreshIn = %d, want 30 (direct; subtract would be −50)", got.FreshIn)
	}
}

func TestFareTotalIncludesUnsplitInput(t *testing.T) {
	// Split agents leave UnsplitIn at zero, so Total stays the four-field sum.
	split := FareTotals{FreshIn: 10, CacheRead: 4, CacheWrite: 1, Out: 3}
	if split.Total() != 18 || split.UnsplitIn != 0 {
		t.Fatalf("split total = %d unsplit = %d, want 18 and 0", split.Total(), split.UnsplitIn)
	}
	// Vibe's stored input is unsplit. Total counts it once, beside output,
	// and does not invent a fresh or cache quantity.
	vibe := Normalize("vibe", RawTokens{Input: 30, Output: 12, CacheRead: 80})
	got := FareTotals{UnsplitIn: vibe.UnsplitIn, Out: vibe.Out}
	if got.Total() != 42 || got.FreshIn != 0 || got.CacheRead != 0 {
		t.Fatalf("vibe total = %+v, want unsplit 30 + out 12", got)
	}
}
