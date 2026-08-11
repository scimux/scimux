package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// storeRecord is one line of the append-only store file. Node metadata is
// tiny; chat content lives in the agents' own transcript files.
type storeRecord struct {
	Type string `json:"type"` // "node" | "transcript" | "key" | "delete" | "transcript-retired"
	Node *Node  `json:"node,omitempty"`
	ID   string `json:"id,omitempty"`
	Path string `json:"path,omitempty"`
	// SessionID is set on "transcript-retired" tombstones so a pane cmdline
	// that still names the launch session cannot rebind the dead file after
	// a /clear (P2a). Omitempty keeps older record types unchanged.
	SessionID string `json:"session_id,omitempty"`
	// "key" records are the answered-dialog evidence trail. Replay ignores
	// them — they carry no node state.
	//
	// Key is the semantic web choice the supervisor selected (e.g. "1", "y",
	// "Escape"). Keys, when present, is the physical tmux key sequence that
	// was actually delivered for that one web action (e.g. ["1","Enter"] for
	// an AX Claude numbered choice, or ["Escape"] for a single navigation
	// key). Structured Codex/ACP decisions leave Keys absent: no terminal
	// keys were pressed.
	Key     string   `json:"key,omitempty"`
	Keys    []string `json:"keys,omitempty"`
	Excerpt string   `json:"excerpt,omitempty"`
	Time    string   `json:"time,omitempty"`
}

func (a *app) appendRecord(rec storeRecord) error {
	// 0600: the store holds prompts, working dirs, transcript paths, and
	// remote-key pane excerpts. Match the ACP log and ui.json rather than
	// relying on the startup chmod of the parent dir (finding 63).
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// One process-local write point, fsynced. The store is the durability and
	// audit mechanism (replay, key evidence, slug-reuse safety), so an append
	// is only "done" once the bytes and a full-write check have reached disk.
	a.storeMu.Lock()
	defer a.storeMu.Unlock()
	// A first append creates nodes.jsonl; its dirent is not crash-durable until
	// the parent directory is synced too (see sessionlog.SyncParentDir). Detect
	// creation under the same lock that serializes the write.
	_, statErr := os.Stat(a.storePath)
	created := os.IsNotExist(statErr)
	f, err := os.OpenFile(a.storePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && created {
		err = sessionlog.SyncParentDir(a.storePath)
	}
	return err
}

func (a *app) loadStore() error {
	b, err := os.ReadFile(a.storePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Replay semantics for the append-only store: corrections are new
	// records. A later node record for an existing ID replaces the value in
	// place (first-seen order preserved, no duplicate UI nodes); the latest
	// transcript record wins regardless of where it appears relative to its
	// node record. A delete record removes the node from the visible registry;
	// a later node record with the same ID would reintroduce it.
	// "transcript-retired" tombstones accumulate per-node into
	// deadTranscripts (path and session id); a delete drops the node's set.
	// Unknown types are silently ignored — never errors.
	if a.deadTranscripts == nil {
		a.deadTranscripts = map[string]map[string]bool{}
	}
	transcripts := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec storeRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		switch {
		case rec.Type == "node" && rec.Node != nil:
			if rec.Node.Description == "" {
				rec.Node.Description = rec.Node.Prompt
			}
			if existing, ok := a.byID[rec.Node.ID]; ok {
				*existing = *rec.Node
			} else {
				a.nodes = append(a.nodes, rec.Node)
				a.byID[rec.Node.ID] = rec.Node
			}
		case rec.Type == "transcript":
			transcripts[rec.ID] = rec.Path
		case rec.Type == "transcript-retired":
			a.markDeadTranscriptLocked(rec.ID, rec.Path, rec.SessionID)
		case rec.Type == "delete":
			a.removeNodeLocked(rec.ID)
			delete(transcripts, rec.ID)
		}
	}
	for id, path := range transcripts {
		if n, ok := a.byID[id]; ok {
			n.Transcript = path
		}
	}
	return nil
}

// markDeadTranscriptLocked records a retired path and/or session id for a
// node. Callers that already hold a.mu (or run single-threaded at startup)
// use this; per-node, never global — another node may own the same path.
func (a *app) markDeadTranscriptLocked(nodeID, path, sessionID string) {
	if nodeID == "" {
		return
	}
	if a.deadTranscripts == nil {
		a.deadTranscripts = map[string]map[string]bool{}
	}
	set := a.deadTranscripts[nodeID]
	if set == nil {
		set = map[string]bool{}
		a.deadTranscripts[nodeID] = set
	}
	if path != "" {
		set[path] = true
	}
	if sessionID != "" {
		set[sessionID] = true
	}
}

// isDeadTranscript reports whether path or sessionID was retired for this
// node. Safe without a.mu for a snapshot read of the map reference; callers
// that mutate hold a.mu around mark/delete.
func (a *app) isDeadTranscript(nodeID, path, sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	set := a.deadTranscripts[nodeID]
	if set == nil {
		return false
	}
	return (path != "" && set[path]) || (sessionID != "" && set[sessionID])
}

// removeNodeLocked drops a node and every per-node poll field. Runtime callers
// hold a.mu; startup replay calls it before the app is published or concurrent.
// This is a deliberate non-poller cleanup: poller.go owns normal
// live/attn/anim/tailer/mirror/chatMark writes, while deletion must clear the
// same maps so a reissued slug never inherits dead poll state.
func (a *app) removeNodeLocked(id string) {
	delete(a.byID, id)
	for i, n := range a.nodes {
		if n.ID == id {
			a.nodes = append(a.nodes[:i], a.nodes[i+1:]...)
			break
		}
	}
	delete(a.live, id)
	delete(a.attn, id)
	delete(a.turnDone, id)
	delete(a.attnAt, id)
	delete(a.prevCap, id)
	delete(a.lastChg, id)
	delete(a.activeSince, id)
	delete(a.tailers, id)
	delete(a.mirrors, id)
	delete(a.piMirrors, id)
	delete(a.chatMark, id)
	delete(a.staleChat, id)
	delete(a.sendState, id)
	delete(a.segCache, id)
	delete(a.fareCache, id)
	delete(a.anim, id)
	delete(a.deadTranscripts, id)
}

// sessionLogPath is the single spelling of a node's session-log location; the
// id is the reusable title slug and the file's basename.
func (a *app) sessionLogPath(id string) string {
	return filepath.Join(a.sessionsDir, id+".jsonl")
}

func (a *app) sessionLogExists(id string) bool {
	if a.sessionsDir == "" {
		return false
	}
	_, err := os.Stat(a.sessionLogPath(id))
	return err == nil
}

// archiveSessionLog moves a deleted node's session log into sessions/archive/
// with a timestamp suffix. Node IDs are friendly slugs that uniqueID can
// reissue once the node is gone; without this move a future node reusing the
// slug would append onto the dead node's history. Best-effort: history
// retention must never block a delete.
func (a *app) archiveSessionLog(id string) {
	src := a.sessionLogPath(id)
	if _, err := os.Stat(src); err != nil {
		return
	}
	dir := filepath.Join(a.sessionsDir, "archive")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive session log for %s: %v\n", id, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	if err := os.Rename(src, filepath.Join(dir, id+"."+stamp+".jsonl")); err != nil {
		fmt.Fprintf(os.Stderr, "scimux: archive session log for %s: %v\n", id, err)
	}
}
