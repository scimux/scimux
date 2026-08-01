// state_api.go — /api/state view projection and system metrics.
// Snapshots poll/liveness state for the UI; does not mutate live, attention,
// or other poll-owned maps. Session-log segment ownership stays in main.go;
// poll writes stay in poller.go.
package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// ---------- sysload ----------

// sysInfo carries normalized numbers; the browser only renders values and
// trends (no Linux parsing rules in JavaScript). Sampled at most every 30 s
// so the state ETag stays stable between sys refreshes on an idle system.
type sysInfo struct {
	Load1      float64 `json:"load1"`
	NCPU       int     `json:"ncpu"`
	MemPct     float64 `json:"mem_pct"`
	MemTotalGB float64 `json:"mem_total_gb"`
	SwapPct    float64 `json:"swap_pct"`
}

var (
	sysMu      sync.Mutex
	sysCache   sysInfo
	sysCacheAt time.Time
)

func sysload() sysInfo {
	sysMu.Lock()
	defer sysMu.Unlock()
	if time.Since(sysCacheAt) < 30*time.Second {
		return sysCache
	}
	out := sysInfo{NCPU: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 1 {
			fmt.Sscanf(f[0], "%f", &out.Load1)
		}
	}
	var totalKB, availKB, swapTotalKB, swapFreeKB float64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			var v float64
			if _, err := fmt.Sscanf(line, "MemTotal: %f kB", &v); err == nil {
				totalKB = v
			}
			if _, err := fmt.Sscanf(line, "MemAvailable: %f kB", &v); err == nil {
				availKB = v
			}
			if _, err := fmt.Sscanf(line, "SwapTotal: %f kB", &v); err == nil {
				swapTotalKB = v
			}
			if _, err := fmt.Sscanf(line, "SwapFree: %f kB", &v); err == nil {
				swapFreeKB = v
			}
		}
	}
	if totalKB > 0 {
		out.MemPct = 100 * (totalKB - availKB) / totalKB
		out.MemTotalGB = totalKB / 1024 / 1024
	}
	if swapTotalKB > 0 {
		out.SwapPct = 100 * (swapTotalKB - swapFreeKB) / swapTotalKB
	}
	sysCache, sysCacheAt = out, time.Now()
	return out
}

// ---------- HTTP state projection ----------

type nodeView struct {
	*Node
	Live            string `json:"live"`
	Attention       string `json:"attention,omitempty"` // "approval" | "question" | "inspect" (quiet, no structured evidence — look at the terminal)
	HasTranscript   bool   `json:"has_transcript"`
	LastActivity    int64  `json:"last_activity,omitempty"`    // unix ms of last pane/log movement
	LastInteraction int64  `json:"last_interaction,omitempty"` // unix ms of last user turn or page-turn
	// CtxPct is the live context occupancy (segment-scoped, 0-100), projected
	// onto the list only for live nodes (active/quiet) — the map's at-a-glance
	// congestion gauge. A pointer so absent (dead node / no usage yet) is
	// distinct from a genuine 0%.
	CtxPct *int `json:"ctx_pct,omitempty"`
	// Stops are the timestamps of the node's /clear page-turns (clear-tagged
	// seams only), oldest first. The metro map prepends the node's creation to
	// get the full stop chain: creation plus each /clear is one station.
	Stops []string `json:"stops,omitempty"`
	// StationLabels maps a stop's start-time key to its frozen name/description
	// snapshot. The map renders a stop with its own label; a stop with no entry
	// (the head, or a legacy pre-snapshot stop) falls back to the node's live
	// Title/Description. Keyed exactly as the client builds the stop chain
	// (created_at + each Stops entry), so no parsing is needed to match.
	StationLabels map[string]sessionlog.StationLabel `json:"station_labels,omitempty"`
}

