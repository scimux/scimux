package remote

import (
	"context"
	"testing"
	"time"
)

func TestSchedWaitOrKickRealTimerConsumesQueuedDeviceWake(t *testing.T) {
	c := &Client{devWake: make(chan struct{}, 1)}
	c.devWake <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- c.schedWaitOrKick(ctx, time.Hour)
	}()

	select {
	case woke := <-done:
		if !woke {
			t.Fatal("queued device wake reported cancellation")
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("queued device wake did not interrupt the real timer")
	}

	select {
	case <-c.devWake:
		t.Fatal("queued device wake was not consumed")
	default:
	}
}
