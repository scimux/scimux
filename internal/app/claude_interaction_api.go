package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/scimux/scimux/internal/sessionworker"
)

// handleClaudeInteraction queues one human decision for the owning Claude
// worker. 202 means the worker recorded it. It does not mean Claude consumed it.
func (a *app) handleClaudeInteraction(w http.ResponseWriter, r *http.Request) {
	n, ok := a.node(r)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if a.refuseRetiredExternal(w, n) || a.refuseEnded(w, n) {
		return
	}
	if n.Agent != "claude" || a.workers == nil || !a.workers.manages(n.ID) {
		http.Error(w, "claude interaction is unsupported", http.StatusNotFound)
		return
	}
	if !bundleSupportsInteraction(a.claudeHookBundle(n)) {
		http.Error(w, "claude interaction is unsupported", http.StatusNotFound)
		return
	}
	body, err := readInteractionPost(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var action sessionworker.InteractionAction
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&action); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch action.Action {
	case "answer", "discuss", "approve", "request_changes", "cancel", "terminal":
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if action.RequestID == "" || strings.TrimSpace(action.RequestID) != action.RequestID {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	result, err := a.workers.SubmitClaudeInteraction(n.ID, action)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, sessionworker.ErrInteractionUnsupported):
			status = http.StatusNotFound
			http.Error(w, "claude interaction is unsupported", status)
		case errors.Is(err, sessionworker.ErrInteractionBadRequest):
			status = http.StatusBadRequest
			http.Error(w, "bad request", status)
		case errors.Is(err, sessionworker.ErrConflict):
			status = http.StatusConflict
			http.Error(w, err.Error(), status)
		default:
			http.Error(w, err.Error(), status)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(result)
}

func readInteractionPost(body io.ReadCloser) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(nil, body, interactionPostMax))
}
