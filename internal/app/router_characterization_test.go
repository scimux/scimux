package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// characterizationAPIRoute is the single route inventory for runtime OPTIONS/
// Allow checks and the source/AST ownership audit (Packet 4F). method+pattern
// must match router.go registrations; handler is the registered symbol name;
// home is the expected single production definition file under package app.
type characterizationAPIRoute struct {
	method  string
	pattern string
	path    string
	handler string
	home    string
}

func characterizationAPIRoutes() []characterizationAPIRoute {
	return []characterizationAPIRoute{
		// Order matches router.go HandleFunc registrations exactly.
		// Homes: extracted (*_api.go / security.go) vs retained feature files.
		{http.MethodGet, "/api/state", "/api/state", "handleState", "state_api.go"},                                                                                                     // extracted
		{http.MethodGet, "/api/usage", "/api/usage", "handleUsage", "usage.go"},                                                                                                         // retained
		{http.MethodPost, "/api/nodes", "/api/nodes", "handleNewNode", "node_api.go"},                                                                                                   // extracted
		{http.MethodPatch, "/api/nodes/{id}", "/api/nodes/node-1", "handleUpdateNode", "node_api.go"},                                                                                   // extracted
		{http.MethodDelete, "/api/nodes/{id}", "/api/nodes/node-1", "handleDeleteNode", "node_api.go"},                                                                                  // extracted
		{http.MethodPost, "/api/nodes/{id}/exit", "/api/nodes/node-1/exit", "handleExitNode", "node_api.go"},                                                                            // extracted
		{http.MethodPost, "/api/adopt", "/api/adopt", "handleAdopt", "node_api.go"},                                                                                                     // extracted
		{http.MethodPost, "/api/nodes/{id}/send", "/api/nodes/node-1/send", "handleSend", "conversation_api.go"},                                                                        // extracted
		{http.MethodPost, "/api/nodes/{id}/attachments", "/api/nodes/node-1/attachments", "handleUploadAttachments", "attachment_api.go"},                                               // extracted
		{http.MethodGet, "/api/nodes/{id}/attachments/{name}", "/api/nodes/node-1/attachments/file.txt", "handleAttachment", "attachment_api.go"},                                       // extracted
		{http.MethodGet, "/api/nodes/{id}/assets/{assetID}", "/api/nodes/node-1/assets/asset-1", "handleAsset", "attachment_api.go"},                                                    // extracted
		{http.MethodPost, "/api/nodes/{id}/send/resolve", "/api/nodes/node-1/send/resolve", "handleSendResolve", "conversation_api.go"},                                                 // extracted
		{http.MethodPost, "/api/nodes/{id}/send/interrupt", "/api/nodes/node-1/send/interrupt", "handleSendInterrupt", "conversation_api.go"},                                           // extracted
		{http.MethodPost, "/api/nodes/{id}/key", "/api/nodes/node-1/key", "handleKey", "conversation_api.go"},                                                                           // extracted
		{http.MethodPost, "/api/nodes/{id}/auto-approve", "/api/nodes/node-1/auto-approve", "handleAutoApprove", "auto_approve.go"},                                                     // P3
		{http.MethodGet, "/api/nodes/{id}/chat", "/api/nodes/node-1/chat", "handleChat", "conversation_api.go"},                                                                         // extracted
		{http.MethodGet, "/api/nodes/{id}/peek", "/api/nodes/node-1/peek", "handlePeek", "conversation_api.go"},                                                                         // extracted
		{http.MethodGet, "/api/notes", "/api/notes", "handleNoteList", "notes.go"},                                                                                                      // retained
		{http.MethodPost, "/api/notes", "/api/notes", "handleNoteCreate", "notes.go"},                                                                                                   // retained
		{http.MethodGet, "/api/notes/{id}", "/api/notes/note-1", "handleNoteGet", "notes.go"},                                                                                           // retained
		{http.MethodPatch, "/api/notes/{id}", "/api/notes/note-1", "handleNotePatch", "notes.go"},                                                                                       // retained
		{http.MethodDelete, "/api/notes/{id}", "/api/notes/note-1", "handleNoteDelete", "notes.go"},                                                                                     // retained
		{http.MethodPost, "/api/notes/{id}/sections/{sectionID}/references", "/api/notes/note-1/sections/section-1/references", "handleNoteAddReference", "notes.go"},                   // retained
		{http.MethodDelete, "/api/notes/{id}/sections/{sectionID}/references/{refID}", "/api/notes/note-1/sections/section-1/references/ref-1", "handleNoteTrashReference", "notes.go"}, // retained
		{http.MethodGet, "/api/search", "/api/search", "handleSearch", "search.go"},                                                                                                     // retained
		{http.MethodGet, "/api/preview", "/api/preview", "handlePreview", "preview.go"},                                                                                                 // retained
		{http.MethodGet, "/api/agents", "/api/agents", "handleAgents", "agents.go"},                                                                                                     // retained
		{http.MethodGet, "/api/ui", "/api/ui", "handleUIGet", "ui_state_api.go"},                                                                                                        // extracted
		{http.MethodPut, "/api/ui", "/api/ui", "handleUIPut", "ui_state_api.go"},                                                                                                        // extracted
		{http.MethodGet, "/api/update/check", "/api/update/check", "handleUpdateCheck", "update.go"},                                                                                    // retained
		{http.MethodPost, "/api/update", "/api/update", "handleUpdateApply", "update.go"},                                                                                               // retained
		{http.MethodGet, "/api/licenses", "/api/licenses", "handleLicenses", "update.go"},                                                                                               // retained
		{http.MethodPost, "/api/remote/pairing", "/api/remote/pairing", "handleRemotePairingMint", "remote_pairing.go"},                                                                 // S7b
		{http.MethodGet, "/api/remote/pairing/{code}", "/api/remote/pairing/04106105", "handleRemotePairingState", "remote_pairing.go"},                                                 // S7b
		{http.MethodPost, "/api/remote/pairing/{code}/confirm", "/api/remote/pairing/04106105/confirm", "handleRemotePairingConfirm", "remote_pairing.go"},                              // S7b
		{http.MethodPost, "/api/remote/pairing/{code}/cancel", "/api/remote/pairing/04106105/cancel", "handleRemotePairingCancel", "remote_pairing.go"},                                 // S7b
		{http.MethodGet, "/api/remote/devices", "/api/remote/devices", "handleRemoteDeviceList", "remote_pairing.go"},                                                                   // S7b
		{http.MethodDelete, "/api/remote/devices/{id}", "/api/remote/devices/phone", "handleRemoteDeviceRevoke", "remote_pairing.go"},                                                   // S7b
	}
}

