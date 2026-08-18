// claude_asked.go — escalation notices: the PermissionRequest hook as an
// attention discriminator.
//
// The hook fires for every decision Claude needs a human for (proven live in
// P0: permissions, subagent calls, AskUserQuestion and ExitPlanMode alike). It
// therefore knows something no amount of transcript reasoning can recover:
// whether a dialog is about to be drawn. An unresolved tool call on disk is
// present both when a long tool is simply running and when a dialog sits on
// screen with parallel calls animating behind it — Claude flushes the call
// record for the *approved* call A while call B, still waiting, has written
// nothing at all. Pane geometry cannot separate those two states either; both
// are a confined animation over a stalled transcript.
//
// So the helper writes a notice for every decision it escalates, whether or not
// a lease is armed. A notice is not an answer: authorization stays gated on the
// arm marker exactly as before, and this file adds no path that can approve
// anything. What it adds is the ability to say "Claude asked nothing", which is
// what makes suppressing a false attention safe.
//
// The gate is deliberately narrow. It applies only to the active-pane paths and
// only for a bundle that proves the notice layout on disk; everything else —
// the quiet-pane fallback, adopted panes, pre-feature bundles, every other
// agent — keeps its existing behaviour. Rate-limit menus, trust-folder and
// login prompts fire no hook, and those are exactly the prompts that reach a
// quiet pane, so they must never be gated.
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"codeberg.org/chrberger/scimux/internal/transcript"
)

// claudeAskedTTL bounds how long a notice may stand. It is a leak backstop,
// not the retirement policy: a notice is normally retired the moment the
// helper auto-approves its call, or when the transcript records the work that
// only an answered dialog can produce. A dialog may
// legitimately wait hours for a human, so this is generous on purpose — the
// failure this whole design exists to prevent is a wait going unnoticed, and a
// notice that outstays its dialog costs only a look at the terminal.
const claudeAskedTTL = 4 * time.Hour

// claudeAskedNotice is what the hook leaves behind for every escalated
// decision. Deliberately not an audit record: a disarmed session writes one of
// these per decision, so the raw tool_input stays out of it and only a digest
// identifies the call. The armed path audits the full input separately, where
// the human has consented by arming.
type claudeAskedNotice struct {
	At       string `json:"at"`
	Session  string `json:"session"`
	Tool     string `json:"tool"`
	AgentID  string `json:"agent_id,omitempty"`
	PromptID string `json:"prompt_id,omitempty"`
	Digest   string `json:"digest"`
}

// claudeAskedDirName is the notice subdirectory inside perm/.
const claudeAskedDirName = "asked"

func claudeAskedPath(perm, id string) string {
	return filepath.Join(perm, claudeAskedDirName, id+".json")
}

// newClaudeAskedNotice projects a hook payload into a notice.
func newClaudeAskedNotice(ev claudePermissionEvent, now time.Time) claudeAskedNotice {
	sum := sha256.Sum256(ev.ToolInput)
	return claudeAskedNotice{
		At:       now.UTC().Format(time.RFC3339Nano),
		Session:  ev.SessionID,
		Tool:     ev.ToolName,
		AgentID:  ev.AgentID,
		PromptID: ev.PromptID,
		Digest:   hex.EncodeToString(sum[:])[:16],
	}
}

// writeClaudeAskedNotice records one escalation. Best-effort by design: the
// helper never creates a missing bundle directory (same rule as the
// SessionStart helper), so a bundle prepared before this feature simply gets
// no notices — and therefore no gate — rather than an error.
func writeClaudeAskedNotice(perm, id string, ev claudePermissionEvent, now time.Time) {
	if perm == "" || id == "" {
		return
	}
	if st, err := os.Stat(filepath.Join(perm, claudeAskedDirName)); err != nil || !st.IsDir() {
		return
	}
	_ = writeClaudePermFile(claudeAskedPath(perm, id), newClaudeAskedNotice(ev, now))
}

// removeClaudeAskedNotice retires one notice. Called after the helper has
// printed its allow: that call never reaches a dialog, so its notice must not
// stand or every auto-approval would raise attention.
func removeClaudeAskedNotice(perm, id string) {
	if perm == "" || id == "" {
		return
	}
	_ = os.Remove(claudeAskedPath(perm, id))
}

// askedFile pairs a standing notice with the file that holds it, so a caller
// can retire one specific ask.
type askedFile struct {
	path string
	note claudeAskedNotice
	at   time.Time
}