func unixMSStamp(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func lastInteractionMS(n *Node, seg sessionlog.Segment) int64 {
	for i := len(seg.Turns) - 1; i >= 0; i-- {
		t := seg.Turns[i]
		if t.Role == "user" {
			if ms := unixMSStamp(t.Time); ms > 0 {
				return ms
			}
		}
	}
	if ms := unixMSStamp(seg.StartTime); ms > 0 {
		return ms
	}
	return unixMSStamp(n.CreatedAt)
}

// Snapshot boundary for handleState (existing behavior; do not change locking
// merely to match this comment):
//
//   - tmux session listing (a.server.Sessions) runs before a.mu.
//   - scalar Node values, poll fields (live/attention/lastChg), HasTranscript,
//     and the unadopted list are snapshotted under a.mu.
//   - live *Node pointers must never be marshaled after unlock (copy under lock).
//   - segment/session-log projection (stops, station labels, last interaction,
//     context gauge) runs after unlock because segment reacquires a.mu.
//   - JSON and ETag emission is read-only and must not mutate liveness or poll
//     state.
func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	sessions := a.server.Sessions()
	a.mu.Lock()
	views := make([]nodeView, 0, len(a.nodes))
	for _, n := range a.nodes {
		var lastMS int64
		if t, ok := a.lastChg[n.ID]; ok {
			lastMS = t.UnixMilli()
		}
		// Copy the node while holding the lock: marshaling a live *Node after
		// unlock races the poller's Transcript writes (a Go data race).
		nc := *n
		// Structured-protocol nodes (ACP, codex) keep their history in the
		// manager's session log rather than a linked transcript file, so they
		// always have chat to show.
		hasTranscript := n.Transcript != "" || a.proc(n) != nil
		views = append(views, nodeView{Node: &nc, Live: a.live[n.ID], Attention: a.attn[n.ID],
			HasTranscript: hasTranscript, LastActivity: lastMS})
	}
	// Sessions on our socket that no node accounts for: candidates for
	// adoption (manually created, or migrated from another tmux server). A
	// session whose id is reserved by an in-flight create is already spoken
	// for (R20.2) — offering it for adoption would race the publish.
	unadopted := []string{}
	for _, s := range sessions {
		if _, known := a.byID[s]; !known && !a.reserved[s] {
			unadopted = append(unadopted, s)
		}
	}
	a.mu.Unlock()
	// Project live context occupancy onto the list — only for live nodes, and
	// only after the unlock (a.segment takes a.mu itself, so calling it inside
	// the loop above would deadlock). segment() is cache-backed, so a quiet
	// node whose log is unchanged re-parses nothing.
	for i := range views {
		v := &views[i]
		// Every node reports its stop chain (the map draws stations for dead
		// threads too); segment() is cache-backed, so this costs a stat per
		// node while the log is unchanged.
		seg := a.segment(v.Node)
		v.Stops = seg.ClearTimes
		if len(seg.Stations) > 0 {
			v.StationLabels = seg.Stations
		}
		v.LastInteraction = lastInteractionMS(v.Node, seg)
		if v.Live == "exited" || v.Live == "unavailable" {
			continue // gauge is live-only
		}
		win := ctxWindowFor(seg.Used, seg.Size, v.Node.Model)
		if win <= 0 {
			continue // no usage reported yet — no gauge, don't fake a 0
		}
		p := ctxPctOf(seg.Used, win)
		v.CtxPct = &p
	}
	body, err := json.Marshal(map[string]any{
		"nodes": views, "unadopted": unadopted, "sys": sysload(),
		"socket": a.server.Socket, "hostname": hostname, "version": version,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// ETag short-circuit: an unchanged state costs the client a few header
	// bytes instead of a body — polling stays, but nearly free when idle.
	h := fnv.New64a()
	h.Write(body)
	etag := fmt.Sprintf(`"%x"`, h.Sum64())
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// ctxWindowFor returns the context window, estimating it from the model name
// when the transcript reports usage but not a window size — Claude reports
// usage but never the window; the "[1m]" marker is the long-context variant.
func ctxWindowFor(used, window int64, model string) int64 {
	if used > 0 && window == 0 {
		if strings.Contains(model, "[1m]") {
			return 1_000_000
		}
		return 200_000
	}
	return window
}

// ctxPctOf is the clamped context-occupancy percent (0 when the window is
// unknown, so a missing window never fabricates a reading).
func ctxPctOf(used, window int64) int {
	if window <= 0 {
		return 0
	}
	p := int(100 * used / window)
	if p > 100 {
		p = 100
	}
	return p
}
