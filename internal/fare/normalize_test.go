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