// readClaudeAskedFiles returns the standing notices, newest first, dropping
// (and deleting) anything past the TTL. Defensive like every other transcript
// reader: an unreadable or unparseable file is ignored, never an error.
func readClaudeAskedFiles(perm string, now time.Time) []askedFile {
	dir := filepath.Join(perm, claudeAskedDirName)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []askedFile
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil || len(b) > claudeHookStdinLimit {
			continue
		}
		var note claudeAskedNotice
		if json.Unmarshal(b, &note) != nil || note.Tool == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, note.At)
		if err != nil || now.Sub(at) > claudeAskedTTL {
			_ = os.Remove(p)
			continue
		}
		out = append(out, askedFile{path: p, note: note, at: at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].note.At > out[j].note.At })
	return out
}

func readClaudeAskedNotices(perm string, now time.Time) []claudeAskedNotice {
	files := readClaudeAskedFiles(perm, now)
	out := make([]claudeAskedNotice, 0, len(files))
	for _, f := range files {
		out = append(out, f.note)
	}
	return out
}

// retireOldestAskedNotice retires exactly one ask — the oldest one written
// before `before`, which the caller sets to the time of the transcript record
// that proves an ask was resolved.
//
// Two properties this deliberately has, both learned the hard way:
//
// One per call. Claude draws queued dialogs in order, and one resolution
// answers one ask, so retiring the whole directory on a single signal would
// discard a second dialog's announcement while it was still on screen.
//
// Older than the evidence. A notice written *after* the newest transcript
// record cannot have been resolved by it: an agent that printed text and then
// asked for permission produces exactly that ordering, and retiring on it
// would erase the ask one tick after it arrived.
func retireOldestAskedNotice(perm string, before, now time.Time) bool {
	files := readClaudeAskedFiles(perm, now)
	for i := len(files) - 1; i >= 0; i-- { // oldest first
		if files[i].at.Before(before) {
			return os.Remove(files[i].path) == nil
		}
	}
	return false
}

// bundleSupportsAsked reports whether a bundle proves the notice layout, from
// disk, so the answer survives a scimux restart. A bundle written before this
// feature says no and keeps the pane-geometry backstop: the gate degrades to
// "no gate", never to "no attention".
func bundleSupportsAsked(bundle string) bool {
	caps, ok := readClaudeHookCapabilities(bundle)
	if !ok || caps.Asked < 1 {
		return false
	}
	// The gate suppresses attention, so it needs the stronger proof: a bundle
	// whose baked binary is gone can never write a notice, and its silence must
	// not read as "Claude asked nothing" (claudeBundleExecUsable).
	if !claudeBundleExecUsable(caps) {
		return false
	}
	st, err := os.Stat(filepath.Join(bundle, "perm", claudeAskedDirName))
	return err == nil && st.IsDir()
}

// noteClaudeAskedCapability records what a fresh bundle proves about notice
// support — from disk, the same proof the restart refresh reads, and recorded
// either way so an unusable bundle cannot keep a stale yes (see
// noteClaudePermCapability).
func (a *app) noteClaudeAskedCapability(hookID string) {
	if hookID == "" {
		return
	}
	ok := bundleSupportsAsked(a.claudeHookBundlePath(hookID))
	a.mu.Lock()
	if a.claudeAskedCap == nil {
		a.claudeAskedCap = map[string]bool{}
	}
	if ok {
		a.claudeAskedCap[hookID] = true
	} else {
		delete(a.claudeAskedCap, hookID)
	}
	a.mu.Unlock()
}

// claudeAskedSupportedLocked reports whether this node's escalations are
// knowable from the hook. Caller may hold a.mu.
func (a *app) claudeAskedSupportedLocked(n *Node) bool {
	if n == nil || n.Agent != "claude" || n.transport() != "tmux" {
		return false
	}
	hookID := a.claudeHookIDLocked(n.ID)
	if hookID == "" {
		return false
	}
	return a.claudeAskedCap[hookID]
}

// claudeAskState is the poller's one read per tick. It answers two questions
// at once: whether the hook is authoritative for this node (capable), and what
// the agent is currently asking for ("" = nothing).
//
// The kind comes from the tool name through the same attentionKind vocabulary
// the transcript path uses, so a plan choice reads as a question and a Bash
// call as an approval. A notice from another session — the previous page after
// a /clear reuses the bundle — is ignored: attention belongs to the session the
// node is bound to.
func (a *app) claudeAskState(n *Node) (attn string, capable bool) {
	a.mu.Lock()
	capable = a.claudeAskedSupportedLocked(n)
	bundle := ""
	sid := ""
	if capable {
		bundle = a.claudePermBundleLocked(n.ID)
		sid = n.SessionID
	}
	a.mu.Unlock()
	if !capable || bundle == "" {
		return "", false
	}
	for _, note := range readClaudeAskedNotices(filepath.Join(bundle, "perm"), time.Now()) {
		if sid != "" && note.Session != "" && note.Session != sid {
			continue
		}
		return attentionKind(note.Tool), true
	}
	return "", true
}

