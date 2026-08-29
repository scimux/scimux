package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAbortClaudeAcceptedTurn(t *testing.T) {
	const seededNonce = "1111222233334444"

	seed := func(t *testing.T) (*app, *Node, string, claudeAcceptedTurn) {
		t.Helper()
		a, n, bundle := seedElicitationNode(t, "quiet pane")
		perm := filepath.Join(bundle, "perm")
		turn := claudeAcceptedTurn{
			Turn: seededNonce, Gen: 1, Session: hookSIDOwn,
			At: time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := writeClaudeAcceptedTurn(perm, turn); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		a.claudeTurns[n.ID] = turn
		a.mu.Unlock()
		return a, n, perm, turn
	}

	assertUntouched := func(t *testing.T, a *app, n *Node, perm string, want claudeAcceptedTurn) {
		t.Helper()
		got, ok := readClaudeAcceptedTurn(perm)
		if !ok {
			t.Fatal("turn.json was removed")
		}
		if got != want {
			t.Fatalf("turn.json = %+v, want %+v", got, want)
		}
		a.mu.Lock()
		mem := a.claudeTurns[n.ID]
		a.mu.Unlock()
		if mem != want {
			t.Fatalf("in-memory turn = %+v, want %+v", mem, want)
		}
	}

	cases := []struct {
		name     string
		nodeID   string // "id" uses the seeded node; other values are used as-is
		turn     string
		wantGone bool
	}{
		{
			name:     "matching abort rolls back the prepared turn in memory and on disk",
			nodeID:   "id",
			turn:     seededNonce,
			wantGone: true,
		},
		{
			name:   "mismatched abort cannot roll back a different live turn",
			nodeID: "id",
			turn:   "9999888877776666",
		},
		{
			name: "empty node id is a no-op",
			turn: seededNonce,
		},
		{
			name:   "empty turn nonce is a no-op",
			nodeID: "id",
		},
		{
			name:   "unknown node cannot disturb a seeded turn",
			nodeID: "never-seeded",
			turn:   seededNonce,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, n, perm, want := seed(t)
			id := tc.nodeID
			if id == "id" {
				id = n.ID
			}
			a.abortClaudeAcceptedTurn(id, tc.turn)
			if !tc.wantGone {
				assertUntouched(t, a, n, perm, want)
				return
			}
			if _, ok := readClaudeAcceptedTurn(perm); ok {
				t.Fatal("matching abort must remove turn.json")
			}
			a.mu.Lock()
			_, present := a.claudeTurns[n.ID]
			a.mu.Unlock()
			if present {
				t.Fatal("matching abort must delete the in-memory entry")
			}
		})
	}
}

func TestClaudeElicitationBelongsToOpenTurn(t *testing.T) {
	const (
		turnN     = "1111222233334444"
		turnOther = "aaaabbbbccccdddd"
		sessOther = "sess-other"
	)
	boundary := time.Date(2026, 8, 29, 12, 0, 0, 123456789, time.UTC)
	boundaryAt := boundary.UTC().Format(time.RFC3339Nano)

	cases := []struct {
		name    string
		current *claudeAcceptedTurn
		closed  *claudeTurnClosed
		f       claudeElicitationFile
		want    bool
	}{
		{
			name: "tagged elicitation belonging to the live turn is open",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 1, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: hookSIDOwn, Turn: turnN, TurnGen: 1,
			}},
			want: true,
		},
		{
			name: "tagged elicitation from another session cannot belong to the live turn",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 1, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: sessOther, Turn: turnN, TurnGen: 1,
			}},
		},
		{
			name: "tagged elicitation from another turn cannot belong to the live turn",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 1, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: hookSIDOwn, Turn: turnOther, TurnGen: 1,
			}},
		},
		{
			name: "tagged elicitation from another generation cannot belong to the live turn",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 2, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: hookSIDOwn, Turn: turnN, TurnGen: 1,
			}},
		},
		{
			name: "missing generation on a tagged elicitation is not a rejection",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 2, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: hookSIDOwn, Turn: turnN,
			}},
			want: true,
		},
		{
			name: "tagged elicitation cannot belong when no turn is current",
			f: claudeElicitationFile{rec: claudeElicitationRecord{
				SessionID: hookSIDOwn, Turn: turnN, TurnGen: 1,
			}},
		},
		{
			name: "untagged elicitation cannot belong while a turn is current",
			current: &claudeAcceptedTurn{
				Turn: turnN, Gen: 1, Session: hookSIDOwn, At: boundaryAt,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{SessionID: hookSIDOwn}},
		},
		{
			name: "untagged elicitation belongs when no turn is current or closed",
			f:    claudeElicitationFile{rec: claudeElicitationRecord{SessionID: hookSIDOwn}},
			want: true,
		},
		{
			name: "untagged elicitation from another session still belongs after a closed turn",
			closed: &claudeTurnClosed{
				Session: sessOther, At: boundaryAt, Turn: turnN, Gen: 1,
			},
			f: claudeElicitationFile{
				rec: claudeElicitationRecord{SessionID: hookSIDOwn},
				at:  boundary,
			},
			want: true,
		},
		{
			name: "untagged elicitation timestamped exactly at the closed turn boundary is rejected",
			closed: &claudeTurnClosed{
				Session: hookSIDOwn, At: boundaryAt, Turn: turnN, Gen: 1,
			},
			f: claudeElicitationFile{
				rec: claudeElicitationRecord{SessionID: hookSIDOwn},
				at:  boundary,
			},
		},
		{
			name: "untagged elicitation timestamped after the closed turn boundary belongs",
			closed: &claudeTurnClosed{
				Session: hookSIDOwn, At: boundaryAt, Turn: turnN, Gen: 1,
			},
			f: claudeElicitationFile{
				rec: claudeElicitationRecord{SessionID: hookSIDOwn},
				at:  boundary.Add(time.Second),
			},
			want: true,
		},
		{
			name: "untagged elicitation cannot belong when the closed marker timestamp is unparseable",
			closed: &claudeTurnClosed{
				Session: hookSIDOwn, At: "not-a-timestamp", Turn: turnN, Gen: 1,
			},
			f: claudeElicitationFile{rec: claudeElicitationRecord{SessionID: hookSIDOwn}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			perm := filepath.Join(bundle, "perm")
			if err := os.MkdirAll(perm, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.current != nil {
				if err := writeClaudeAcceptedTurn(perm, *tc.current); err != nil {
					t.Fatal(err)
				}
			}
			if tc.closed != nil {
				if err := writeClaudePermFile(claudeTurnClosedPath(perm), *tc.closed); err != nil {
					t.Fatal(err)
				}
			}
			got := claudeElicitationBelongsToOpenTurn(bundle, tc.f)
			if got != tc.want {
				t.Fatalf("claudeElicitationBelongsToOpenTurn() = %v, want %v\ncurrent=%+v closed=%+v f.rec=%+v f.at=%v",
					got, tc.want, tc.current, tc.closed, tc.f.rec, tc.f.at)
			}
		})
	}
}
