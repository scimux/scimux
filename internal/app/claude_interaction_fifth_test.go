package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/transcript"
)

func TestInteractionUnparseablePrefixCannotRefreshOldInterrupt(t *testing.T) {
	t.Setenv("SCIMUX_EXPERIMENTAL_CLAUDE_INTERACTIONS", "1")
	a := newTestApp(t, &fakeTmux{})
	n, bundle := seedPermClaude(t, a, "n1", hookSIDOwn)
	path := filepath.Join(t.TempDir(), "prefix.jsonl")
	body := `{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"}}` + "\n" + strings.Repeat(`{"futureRecord":"synthetic"}`+"\n", 12)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	n.Transcript = path
	tl := &transcript.Tailer{Path: path}
	tl.Poll()
	if !tl.EndTurn() || !tl.Unparseable() {
		t.Fatal("setup: prefix did not retain the old interruption with an unparseable tail")
	}
	turn, err := a.beginClaudeAcceptedTurn(n)
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRequest(bundle, time.Now())
	rec.ID = "fifth-unknown-prefix"
	rec.SessionID = n.SessionID
	rec.Turn = turn.Turn
	rec.TurnGen = turn.Gen
	publishEmittedCancel(t, bundle, rec)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`{"type":"file-history-snapshot","snapshot":{}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	after := parseInteractionTime(turn.At).Add(time.Second)
	if err = os.Chtimes(path, after, after); err != nil {
		t.Fatal(err)
	}
	tl.Poll()
	if tl.Unparseable() {
		t.Fatal("setup: recognized metadata did not clear the unknown streak")
	}
	a.noteClaudeInteractionInterrupt(n, tl)
	if a.claudeAcceptedTurnOf(n.ID).Turn != turn.Turn {
		t.Fatal("an unparseable prefix followed only by metadata refreshed its old interruption and settled this turn")
	}
}

func TestInteractionBoundaryConcurrentPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	line := `{"type":"user","timestamp":"2099-01-01T00:00:00Z","message":{"role":"user","content":"[Request interrupted by user]"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	tl := &transcript.Tailer{Path: path}
	tl.Poll()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	turn := claudeAcceptedTurn{At: "2001-01-01T00:00:00Z"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 100; i++ {
			if _, err := f.WriteString(line); err != nil {
				t.Error(err)
				return
			}
			tl.Poll()
		}
	}()
	close(start)
	for i := 0; i < 1000; i++ {
		interactionInterruptIsFresh(tl, turn, claudeInterruptWatermark{})
	}
	wg.Wait()
}

func TestInteractionBoundarySnapshotRejectsChanges(t *testing.T) {
	for _, change := range []string{"advance", "incomplete copy"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.jsonl")
			line := interruptLine("2099-01-01T00:00:00Z", "[Request interrupted by user]")
			writeTranscript(t, path, line)
			tl := pollTranscript(t, path)
			previous := writeInterruptPrefix
			t.Cleanup(func() { writeInterruptPrefix = previous })
			writeInterruptPrefix = func(f *os.File, data []byte) error {
				if change == "advance" {
					appendTranscript(t, path, userLine("", "a new synthetic prompt"))
					tl.Poll()
				} else {
					data = data[:len(data)-1]
				}
				_, err := f.Write(data)
				return err
			}
			turn := claudeAcceptedTurn{At: "2001-01-01T00:00:00Z"}
			if interactionInterruptIsFresh(tl, turn, claudeInterruptWatermark{}) {
				t.Fatal("an advancing or incomplete snapshot settled an interruption")
			}
		})
	}
}

func TestUndatedInteractionRequiresReadableSource(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "deleted.jsonl")} {
		if undatedInterruptNewlyObserved(&transcript.Tailer{Path: path}, time.Now(), claudeInterruptWatermark{}) {
			t.Fatal("an undated interruption without a readable source was fresh")
		}
	}
}
