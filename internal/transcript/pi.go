package transcript

// pi.go — native pi JSONL re-source (fare-design.md Phase 4 / D4).
//
// pi's ACP path yields no usage (Tier C). The reliable source is the native
// session file under ~/.pi/agent/sessions/…, joined via the pi-acp wrapper's
// session-map.json (sessionId → sessionFile). This package only resolves and
// parses; projection into the session-log store lives in app (sibling to the
// Claude transcript mirror).
//
// Defensive by contract (AGENTS.md): missing map / missing file / unknown
// shapes never error — callers degrade to no fare.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
)

// PiUsage is one billable turn extracted from a pi native type:"message"
// record. Field mapping (fare-design.md §2.2 / §2.3):
//
//	usage.input       → InputTokens          (fresh-only; Normalize direct)
//	usage.output      → OutputTokens
//	usage.cacheRead   → CachedReadTokens
//	usage.cacheWrite  → CacheCreationTokens
//	usage.reasoning   → ReasoningTokens      (stored; excluded from the four — D7)
//	usage.totalTokens → TotalTokens
//	usage.cost.total  → CostAmount (+ CostCurrency "USD")
//	message.model     → Model
//	record id         → TurnID               (dedup identity, D3)
type PiUsage struct {
	InputTokens         int
	OutputTokens        int
	CachedReadTokens    int
	CacheCreationTokens int
	ReasoningTokens     int
	TotalTokens         int
	CostAmount          float64
	CostCurrency        string
	Model               string
	TurnID              string
}

// ResolvePiSessionFile looks up sessionID in a pi-acp session-map.json and
// returns the absolute sessionFile path. ok is false on any problem (missing
// map, corrupt JSON, absent id, empty path) — never an error. A path that
// points at a missing file still returns ok=true so the caller can degrade
// on open without treating "stale map" as "unknown session".
//
// Map shape (written by pi-acp):
//
//	{"version":1,"sessions":{"<id>":{"sessionId","cwd","sessionFile","updatedAt"}}}
func ResolvePiSessionFile(mapPath, sessionID string) (string, bool) {
	if mapPath == "" || sessionID == "" {
		return "", false
	}
	b, err := os.ReadFile(mapPath)
	if err != nil {
		return "", false
	}
	var root struct {
		Sessions map[string]struct {
			SessionFile string `json:"sessionFile"`
		} `json:"sessions"`
	}
	if json.Unmarshal(b, &root) != nil || root.Sessions == nil {
		return "", false
	}
	ent, ok := root.Sessions[sessionID]
	if !ok || ent.SessionFile == "" {
		return "", false
	}
	return ent.SessionFile, true
}

// ParsePiUsageLine extracts fare fields from one pi native JSONL line.
// Only type:"message" records with a usage block yield ok=true; everything
// else (session/model_change/user/no-usage/garbage) is silently ignored.
func ParsePiUsageLine(line []byte) (PiUsage, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return PiUsage{}, false
	}
	var rec struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Message struct {
			Role  string `json:"role"`
			Model string `json:"model"`
			Usage *struct {
				Input       int `json:"input"`
				Output      int `json:"output"`
				CacheRead   int `json:"cacheRead"`
				CacheWrite  int `json:"cacheWrite"`
				Reasoning   int `json:"reasoning"`
				TotalTokens int `json:"totalTokens"`
				Cost        *struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return PiUsage{}, false
	}
	if rec.Type != "message" || rec.Message.Usage == nil {
		return PiUsage{}, false
	}
	u := rec.Message.Usage
	// A usage object with every token field zero and no cost is still a
	// structured usage record; accept it so zero-cost local models project.
	// Callers that care about "has fare data" apply their own filter.
	out := PiUsage{
		InputTokens:         u.Input,
		OutputTokens:        u.Output,
		CachedReadTokens:    u.CacheRead,
		CacheCreationTokens: u.CacheWrite,
		ReasoningTokens:     u.Reasoning,
		TotalTokens:         u.TotalTokens,
		Model:               rec.Message.Model,
		TurnID:              rec.ID,
	}
	if u.Cost != nil {
		out.CostAmount = u.Cost.Total
		out.CostCurrency = "USD"
	}
	// Require at least one billable or identity signal so empty assistant
	// shells without usage-shaped numbers are ignored.
	if out.InputTokens == 0 && out.OutputTokens == 0 && out.CachedReadTokens == 0 &&
		out.CacheCreationTokens == 0 && out.TotalTokens == 0 && out.ReasoningTokens == 0 &&
		out.CostAmount == 0 {
		return PiUsage{}, false
	}
	return out, true
}

// PiTailer incrementally reads one pi native session file and yields new
// usage-bearing message records since the last Poll. Safe for concurrent use.
// Missing/unreadable files yield no events (degrade, never error).
type PiTailer struct {
	Path string

	mu     sync.Mutex
	offset int64
	buf    []byte
}

// Poll advances the file and returns newly seen PiUsage records in order.
// An unchanged file (or a missing one) returns nil.
func (t *PiTailer) Poll() []PiUsage {
	if t == nil || t.Path == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := os.Open(t.Path)
	if err != nil {
		return nil
	}
	defer f.Close()

	if t.offset > 0 {
		if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
			return nil
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	chunk := append(t.buf, data...)
	t.buf = nil

	var out []PiUsage
	start := 0
	for i := 0; i < len(chunk); i++ {
		if chunk[i] != '\n' {
			continue
		}
		line := chunk[start:i]
		start = i + 1
		if u, ok := ParsePiUsageLine(line); ok {
			out = append(out, u)
		}
	}
	// Keep a partial trailing line buffered; offset advances past all bytes
	// read so the next seek+prepend reconstructs the completed line.
	if start < len(chunk) {
		t.buf = append([]byte(nil), chunk[start:]...)
	}
	t.offset += int64(len(data))
	return out
}

// Reset clears the watermark so the next Poll re-reads from the start.
// Used when the bound native path changes (/clear → new session file).
func (t *PiTailer) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.offset = 0
	t.buf = nil
}