// noteClaudeAskedProgress records the transcript watermark and reports whether
// it grew since the last observation. Growth is the mechanical signal that the
// agent reached its own control flow again — which, for a call that was waiting
// on a human, cannot happen before the human answered. A first observation
// never counts as growth, and a rotation (a smaller offset) counts as none.
func (a *app) noteClaudeAskedProgress(id string, off int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, seen := a.claudeAskedOff[id]
	a.claudeAskedOff[id] = off
	return seen && off > prev
}

// retireClaudeAskedOnProgress retires one ask after the transcript proved one
// was resolved. `before` is the newest dated transcript record; an undated
// transcript retires nothing, the same way every other freshness gate declines
// rather than guesses.
func (a *app) retireClaudeAskedOnProgress(n *Node, before time.Time, dated bool) bool {
	if !dated {
		return false
	}
	a.mu.Lock()
	capable := a.claudeAskedSupportedLocked(n)
	bundle := ""
	if capable {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
	if bundle == "" {
		return false
	}
	return retireOldestAskedNotice(filepath.Join(bundle, "perm"), before, time.Now())
}

// toolEvidence is one transcript tool-call record, reduced to what can retire a
// notice: the call's identity, its tool name, the digest of its input, and when
// it was written.
type toolEvidence struct {
	ID     string
	Tool   string
	Digest string
	At     time.Time
}

// retireAskedByToolEvidence retires the notices whose calls have appeared in
// the transcript, and returns how many went.
//
// This is the exact join that growth-based retirement could only approximate.
// Claude writes a tool_use record *after* the human answers the dialog, so a
// record carrying the same tool name and input digest as a standing notice is
// proof that that specific ask was resolved — no counting, no oldest-first
// guess, and no dependence on how many other calls are in flight. That last
// property is the point: retireOldestAskedNotice is gated on an empty pending
// set, and a working agent almost never has one, so notices accumulated until
// their 4h TTL and held hard attention over an agent that was merely busy
// (observed 2026-08-18: 16 standing notices across 23 minutes of one turn).
//
// Three clauses are kept from the growth rule because they guard real states,
// not hypotheticals:
//
//	one per record — the same command may be asked twice; the second dialog can
//	still be on screen while the first one's record lands.
//	older than the evidence — a record written before the ask cannot have
//	resolved it (run a tool, then ask for the same command again).
//	dated only — an undated record reads as unknown and retires nothing, the
//	same way every other freshness gate declines rather than guesses.
func retireAskedByToolEvidence(perm string, seen []toolEvidence, now time.Time) int {
	if perm == "" || len(seen) == 0 {
		return 0
	}
	files := readClaudeAskedFiles(perm, now)
	if len(files) == 0 {
		return 0
	}
	// Oldest first, so identical asks retire in the order they were made.
	used := make(map[int]bool, len(files))
	retired := 0
	for _, ev := range seen {
		if ev.Digest == "" || ev.Tool == "" || ev.At.IsZero() {
			continue
		}
		for i := len(files) - 1; i >= 0; i-- {
			f := files[i]
			if used[i] || f.note.Digest != ev.Digest || f.note.Tool != ev.Tool {
				continue
			}
			if !f.at.Before(ev.At) {
				continue // the ask is newer than its supposed proof
			}
			if os.Remove(f.path) == nil {
				used[i] = true
				retired++
			}
			break
		}
	}
	return retired
}

// retireClaudeAskedByCalls credits the transcript tool calls this node has not
// been credited for yet against its standing notices, and returns how many were
// retired. The stamps accumulate for the life of the tailer, so a watermark
// keeps one record worth exactly one retirement.
//
// Unlike the growth path this is indifferent to pane state and to the pending
// set: it matches a specific call to the specific notice that announced it, so
// there is nothing for a static dialog or a queued call to confuse.
func (a *app) retireClaudeAskedByCalls(n *Node, stamps []transcript.ToolStamp) int {
	a.mu.Lock()
	seenN := a.claudeAskedTools[n.ID]
	if len(stamps) < seenN {
		seenN = 0 // rotation: the tailer rewound, same signal the mirror uses
	}
	a.claudeAskedTools[n.ID] = len(stamps)
	capable := a.claudeAskedSupportedLocked(n)
	bundle := ""
	if capable {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
	if bundle == "" || seenN >= len(stamps) {
		return 0
	}
	var ev []toolEvidence
	for _, s := range stamps[seenN:] {
		if s.Digest == "" {
			continue // result side, or a call whose input did not parse
		}
		at, err := time.Parse(time.RFC3339, s.Time)
		if err != nil {
			continue // undated reads as unknown, and retires nothing
		}
		ev = append(ev, toolEvidence{ID: s.ID, Tool: s.Title, Digest: s.Digest, At: at})
	}
	return retireAskedByToolEvidence(filepath.Join(bundle, "perm"), ev, time.Now())
}