func TestCharacterizationAPIRouteInventory(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))

	for _, route := range characterizationAPIRoutes() {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:8787"+route.path, nil)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("OPTIONS %s status = %d, want 405", route.path, rec.Code)
			}
			if !allowContains(rec.Header().Get("Allow"), route.method) {
				t.Fatalf("OPTIONS %s Allow = %q, want token %q", route.path, rec.Header().Get("Allow"), route.method)
			}
		})
	}
}

func TestCharacterizationRepresentativeBindings(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
		csrf        bool
		want        int
	}{
		{"state", http.MethodGet, "/api/state", "", "", false, http.StatusOK},
		{"usage", http.MethodGet, "/api/usage", "", "", false, http.StatusOK},
		{"notes", http.MethodGet, "/api/notes", "", "", false, http.StatusOK},
		{"search too short", http.MethodGet, "/api/search?q=a", "", "", false, http.StatusOK},
		{"preview missing target", http.MethodGet, "/api/preview", "", "", false, http.StatusBadRequest},
		{"licenses", http.MethodGet, "/api/licenses", "", "", false, http.StatusOK},
		{"new node invalid", http.MethodPost, "/api/nodes", `{}`, "application/json", true, http.StatusBadRequest},
		{"adopt invalid", http.MethodPost, "/api/adopt", `{}`, "application/json", true, http.StatusBadRequest},
		{"ui put missing if-match", http.MethodPut, "/api/ui", `{}`, "application/json", true, http.StatusPreconditionRequired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://127.0.0.1:8787"+tt.path, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.csrf {
				req.Header.Set("X-Scimux-CSRF", csrfToken)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Fatalf("%s %s status = %d, want %d; body=%q", tt.method, tt.path, rec.Code, tt.want, rec.Body.String())
			}
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s reached generic 404", tt.method, tt.path)
			}
		})
	}
}

func TestCharacterizationMutationGuard(t *testing.T) {
	h := newTestHandler(t, newTestApp(t, &fakeTmux{}))

	for _, route := range characterizationAPIRoutes() {
		if route.method == http.MethodGet {
			continue
		}
		t.Run(route.method+" "+route.pattern+" without csrf", func(t *testing.T) {
			req := httptest.NewRequest(route.method, "http://127.0.0.1:8787"+route.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s %s without CSRF status = %d, want 403; body=%q", route.method, route.path, rec.Code, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodPut, "http://127.0.0.1:8787/api/usage", http.NoBody)
	req.Header.Set("X-Scimux-CSRF", csrfToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/usage with CSRF status = %d, want 405; body=%q", rec.Code, rec.Body.String())
	}
	if !allowContains(rec.Header().Get("Allow"), http.MethodGet) {
		t.Fatalf("PUT /api/usage Allow = %q, want token %q", rec.Header().Get("Allow"), http.MethodGet)
	}
}

func allowContains(header, want string) bool {
	for method := range strings.SplitSeq(header, ",") {
		if strings.TrimSpace(method) == want {
			return true
		}
	}
	return false
}
