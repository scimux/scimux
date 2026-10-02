package muse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// An admitted send can lose the process before it records a user turn. Keep
// that post-admission refusal covered without depending on goroutine timing.
func TestAdmittedSendRejectsStoppedSessionWithoutWriting(t *testing.T) {
	m := &Manager{}
	s := &nodeSession{nodeID: "synthetic", procAlive: true, stopping: true, turnActive: true}
	m.wg.Add(1)
	s.work.Add(1)
	if err := m.sendAdmittedTurn("synthetic", s, "hello"); !errors.Is(err, ErrNotAlive) {
		t.Fatalf("send admitted after stop = %v, want ErrNotAlive", err)
	}
	s.mu.Lock()
	active := s.turnActive
	s.mu.Unlock()
	if active {
		t.Fatal("stopped send retained an active turn")
	}
	s.work.Wait()
	m.wg.Wait()
}

// The cancellation task must exit quietly when Close or cancellation wins
// the operation gate. Calling the phase synchronously makes both outcomes
// independent of dispatch-loop scheduling.
func TestUnsupportedUserInputCancelStopsQuietlyBeforeWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Client) context.Context
	}{
		{"closed", func(c *Client) context.Context {
			c.op.shutdown()
			return context.Background()
		}},
		{"canceled", func(c *Client) context.Context {
			c.op.held = true
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var emitted []Event
			c := &Client{sink: func(e Event) { emitted = append(emitted, e) }}
			parent := tc.setup(c)
			c.cancelUnsupportedUserInput("", "session", "input", parent, time.Second)
			if len(emitted) != 0 {
				t.Fatalf("quiet cancellation emitted %+v", emitted)
			}
		})
	}
}

func TestUnsupportedUserInputCancelReportsCommandIDFailure(t *testing.T) {
	previous := defaultUUID
	defaultUUID = newUUIDGen(func() int64 { return 1 }, failReader{err: errors.New("entropy unavailable")})
	t.Cleanup(func() { defaultUUID = previous })
	var emitted []Event
	c := &Client{sink: func(e Event) { emitted = append(emitted, e) }}
	c.cancelUnsupportedUserInput("", "session", "input", context.Background(), time.Second)
	if len(emitted) != 1 || emitted[0].T != "error" || !strings.Contains(emitted[0].Error, "entropy unavailable") {
		t.Fatalf("command ID failure events = %+v", emitted)
	}
}
