package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// A Claude escalation notice records a digest of the tool_input it escalated
// (internal/app/claude_asked.go). Claude writes the matching tool_use record
// only *after* a human answers the dialog, so that record is mechanical proof
// that one specific ask was resolved — far stronger than "the transcript grew".
// Pinned against real data 2026-08-18: the hook's tool_input bytes and the
// transcript's input bytes hash identically, so the raw digest joins them with
// no canonicalization on either side.
func TestToolStampCarriesInputDigest(t *testing.T) {
	input := `{"command":"date > x.txt","description":"stamp"}`
	sum := sha256.Sum256([]byte(input))
	want := hex.EncodeToString(sum[:])[:16]

	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"user","timestamp":"t1","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":`+input+`}]}}`)

	tl := &Tailer{Path: path}
	tl.Poll()

	var got ToolStamp
	for _, ts := range tl.Tools {
		if ts.ID == "tu1" && ts.Status == "pending" {
			got = ts
		}
	}
	if got.ID == "" {
		t.Fatal("no pending stamp for tu1")
	}
	if got.Digest != want {
		t.Fatalf("digest = %q, want %q (raw sha256 of the input bytes, 16 hex)", got.Digest, want)
	}
}

func TestToolResultStampCarriesNoDigest(t *testing.T) {
	// Only the call side carries input. A result stamp must not invent one, or
	// a notice could be retired by the wrong half of the pair.
	path := filepath.Join(t.TempDir(), "tx.jsonl")
	appendLines(t, path,
		`{"type":"assistant","timestamp":"t2","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","timestamp":"t3","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}}`)

	tl := &Tailer{Path: path}
	tl.Poll()
	for _, ts := range tl.Tools {
		if ts.Status == "completed" && ts.Digest != "" {
			t.Fatalf("result stamp carries digest %q; only the call side has input", ts.Digest)
		}
	}
}
