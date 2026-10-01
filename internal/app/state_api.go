// state_api.go — /api/state view projection and system metrics.
// Snapshots poll/liveness state for the UI; does not mutate live, attention,
// or other poll-owned maps. Session-log segment ownership stays in main.go;
// poll writes stay in poller.go.
package app

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
	"github.com/scimux/scimux/internal/storagebudget"
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
	Live        string `json:"live"`
	Attention   string `json:"attention,omitempty"`    // "approval" | "question" | "dialog" | "inspect"
	AttentionAt int64  `json:"attention_at,omitempty"` // unix ms of the fresh evidence that raised the current attention
	// Supervision is Claude-only: starting | strict | unsupported | failed.
	// Other agents omit it. The UI uses it to refuse inspect/fallback peeks.
	Supervision string `json:"supervision,omitempty"`
	LaunchError string `json:"launch_error,omitempty"`
	// TurnDone: the agent has finished its turn and is waiting for the human
	// (quiet + delivered + no pending tool + no attention on tmux; quiet +
	// successful active→quiet on structured). omitempty so absent means
	// UNKNOWN — never-ran structured nodes still omit it.
	TurnDone        bool  `json:"turn_done,omitempty"`
	HasTranscript   bool  `json:"has_transcript"`
	LastActivity    int64 `json:"last_activity,omitempty"`    // unix ms of last pane/log movement
	LastInteraction int64 `json:"last_interaction,omitempty"` // unix ms of last user turn or page-turn
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
	// Fare fields — whole-journey token meter (fare-design.md Phase 7, D1).
	// Distinct from CtxPct (segment-scoped tank). Omitted entirely when
	// ReadFare.Turns==0 (absent ≠ zero). fare_cost only meaningful with
	// fare_cost_complete (D8). Pointers so a genuine 0 still serializes when
	// fare is available.
	FareFreshIn      *int     `json:"fare_fresh_in,omitempty"`
	FareCacheRead    *int     `json:"fare_cache_read,omitempty"`
	FareCacheWrite   *int     `json:"fare_cache_write,omitempty"`
	FareOut          *int     `json:"fare_out,omitempty"`
	FareUnsplitIn    *int     `json:"fare_unsplit_in,omitempty"`
	FareTotal        *int     `json:"fare_total,omitempty"`
	FareTurns        *int     `json:"fare_turns,omitempty"`
	FareCost         *float64 `json:"fare_cost,omitempty"`
	FareCostComplete *bool    `json:"fare_cost_complete,omitempty"`
	FareModel        string   `json:"fare_model,omitempty"`
	// FareSegments is the V2-P2 per-segment ride projection (tokens + time
	// model). Omitted when empty. Split fields (agent/tools/wait) are absent
	// on historical / complete-only segments — real_ms still present.
	// UI (lane heat / capsule) lands in V2-P3/P4; this is data plumbing only.
	FareSegments []fareSegView `json:"fare_segments,omitempty"`
	// MuseSchemaWarning is additive fingerprint-drift notice. Older browsers
	// ignore it. It is never a reason to hide chat or change liveness.
	MuseSchemaWarning string `json:"muse_schema_warning,omitempty"`
	// StorageBytes is this live node's session log, attachments, and asset
	// blobs. Archived history remains in the aggregate storage snapshot.
	StorageBytes    int64 `json:"storage_bytes"`
	StorageWritable bool  `json:"storage_writable"`
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

