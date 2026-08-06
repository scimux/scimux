package app

// pi_fare.go — project pi native JSONL usage into the unified session log
// (fare-design.md Phase 4 / D4). Sibling to the Claude transcript mirror
// (mirror.go): same store, same source-seam mechanics, same UsageEvent schema.
//
// Join key (never guess the path):
//
//	node ACP sessionId → ~/.pi/pi-acp/session-map.json → sessionFile
//
// Wired from poll() for agent=="pi" structured-transport nodes. Missing map,
// missing file, or unreadable native content degrades to no fare — never
// panics. Append-only; /clear (O1) yields a new sessionId → new native file →
// new source seam, then usage from the fresh file only.

import (
	"os"
	"path/filepath"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/transcript"
)

// piFareMirror is one pi node's native-usage projection state. Owned by the
// poller goroutine (same ownership as mirror).
type piFareMirror struct {
	logw   *sessionlog.Writer
	path   string // native sessionFile currently bound
	sid    string // ACP sessionId currently bound
	tailer *transcript.PiTailer
	// seenTurnIDs prevents double-append within a process lifetime when a
	// re-open re-reads (belt-and-suspenders with TurnID dedup in ReadFare).
	seenTurnIDs map[string]bool
}

// piSessionMapPath is the pi-acp join key on disk. Overridable via a.piMapPath
// for tests; default is <home>/.pi/pi-acp/session-map.json.
func (a *app) piSessionMapPath() string {
	if a.piMapPath != "" {
		return a.piMapPath
	}
	return filepath.Join(a.home, ".pi", "pi-acp", "session-map.json")
}

// liveSessionID prefers the live ACP session id (correct after /clear) and
// falls back to the durable Node.SessionID.
func (a *app) liveSessionID(n *Node) string {
	if n == nil {
		return ""
	}
	if a.acp.Manager != nil {
		if live := a.acp.SessionID(n.ID); live != "" {
			return live
		}
	}
	return n.SessionID
}

// syncPiFare advances a pi node's session log with usage from the native
// JSONL transcript. Called once per poll tick for agent=="pi" nodes. Every
// step is incremental and cheap when nothing changed.
func (a *app) syncPiFare(n *Node) {
	if a == nil || n == nil || a.sessionsDir == "" {
		return
	}
	if n.Agent != "pi" {
		return
	}
	sid := a.liveSessionID(n)
	if sid == "" {
		return
	}
	// Note: we deliberately do NOT write n.SessionID here — this runs on the
	// poller goroutine without a.mu, and handleSend already persists the
	// post-/clear id under the lock. syncPiFare only needs the local sid for
	// the session-map join.

	mapPath := a.piSessionMapPath()
	native, ok := transcript.ResolvePiSessionFile(mapPath, sid)
	if !ok || native == "" {
		return // missing/stale map: no fare, never crash
	}

	a.mu.Lock()
	if a.piMirrors == nil {
		a.piMirrors = map[string]*piFareMirror{}
	}
	m := a.piMirrors[n.ID]
	if m == nil {
		m = &piFareMirror{seenTurnIDs: map[string]bool{}}
		a.piMirrors[n.ID] = m
	}
	logPath := a.sessionLogPath(n.ID)
	a.mu.Unlock()

	if m.logw == nil || m.logw.Path != logPath {
		m.logw = &sessionlog.Writer{Path: logPath}
		// Recover durable state: last native path/session + already-seen turn ids
		// so a restart does not re-append usage (TurnID dedup still protects
		// ReadFare, but we keep the log clean).
		st := replayPiFareState(logPath)
		if !st.hasMeta {
			if err := m.logw.Append(sessionlog.NewMeta(n.ID, n.Agent, n.Model, n.Effort, n.Dir)); err != nil {
				m.logw = nil
				return
			}
		}
		m.path, m.sid = st.path, st.sid
		m.seenTurnIDs = st.seen
		if m.seenTurnIDs == nil {
			m.seenTurnIDs = map[string]bool{}
		}
		// On restart with the same native file, re-Poll from offset 0 and
		// drop against seenTurnIDs (ReadFare also TurnID-dedups).
		if m.path != "" && m.path == native {
			m.tailer = &transcript.PiTailer{Path: native}
		}
	}

	// Source seam when the bound native file or session id changes (first
	// bind, or /clear → new sessionId → new file per O1).
	if native != m.path || sid != m.sid {
		if err := m.logw.Append(sessionlog.NewSource(native, sid)); err != nil {
			return
		}
		m.path, m.sid = native, sid
		m.tailer = &transcript.PiTailer{Path: native}
		// Do not clear seenTurnIDs: a re-bind of a previously mirrored file
		// (unlikely) must not re-append; a brand-new file has new turn ids.
	}
	if m.tailer == nil || m.tailer.Path != native {
		m.tailer = &transcript.PiTailer{Path: native}
	}

	// Missing/unreadable native file: Poll degrades to nil.
	for _, u := range m.tailer.Poll() {
		if u.TurnID != "" && m.seenTurnIDs[u.TurnID] {
			continue
		}
		ev := sessionlog.Event{T: "usage", Usage: &sessionlog.UsageEvent{
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
			// No Used/Size: pi advertises no context window (neither ACP
			// initialize nor the native store carries one), so pi has a fare
			// meter but no occupancy tank — the two gauges are independent
			// by design (D1). Do not synthesize a tank from spend-shaped
			// totalTokens; a real pi tank needs a window source it lacks.
		}}
		if err := m.logw.Append(ev); err != nil {
			return // retry next tick
		}
		if u.TurnID != "" {
			m.seenTurnIDs[u.TurnID] = true
		}
	}
}

// piFareState is what replayPiFareState recovers from an existing log.
type piFareState struct {
	hasMeta bool
	path    string
	sid     string
	seen    map[string]bool
}

// replayPiFareState derives the pi fare mirror's resume point from the log.
func replayPiFareState(logPath string) piFareState {
	st := piFareState{seen: map[string]bool{}}
	if _, err := os.Stat(logPath); err != nil {
		return st
	}
	for _, ev := range sessionlog.ReadEvents(logPath) {
		switch ev.T {
		case "meta":
			st.hasMeta = true
		case "source":
			if ev.Source != nil {
				// Keep the latest source with a path (native bind); a
				// path-less clear seam still carries the new sessionId.
				if ev.Source.Path != "" {
					st.path = ev.Source.Path
				}
				if ev.Source.SessionID != "" {
					st.sid = ev.Source.SessionID
				}
			}
		case "usage":
			if ev.Usage != nil && ev.Usage.TurnID != "" {
				st.seen[ev.Usage.TurnID] = true
			}
		}
	}
	return st
}
