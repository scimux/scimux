package transcript

// pi_models.go — resolve context-window sizes from pi's own models.json
// (fare-design.md Phase 4a).
//
// pi advertises no context window over ACP or in its native session JSONL.
// It does list one per model in ~/.pi/agent/models.json (id → contextWindow),
// including local ollama/lmstudio endpoints. Reading that file is the
// grounded, per-user source for the occupancy tank Size — not a scimux-
// maintained table (which would go stale and miss local models).
//
// Join field (verified live, Phase 4a Step 0): native usage records put
// message.model equal to the models.json entry's `id` (e.g. "qwen27:latest",
// "smollm2:1.7b"), not `name` ("qwen27", "smollm2"). Match on id.
//
// Defensive (AGENTS.md): missing/corrupt file → empty map, no error.
// Duplicate ids across providers → first-wins, no error. Cost rates in the
// file are never read (D8: reported cost only from native cost.total).

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// LoadPiModelWindows parses path as pi's models.json and returns
// model-id → contextWindow. Missing/unreadable/corrupt files yield an empty
// (non-nil) map. Duplicate ids keep the first seen window (document order).
func LoadPiModelWindows(path string) map[string]int {
	out := map[string]int{}
	if path == "" {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	return parsePiModelsJSON(b)
}

// parsePiModelsJSON walks providers.*.models[] in JSON document order and
// collects id → contextWindow. First-wins on duplicate ids (stable across
// runs — not Go map iteration order). contextWindow ≤ 0 is still recorded
// (Window treats 0 as "unknown").
func parsePiModelsJSON(b []byte) map[string]int {
	out := map[string]int{}
	var root struct {
		Providers json.RawMessage `json:"providers"`
	}
	if json.Unmarshal(b, &root) != nil || len(root.Providers) == 0 {
		return out
	}
	// Decode the providers object with a streaming decoder so key order is
	// the file order (encoding/json.Map would randomize first-wins).
	dec := json.NewDecoder(bytes.NewReader(root.Providers))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return out
	}
	type modelEnt struct {
		ID            string `json:"id"`
		ContextWindow int    `json:"contextWindow"`
	}
	type provEnt struct {
		Models []modelEnt `json:"models"`
	}
	for dec.More() {
		if _, err := dec.Token(); err != nil { // provider key
			return out
		}
		var prov provEnt
		if err := dec.Decode(&prov); err != nil {
			// Skip a corrupt provider value if possible; otherwise stop.
			// Decode already consumed or failed — defensive empty for rest.
			return out
		}
		for _, m := range prov.Models {
			if m.ID == "" {
				continue
			}
			if _, exists := out[m.ID]; exists {
				continue // first-wins
			}
			out[m.ID] = m.ContextWindow
		}
	}
	return out
}

// PiModels is a mtime-cached view of models.json. Safe for concurrent use.
// Path is required; callers (app) supply the default
// <home>/.pi/agent/models.json or a test override.
type PiModels struct {
	Path string

	mu    sync.Mutex
	mtime time.Time
	byID  map[string]int
	// loaded is true after at least one successful Stat+read attempt
	// (including "file missing" → empty map), so we do not re-Stat on every
	// call when the file never appears; mtime zero + loaded still rechecks
	// when the file materializes.
	loaded bool
	// lastStat is when we last checked; throttle missing-file rechecks.
	// For simplicity we re-Stat every Window call — models.json is tiny and
	// Window is only hit on usage projection (per new turn), not per poll
	// byte. Revisit if profiling says otherwise.
}

// Window returns contextWindow for modelID, or 0 if unknown / missing config /
// contextWindow was 0. Reloads when the file's mtime advances (user edit).
func (p *PiModels) Window(modelID string) int {
	if p == nil || p.Path == "" || modelID == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloadLocked()
	if p.byID == nil {
		return 0
	}
	return p.byID[modelID]
}

func (p *PiModels) reloadLocked() {
	st, err := os.Stat(p.Path)
	if err != nil {
		// Missing/unreadable: empty map. Keep loaded so we still re-Stat next
		// call (file may appear later) but avoid holding a stale map if the
		// file was deleted after a successful load.
		if p.loaded && !p.mtime.IsZero() {
			// Was present, now gone — clear.
			p.mtime = time.Time{}
			p.byID = map[string]int{}
		} else if !p.loaded {
			p.byID = map[string]int{}
			p.loaded = true
		}
		return
	}
	mt := st.ModTime()
	if p.loaded && mt.Equal(p.mtime) && p.byID != nil {
		return
	}
	p.byID = LoadPiModelWindows(p.Path)
	p.mtime = mt
	p.loaded = true
}
