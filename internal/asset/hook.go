package asset

// IngestFunc is the turn-append ingestion hook (Phase 4): called once per
// completed user/assistant/tool session-log record with every local-path
// candidate scanned from it (see Candidates, ScanMarkdown, ScanToolOutput).
// The transport managers (internal/acp, internal/acp/codex, and the tmux
// mirror in package main) call it right after the record itself is durably
// appended — never from a read path — so a short-lived agent process's temp
// files are captured before they can disappear (upload-design.md, "Ingestion
// Timing"). A nil hook disables scanning, the default for tests that don't
// exercise assets.
type IngestFunc func(nodeID, dir string, cands []Candidate)
