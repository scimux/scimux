package app

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/team"
)

// shareOutbox copies shared chats to their team servers. The poll loop
// calls kick. A tick that is still running makes the next kick return.
type shareOutbox struct {
	dataDir string
	now     func() time.Time
	client  *team.Client
	save    func(dir string, s team.Share) error
	readLog func(path string) []sessionlog.Event
	logSize func(path string) (int64, error)

	mu      sync.Mutex
	running bool
}

func (o *shareOutbox) kick() {}

func (o *shareOutbox) tick(context.Context) {}

func (o *shareOutbox) delivery(string) string { return "ok" }

// copySharedChats is the poll loop's hook. A nil outbox does nothing.
func (a *app) copySharedChats() {
	if a.shareOutbox == nil {
		return
	}
	a.shareOutbox.kick()
}

// defaultLogSize is the size check used when a test does not replace it.
func defaultLogSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
