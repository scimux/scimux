// ui_state_api.go — opaque UI JSON ETag read/revision/write storage.
//
// Ownership / locks (existing boundaries; this file does not change them):
//   - Request-body reading and validation (size, JSON, If-Match presence)
//     occur before uiMu.
//   - GET holds uiMu only while reading the file (readUILocked).
//   - PUT holds uiMu across current-state read, revision comparison, sibling
//     .tmp write, and rename.
//   - uiMu is independent of a.mu (pure private-document I/O; must not stall
//     the poller).
//   - Response encoding (ETag header + writeJSON) occurs after the successful
//     atomic replace; the existing contract deliberately does not fsync.
//   - uiMu/uiPath fields and construction remain in app.go; router registration
//     remains in router.go; shared JSON response encoding lives in security.go.
//   - search.go's read-only bookmark scan of uiPath is unchanged.
package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
)

// ---------- UI state ----------

// The web client owns a small blob of cross-device state — map group tabs,
// the archived-card set, private notes — that must survive scimux restarts,
// so it lives next to the node store instead of in localStorage (per-device
// state like drafts stays client-side). The blob is opaque JSON to the
// server: its shape belongs to the client.
//
// Writes are revisioned, not last-writer-wins: every GET carries an ETag
// derived from the stored bytes, every PUT must name the revision it was
// based on (If-Match), and a stale base gets 409 — the client refetches,
// replays its local operations on the fresh document, and retries. That is
// what lets two of the supervisor's devices add notes concurrently without
// silently erasing each other. Clients poll GET with If-None-Match (304) on
// the same cadence as /api/state.
const uiStateMax = 1 << 20

// uiETag derives the revision tag from the canonical stored bytes.
func uiETag(b []byte) string {
	h := fnv.New64a()
	h.Write(b)
	return fmt.Sprintf(`"%x"`, h.Sum64())
}

// readUILocked returns the stored UI document, "{}" when none exists yet.
// Any error other than not-exist is a real storage failure the client must
// see — reporting it as an empty document would invite the next mutation to
// overwrite whatever the unreadable file still holds. Callers hold a.uiMu.
func (a *app) readUILocked() ([]byte, error) {
	b, err := os.ReadFile(a.uiPath)
	if os.IsNotExist(err) {
		return []byte("{}"), nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("stored ui state is not valid JSON")
	}
	return b, nil
}

func (a *app) handleUIGet(w http.ResponseWriter, r *http.Request) {
	a.uiMu.Lock()
	b, err := a.readUILocked()
	a.uiMu.Unlock()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	etag := uiETag(b)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (a *app) handleUIPut(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, uiStateMax+1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if len(b) > uiStateMax {
		http.Error(w, "ui state too large", 413)
		return
	}
	if !json.Valid(b) {
		http.Error(w, "ui state must be valid JSON", 400)
		return
	}
	match := r.Header.Get("If-Match")
	if match == "" {
		http.Error(w, "ui writes require If-Match (use * to bootstrap)", 428)
		return
	}
	// Atomic replace under the UI lock (not a.mu): a crash mid-write must never
	// leave a truncated file, concurrent PUTs must not interleave tmp files, and
	// the revision check must be atomic with the write it guards — but this is
	// pure I/O over a private document, so it must not stall the poller.
	a.uiMu.Lock()
	defer a.uiMu.Unlock()
	cur, err := a.readUILocked()
	if err != nil {
		http.Error(w, "read ui state: "+err.Error(), 500)
		return
	}
	if match != "*" && match != uiETag(cur) {
		http.Error(w, "ui state changed since this revision was read", 409)
		return
	}
	// Private notes live here: owner-only permissions. The tmp write + rename is
	// atomic for content (rename swaps the inode), which is the guarantee this
	// per-device UI blob needs; unlike the audit store it is not fsync'd — a lost
	// note after a host crash is recoverable, a corrupted nodes.jsonl is not.
	tmp := a.uiPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := os.Rename(tmp, a.uiPath); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("ETag", uiETag(b))
	writeJSON(w, map[string]string{"ok": "saved"})
}
