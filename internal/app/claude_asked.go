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
)

// claudeAskedTTL bounds how long a notice may stand. It is a leak backstop,
// not the retirement policy: a notice is normally retired the moment the
// helper auto-approves its call, or when the pane resumes streaming (the
// mechanical diff-geometry signal that the dialog is gone). A dialog may
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

// readClaudeAskedNotices returns the standing notices, newest first, dropping
// (and deleting) anything past the TTL. Defensive like every other transcript
// reader: an unreadable or unparseable file is ignored, never an error.
func readClaudeAskedNotices(perm string, now time.Time) []claudeAskedNotice {
	dir := filepath.Join(perm, claudeAskedDirName)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []claudeAskedNotice
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
		out = append(out, note)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

// sweepClaudeAskedNotices retires every standing notice for a bundle. The
// caller must have mechanical evidence that no dialog is on screen — today
// that is the pane resuming unconfined output, which is the same diff geometry
// noteAnim already uses to decide an animation strip has ended.
func sweepClaudeAskedNotices(perm string) {
	dir := filepath.Join(perm, claudeAskedDirName)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// bundleSupportsAsked reports whether a bundle proves the notice layout, from
// disk, so the answer survives a scimux restart. A bundle written before this
// feature says no and keeps the pane-geometry backstop: the gate degrades to
// "no gate", never to "no attention".
func bundleSupportsAsked(bundle string) bool {
	if bundle == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(bundle, "capabilities.json"))
	if err != nil {
		return false
	}
	var caps struct {
		Asked int `json:"asked"`
	}
	if json.Unmarshal(b, &caps) != nil || caps.Asked < 1 {
		return false
	}
	st, err := os.Stat(filepath.Join(bundle, "perm", claudeAskedDirName))
	return err == nil && st.IsDir()
}

// noteClaudeAskedCapability records a fresh bundle's notice support.
func (a *app) noteClaudeAskedCapability(hookID string) {
	if hookID == "" {
		return
	}
	a.mu.Lock()
	if a.claudeAskedCap == nil {
		a.claudeAskedCap = map[string]bool{}
	}
	a.claudeAskedCap[hookID] = true
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

// sweepClaudeAsked retires a node's notices when the pane proves no dialog is
// up. Never called with pane *text* as the evidence.
func (a *app) sweepClaudeAsked(n *Node) {
	a.mu.Lock()
	capable := a.claudeAskedSupportedLocked(n)
	bundle := ""
	if capable {
		bundle = a.claudePermBundleLocked(n.ID)
	}
	a.mu.Unlock()
	if bundle == "" {
		return
	}
	sweepClaudeAskedNotices(filepath.Join(bundle, "perm"))
}
