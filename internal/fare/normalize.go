// Package fare holds pure token-fare logic: per-agent normalization of raw
// usage fields into the four canonical billable quantities.
//
// No I/O, no sessionlog/acp imports — later phases (ReadFare) adapt stored
// records into RawTokens and call Normalize. See fare-design.md §2.3 and D2.
package fare

// RawTokens are the near-raw per-turn token fields as stored by a transport.
// Semantics of Input depend on the agent (§2.3); Normalize encodes that.
type RawTokens struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Canonical is the four billable per-turn quantities after §2.3 normalization.
// These are the only fields summed for fare; occupancy (used) is never here.
type Canonical struct {
	FreshIn    int // full-price input
	CacheRead  int // discounted cached input
	CacheWrite int // cache-creation
	Out        int // output
}

// freshRule decides how Input maps to FreshIn for a given agent.
type freshRule int

const (
	// direct: Input is already fresh-only (claude, pi, opencode, unknown).
	direct freshRule = iota
	// subtractCache: Input is whole-prompt → FreshIn = Input − CacheRead (≥0).
	subtractCache
)

// agentFreshRule keys the §2.3 table on meta.agent (stable per session file, D2).
// Unknown agents degrade to direct — safe default, never panic.
var agentFreshRule = map[string]freshRule{
	"claude":   direct,
	"pi":       direct,
	"opencode": direct,
	"codex":    subtractCache,
	"grok":     subtractCache,
}

// Normalize maps one turn's raw stored token fields + agent name to the four
// canonical billable quantities (fare-design.md §2.3, D2).
//
// Only FreshIn differs by agent:
//
//	claude, pi, opencode, unknown → direct (Input is already fresh-only)
//	codex, grok                   → Input − CacheRead, clamped ≥ 0
//
// CacheRead, CacheWrite, and Out are pass-throughs of the raw fields.
// The agent key is the file's meta.agent (stable per file); unknown agents
// use direct and never error or panic.
func Normalize(agent string, raw RawTokens) Canonical {
	fresh := raw.Input
	if agentFreshRule[agent] == subtractCache {
		fresh = raw.Input - raw.CacheRead
		if fresh < 0 {
			fresh = 0
		}
	}
	return Canonical{
		FreshIn:    fresh,
		CacheRead:  raw.CacheRead,
		CacheWrite: raw.CacheWrite,
		Out:        raw.Output,
	}
}