// handleState uses this snapshot boundary (existing behavior; do not change locking
// merely to match this comment):
//
//   - scalar Node values, poll fields (live/attention/lastChg), HasTranscript,
//     are snapshotted under a.mu.
//   - live *Node pointers must never be marshaled after unlock (copy under lock).
//   - segment/session-log projection (stops, station labels, last interaction,
//     context gauge) runs after unlock because segment reacquires a.mu.
//   - JSON and ETag emission is read-only and must not mutate liveness or poll
//     state.
func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	workerStates := map[string]sessionworker.State{}
	if a.workers != nil {
		a.mu.Lock()
		ids := make([]string, 0, len(a.nodes))
		for _, n := range a.nodes {
			if !n.Adopted && n.Agent == "claude" && a.workers.manages(n.ID) {
				ids = append(ids, n.ID)
			}
		}
		a.mu.Unlock()
		for _, id := range ids {
			workerStates[id] = a.workers.State(id)
		}
	}
	a.mu.Lock()
	views := make([]nodeView, 0, len(a.nodes))
	for _, n := range a.nodes {
		var lastMS int64
		if t, ok := a.lastChg[n.ID]; ok {
			lastMS = t.UnixMilli()
		}
		var attentionMS int64
		if t, ok := a.attnAt[n.ID]; ok {
			attentionMS = t.UnixMilli()
		}
		// Copy the node while holding the lock: marshaling a live *Node after
		// unlock races the poller's Transcript writes (a Go data race).
		nc := *n
		// Structured-protocol nodes (ACP, codex, Muse) keep their history in the
		// manager's session log rather than a linked transcript file, so they
		// always have chat to show.
		hasTranscript := n.Transcript != "" || (n.Agent != "claude" && a.proc(n) != nil)
		sup := a.claudeSupervisionOf(n)
		attn := a.attn[n.ID]
		live, turnDone, launchErr := a.live[n.ID], a.turnDone[n.ID], a.claudeLaunchErr[n.ID]
		if n.Adopted {
			live, attn, turnDone = "unavailable", "", false
			launchErr = "This external session integration is retired. Saved history remains available; start a fresh chat to continue."
		} else if state, ok := workerStates[n.ID]; ok {
			nc.SessionID, nc.Transcript, nc.AXScreenReader = state.SessionID, state.Transcript, state.AXScreenReader
			sup, attn, live, turnDone, launchErr = claudeSupervision(state.Supervision), state.Attention, state.Live, state.TurnDone, state.LastError
			hasTranscript = state.Transcript != ""
		}
		if n.Agent == "claude" && attn == "inspect" {
			attn = ""
		}
		if n.Agent == "claude" && sup != claudeSupStrict && attn != "" {
			attn = ""
		}
		views = append(views, nodeView{Node: &nc, Live: live, Attention: attn, AttentionAt: attentionMS,
			TurnDone: turnDone, HasTranscript: hasTranscript, LastActivity: lastMS,
			Supervision: string(sup), LaunchError: launchErr})
	}
	hosted := a.hostedRemote
	a.mu.Unlock()
	for i := range views {
		n := views[i].Node
		if n.Agent == "muse" {
			pm := a.proc(n)
			if fp, ok := pm.(museFingerprintReporter); ok && fp.FingerprintMismatch(n.ID) {
				views[i].MuseSchemaWarning = museSchemaWarning
			}
		}
	}
	// Project live context occupancy onto the list — only for live nodes, and
	// only after the unlock (a.segment takes a.mu itself, so calling it inside
	// the loop above would deadlock). segment() is cache-backed, so a quiet
	// node whose log is unchanged re-parses nothing.
	//
	// Fare (journey meter) is projected for every node, also cache-backed
	// (shared LogCache: tail parse on growth, hit-list re-fold for fare). It
	// is whole-journey ReadFare — never segment history and never merged into
	// ctx_pct (D1). Missing/empty fare → fields omitted.
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
		// Fare meter: independent of live state (journey total still meaningful
		// on exited threads). applyFare no-ops when Turns==0. Per-segment rides
		// (V2-P2) share the same LogCache walk as segment/assets.
		f, rides := a.fareAndRides(v.Node)
		applyFare(v, f)
		applyFareRides(v, rides)
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
	storage := a.storageStatus()
	for i := range views {
		views[i].StorageBytes = storage.Nodes[views[i].ID]
		views[i].StorageWritable = storage.Writable &&
			(storage.NodeLimitBytes == 0 || views[i].StorageBytes < storage.NodeLimitBytes)
	}
	payload := map[string]any{
		"nodes": views, "sys": sysload(),
		"socket": a.server.Socket, "hostname": hostname, "version": a.activeWebVersion(),
		"storage": storage,
	}
	if hosted != nil {
		payload["remote"] = map[string]string{"status": hosted.HostedStatus()}
	}
	body, err := json.Marshal(payload)
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

func (a *app) storageStatus() storagebudget.Status {
	a.storageMu.Lock()
	defer a.storageMu.Unlock()
	if time.Since(a.storageAt) < 30*time.Second && a.storageSnapshot.Nodes != nil {
		return a.storageSnapshot
	}
	dataDir := filepath.Dir(a.sessionsDir)
	status, err := storagebudget.Inspect(dataDir)
	if err != nil {
		status = storagebudget.Status{Writable: false, Reason: err.Error(), Nodes: map[string]int64{}}
	}
	a.storageSnapshot, a.storageAt = status, time.Now()
	return status
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
