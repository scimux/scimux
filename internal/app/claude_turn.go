package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// claude_turn.go owns the app-side accepted-turn nonce: mint, publish before
// Enter, close, abort, and the two matching predicates.

var (
	errClaudeTurnInFlight = errors.New("a Claude turn is still awaiting its Stop hook")
	errClaudeTurnMarker   = errors.New("could not publish the Claude turn fence")
)

// claudeAcceptedTurn is the app-owned identity of one accepted Claude turn.
// The Stop and Notification helpers copy Turn/Gen into their notices so a
// late event from turn N cannot mutate turn N+1.
type claudeAcceptedTurn struct {
	Turn    string `json:"turn"`
	Gen     int    `json:"gen"`
	Session string `json:"session,omitempty"`
	At      string `json:"at,omitempty"`
}

func claudeTurnPath(perm string) string {
	return filepath.Join(perm, "turn.json")
}

func readClaudeAcceptedTurn(perm string) (claudeAcceptedTurn, bool) {
	var empty claudeAcceptedTurn
	if perm == "" {
		return empty, false
	}
	b, err := os.ReadFile(claudeTurnPath(perm))
	if err != nil || len(b) == 0 {
		return empty, false
	}
	var t claudeAcceptedTurn
	if json.Unmarshal(b, &t) != nil || t.Turn == "" || !safePathComponent(t.Turn) {
		return empty, false
	}
	return t, true
}

func writeClaudeAcceptedTurn(perm string, t claudeAcceptedTurn) error {
	if perm == "" || t.Turn == "" {
		return errClaudeHookRejected
	}
	if st, err := os.Stat(perm); err != nil || !st.IsDir() {
		return errClaudeHookRejected
	}
	return writeClaudePermFile(claudeTurnPath(perm), t)
}

func clearClaudeAcceptedTurnFile(perm string) {
	if perm == "" {
		return
	}
	_ = os.Remove(claudeTurnPath(perm))
}

func newClaudeTurnNonce() (string, error) {
	var rnd [8]byte
	if _, err := io.ReadFull(rand.Reader, rnd[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(rnd[:]), nil
}

func (a *app) claudeBindLock(nodeID string) *sync.Mutex {
	if nodeID == "" {
		return new(sync.Mutex)
	}
	a.claudeBindMu.Lock()
	defer a.claudeBindMu.Unlock()
	if a.claudeBindLocks == nil {
		a.claudeBindLocks = map[string]*sync.Mutex{}
	}
	m := a.claudeBindLocks[nodeID]
	if m == nil {
		m = &sync.Mutex{}
		a.claudeBindLocks[nodeID] = m
	}
	return m
}

func (a *app) claudeAcceptedTurnOf(nodeID string) claudeAcceptedTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeTurns[nodeID]
}

func (a *app) claudeClosingTurnOf(nodeID string) claudeAcceptedTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeClosing[nodeID]
}

