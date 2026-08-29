package app

import "testing"

// A late Stop from turn N cannot revoke, clear, tombstone, or otherwise
// mutate turn N+1. These tables pin the two pure fence predicates that
// enforce that rule.

func TestClaudeStopMatchesTurn(t *testing.T) {
	const (
		turnN     = "turn-n"
		turnNPlus = "turn-n-plus-1"
	)
	cases := []struct {
		name    string
		notice  claudeStopNotice
		current claudeAcceptedTurn
		closing claudeAcceptedTurn
		want    bool
	}{
		{
			name:    "stop for the live turn settles it",
			notice:  claudeStopNotice{Turn: turnN},
			current: claudeAcceptedTurn{Turn: turnN},
			want:    true,
		},
		{
			name:    "late stop from turn N cannot settle turn N+1",
			notice:  claudeStopNotice{Turn: turnN},
			current: claudeAcceptedTurn{Turn: turnNPlus},
			want:    false,
		},
		{
			name:    "stop for the closing turn settles after the live nonce is gone",
			notice:  claudeStopNotice{Turn: turnN},
			closing: claudeAcceptedTurn{Turn: turnN},
			want:    true,
		},
		{
			name:    "late stop from another turn cannot settle a different closing turn",
			notice:  claudeStopNotice{Turn: turnN},
			closing: claudeAcceptedTurn{Turn: turnNPlus},
			want:    false,
		},
		{
			name:   "stop with a nonce cannot settle when no turn is current or closing",
			notice: claudeStopNotice{Turn: turnN},
			want:   false,
		},
		{
			name: "legacy stop without a nonce still settles when no nonce-fenced turn is live",
			want: true,
		},
		{
			name:    "legacy stop without a nonce cannot settle a nonce-fenced live turn",
			current: claudeAcceptedTurn{Turn: turnN},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeStopMatchesTurn(tc.notice, tc.current, tc.closing)
			if got != tc.want {
				t.Fatalf("claudeStopMatchesTurn() = %v, want %v\nnotice=%+v current=%+v closing=%+v",
					got, tc.want, tc.notice, tc.current, tc.closing)
			}
		})
	}
}

func TestClaudeNotifyBelongsToCurrentTurn(t *testing.T) {
	const (
		turnN         = "turn-n"
		turnNPlus     = "turn-n-plus-1"
		currentAt     = "2026-08-29T12:00:00.000000000Z"
		beforeCurrent = "2026-08-29T11:59:59.999999999Z"
		closedAt      = "2026-08-29T11:30:00.000000000Z"
		afterClosed   = "2026-08-29T11:30:00.000000001Z"
	)
	cases := []struct {
		name      string
		ev        claudeNotifyNotice
		current   claudeAcceptedTurn
		closed    claudeTurnClosed
		hadClosed bool
		want      bool
	}{
		{
			name:    "notification carrying the live turn nonce belongs",
			ev:      claudeNotifyNotice{Turn: turnN},
			current: claudeAcceptedTurn{Turn: turnN},
			want:    true,
		},
		{
			name:    "late notification from turn N cannot belong to turn N+1",
			ev:      claudeNotifyNotice{Turn: turnN},
			current: claudeAcceptedTurn{Turn: turnNPlus},
			want:    false,
		},
		{
			name:      "notification for a closed turn cannot belong after that turn has ended",
			ev:        claudeNotifyNotice{Turn: turnN},
			hadClosed: true,
			closed:    claudeTurnClosed{Turn: turnN},
			want:      false,
		},
		{
			name:    "notification from an earlier generation cannot belong to a later one",
			ev:      claudeNotifyNotice{TurnGen: 1},
			current: claudeAcceptedTurn{Gen: 2},
			want:    false,
		},
		{
			name:    "missing generation on the notification is not a rejection",
			current: claudeAcceptedTurn{Gen: 2},
			want:    true,
		},
		{
			name: "missing generation on the live turn is not a rejection",
			ev:   claudeNotifyNotice{TurnGen: 2},
			want: true,
		},
		{
			name:    "notification timestamped exactly at the live turn start is accepted",
			ev:      claudeNotifyNotice{At: currentAt},
			current: claudeAcceptedTurn{At: currentAt},
			want:    true,
		},
		{
			name:    "notification timestamped before the live turn cannot belong to it",
			ev:      claudeNotifyNotice{At: beforeCurrent},
			current: claudeAcceptedTurn{At: currentAt},
			want:    false,
		},
		{
			name:      "notification timestamped exactly at the closed turn boundary is rejected",
			ev:        claudeNotifyNotice{At: closedAt},
			hadClosed: true,
			closed:    claudeTurnClosed{At: closedAt},
			want:      false,
		},
		{
			name:      "notification timestamped after the closed turn boundary belongs to the live turn",
			ev:        claudeNotifyNotice{At: afterClosed},
			hadClosed: true,
			closed:    claudeTurnClosed{At: closedAt},
			want:      true,
		},
		{
			name: "unparseable timestamp skips time fencing rather than rejecting",
			ev:   claudeNotifyNotice{At: "not-a-timestamp"},
			want: true,
		},
		{
			name:      "legacy notification without a nonce cannot belong after a turn has closed",
			current:   claudeAcceptedTurn{Turn: turnNPlus},
			hadClosed: true,
			closed:    claudeTurnClosed{Turn: turnN},
			want:      false,
		},
		{
			name:    "legacy notification without a nonce belongs when no turn has closed",
			current: claudeAcceptedTurn{Turn: turnN},
			want:    true,
		},
		{
			name: "zero-value notification belongs to a zero-value live turn",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeNotifyBelongsToCurrentTurn(tc.ev, tc.current, tc.closed, tc.hadClosed)
			if got != tc.want {
				t.Fatalf("claudeNotifyBelongsToCurrentTurn() = %v, want %v\nev=%+v current=%+v closed=%+v hadClosed=%v",
					got, tc.want, tc.ev, tc.current, tc.closed, tc.hadClosed)
			}
		})
	}
}