// beginClaudeAcceptedTurn publishes the app-owned turn nonce before the
// prompt's Enter is sent. Callers hold the node's auto gate from this call
// through delivery (or abortClaudeAcceptedTurn). That ordering is the causal
// fence for Stop and Notification: a hook can never fire for a submitted turn
// before its token is visible, and the next turn cannot replace the token
// until the matching Stop has been drained.
func (a *app) beginClaudeAcceptedTurn(n *Node) (claudeAcceptedTurn, error) {
	var empty claudeAcceptedTurn
	if n == nil {
		return empty, errClaudeTurnMarker
	}
	nonce, err := newClaudeTurnNonce()
	if err != nil {
		return empty, errClaudeTurnMarker
	}
	a.mu.Lock()
	cur := a.byID[n.ID]
	if cur == nil || cur != n || n.Agent != "claude" {
		a.mu.Unlock()
		return empty, errClaudeTurnMarker
	}
	if a.claudeTurns[n.ID].Turn != "" {
		a.mu.Unlock()
		return empty, errClaudeTurnInFlight
	}
	prev := a.claudeClosing[n.ID]
	next := claudeAcceptedTurn{
		Turn:    nonce,
		Gen:     prev.Gen + 1,
		Session: n.SessionID,
		At:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	bundle := a.claudePermBundleLocked(n.ID)
	a.mu.Unlock()
	if bundle == "" || writeClaudeAcceptedTurn(filepath.Join(bundle, "perm"), next) != nil {
		return empty, errClaudeTurnMarker
	}

	// Revalidate after I/O. No prompt has been submitted yet, so removing our
	// own marker is safe if the node/bundle changed while the file was written.
	a.mu.Lock()
	valid := a.byID[n.ID] == n && n.Agent == "claude" &&
		a.claudeTurns[n.ID].Turn == "" &&
		a.claudePermBundleLocked(n.ID) == bundle && n.SessionID == next.Session
	if valid {
		a.claudeTurns[n.ID] = next
	}
	a.mu.Unlock()
	if !valid {
		clearClaudeAcceptedTurnFileIf(filepath.Join(bundle, "perm"), next.Turn)
		return empty, errClaudeTurnMarker
	}
	return next, nil
}

// abortClaudeAcceptedTurn rolls back a prepared token when the tmux send
// failed before a turn was accepted. Callers still hold the auto gate.
func (a *app) abortClaudeAcceptedTurn(nodeID, turn string) {
	if nodeID == "" || turn == "" {
		return
	}
	a.mu.Lock()
	cur := a.claudeTurns[nodeID]
	bundle := a.claudePermBundleLocked(nodeID)
	if cur.Turn == turn {
		delete(a.claudeTurns, nodeID)
	}
	a.mu.Unlock()
	if cur.Turn == turn && bundle != "" {
		clearClaudeAcceptedTurnFileIf(filepath.Join(bundle, "perm"), turn)
	}
}

// closeClaudeAcceptedTurnLocked closes the token while the node's auto gate is
// held. It moves the current turn to closing so a second Stop for the same
// turn is idempotent, and a later begin cannot be matched by the closed nonce.
// Serializing this with beginClaudeAcceptedTurn is what prevents Stop N
// from ever observing a token for N+1.
func (a *app) closeClaudeAcceptedTurnLocked(nodeID string) {
	a.mu.Lock()
	cur := a.claudeTurns[nodeID]
	if cur.Turn != "" {
		if a.claudeClosing == nil {
			a.claudeClosing = map[string]claudeAcceptedTurn{}
		}
		a.claudeClosing[nodeID] = cur
	}
	delete(a.claudeTurns, nodeID)
	bundle := a.claudePermBundleLocked(nodeID)
	a.mu.Unlock()
	if bundle != "" && cur.Turn != "" {
		clearClaudeAcceptedTurnFileIf(filepath.Join(bundle, "perm"), cur.Turn)
	}
}

func clearClaudeAcceptedTurnFileIf(perm, turn string) {
	cur, ok := readClaudeAcceptedTurn(perm)
	if ok && cur.Turn == turn {
		clearClaudeAcceptedTurnFile(perm)
	}
}

// claudeStopMatchesTurn reports whether a Stop notice may settle this node's
// current or closing accepted turn. A late Stop from turn N cannot revoke,
// clear, tombstone, or otherwise mutate turn N+1: a tagged notice settles
// only the live turn with that nonce, or, once that nonce has moved to
// closing, the closing turn — never a successor.
//
// An empty notice.Turn is a pre-nonce notice. It has no identity, so it may
// settle only a node with no current turn; a nonce-fenced live turn must not
// be closed by an untagged leftover.
func claudeStopMatchesTurn(notice claudeStopNotice, current, closing claudeAcceptedTurn) bool {
	if notice.Turn != "" {
		if current.Turn != "" {
			return notice.Turn == current.Turn
		}
		return closing.Turn != "" && notice.Turn == closing.Turn
	}
	// A notice written before turn nonces existed can only settle a node
	// that has not accepted a newer turn.
	return current.Turn == ""
}

// claudeNotifyBelongsToCurrentTurn reports whether a Notification may mint a
// visible-dialog epoch for the live turn. A late notice from turn N cannot
// present itself as turn N+1's dialog.
//
// Generation comparison is skipped when either side is 0: 0 is "unknown",
// not an identity. Treating a missing generation as older than every live
// turn would drop a current-turn prompt that simply predates the stamp.
//
// An unparseable timestamp skips time fencing instead of rejecting. Time is
// a sharpening constraint when both clocks parse; a garbage stamp is missing
// evidence, not proof that the notice belongs to a previous turn.
//
// A notice stamped exactly at the live turn's start belongs to it
// (evAt.Before(curAt) is exclusive): that instant is the turn beginning.
// A notice stamped exactly at the closed turn's boundary belongs to the
// closed one (!evAt.After(clAt) is inclusive): that instant is the Stop,
// and must not mint an epoch for the successor.
func claudeNotifyBelongsToCurrentTurn(ev claudeNotifyNotice, current claudeAcceptedTurn, closed claudeTurnClosed, hadClosed bool) bool {
	if ev.Turn != "" {
		if current.Turn != "" && ev.Turn != current.Turn {
			return false
		}
		if hadClosed && closed.Turn != "" && ev.Turn == closed.Turn {
			return false
		}
	}
	if ev.TurnGen > 0 && current.Gen > 0 && ev.TurnGen < current.Gen {
		return false
	}
	if ev.At != "" {
		evAt, evErr := time.Parse(time.RFC3339Nano, ev.At)
		if evErr == nil {
			if current.At != "" {
				if curAt, err := time.Parse(time.RFC3339Nano, current.At); err == nil && evAt.Before(curAt) {
					return false
				}
			}
			if hadClosed && closed.At != "" {
				if clAt, err := time.Parse(time.RFC3339Nano, closed.At); err == nil && !evAt.After(clAt) {
					return false
				}
			}
		}
	}
	// No turn tag, a turn already closed, and a newer turn is live: the
	// notify cannot be proven to belong to the current turn.
	if ev.Turn == "" && hadClosed && current.Turn != "" {
		return false
	}
	return true
}

func (a *app) noteClaudeDialogAmbiguous(nodeID, msg string) {
	if nodeID == "" || msg == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.claudeDialogNote == nil {
		a.claudeDialogNote = map[string]string{}
	}
	a.claudeDialogNote[nodeID] = msg
}

func (a *app) claudeDialogNoteOf(nodeID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claudeDialogNote[nodeID]
}

func (a *app) clearClaudeDialogNote(nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.claudeDialogNote, nodeID)
}
