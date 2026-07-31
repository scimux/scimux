package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type characterizationAPIRoute struct {
	method  string
	pattern string
	path    string
}

func characterizationAPIRoutes() []characterizationAPIRoute {
	return []characterizationAPIRoute{
		{http.MethodGet, "/api/state", "/api/state"},
		{http.MethodGet, "/api/usage", "/api/usage"},
		{http.MethodPost, "/api/nodes", "/api/nodes"},
		{http.MethodPatch, "/api/nodes/{id}", "/api/nodes/node-1"},
		{http.MethodDelete, "/api/nodes/{id}", "/api/nodes/node-1"},
		{http.MethodPost, "/api/nodes/{id}/exit", "/api/nodes/node-1/exit"},
		{http.MethodPost, "/api/adopt", "/api/adopt"},
		{http.MethodPost, "/api/nodes/{id}/send", "/api/nodes/node-1/send"},
		{http.MethodPost, "/api/nodes/{id}/attachments", "/api/nodes/node-1/attachments"},
		{http.MethodGet, "/api/nodes/{id}/attachments/{name}", "/api/nodes/node-1/attachments/file.txt"},
		{http.MethodGet, "/api/nodes/{id}/assets/{assetID}", "/api/nodes/node-1/assets/asset-1"},
		{http.MethodPost, "/api/nodes/{id}/send/resolve", "/api/nodes/node-1/send/resolve"},
		{http.MethodPost, "/api/nodes/{id}/send/interrupt", "/api/nodes/node-1/send/interrupt"},
		{http.MethodPost, "/api/nodes/{id}/key", "/api/nodes/node-1/key"},
		{http.MethodGet, "/api/nodes/{id}/chat", "/api/nodes/node-1/chat"},
		{http.MethodGet, "/api/nodes/{id}/peek", "/api/nodes/node-1/peek"},
		{http.MethodGet, "/api/notes", "/api/notes"},
		{http.MethodPost, "/api/notes", "/api/notes"},
		{http.MethodGet, "/api/notes/{id}", "/api/notes/note-1"},
		{http.MethodPatch, "/api/notes/{id}", "/api/notes/note-1"},
		{http.MethodDelete, "/api/notes/{id}", "/api/notes/note-1"},
		{http.MethodPost, "/api/notes/{id}/sections/{sectionID}/references", "/api/notes/note-1/sections/section-1/references"},
		{http.MethodDelete, "/api/notes/{id}/sections/{sectionID}/references/{refID}", "/api/notes/note-1/sections/section-1/references/ref-1"},
		{http.MethodGet, "/api/search", "/api/search"},
		{http.MethodGet, "/api/archived", "/api/archived"},
		{http.MethodGet, "/api/agents", "/api/agents"},
		{http.MethodGet, "/api/ui", "/api/ui"},
		{http.MethodPut, "/api/ui", "/api/ui"},
		{http.MethodGet, "/api/update/check", "/api/update/check"},
		{http.MethodPost, "/api/update", "/api/update"},
		{http.MethodGet, "/api/licenses", "/api/licenses"},
	}
}

func newCharacterizationHandler(t *testing.T, a *app) http.Handler {
	t.Helper()

	// Temporary complete route source for Phase 0 characterization. Packet 1B
	// deletes this copy when production NewHandler becomes authoritative.
	mux := http.NewServeMux()
	assets, err := fs.Sub(webFS, "web/assets")
	if err != nil {
		t.Fatalf("sub web/assets: %v", err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	indexHTML := csrfIndex(webFS)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b := indexHTML
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/usage", a.handleUsage)
	mux.HandleFunc("POST /api/nodes", a.handleNewNode)
	mux.HandleFunc("PATCH /api/nodes/{id}", a.handleUpdateNode)
	mux.HandleFunc("DELETE /api/nodes/{id}", a.handleDeleteNode)
	mux.HandleFunc("POST /api/nodes/{id}/exit", a.handleExitNode)
	mux.HandleFunc("POST /api/adopt", a.handleAdopt)
	mux.HandleFunc("POST /api/nodes/{id}/send", a.handleSend)
	mux.HandleFunc("POST /api/nodes/{id}/attachments", a.handleUploadAttachments)
	mux.HandleFunc("GET /api/nodes/{id}/attachments/{name}", a.handleAttachment)
	mux.HandleFunc("GET /api/nodes/{id}/assets/{assetID}", a.handleAsset)
	mux.HandleFunc("POST /api/nodes/{id}/send/resolve", a.handleSendResolve)
	mux.HandleFunc("POST /api/nodes/{id}/send/interrupt", a.handleSendInterrupt)
	mux.HandleFunc("POST /api/nodes/{id}/key", a.handleKey)
	mux.HandleFunc("GET /api/nodes/{id}/chat", a.handleChat)
	mux.HandleFunc("GET /api/nodes/{id}/peek", a.handlePeek)
	mux.HandleFunc("GET /api/notes", a.handleNoteList)
	mux.HandleFunc("POST /api/notes", a.handleNoteCreate)
	mux.HandleFunc("GET /api/notes/{id}", a.handleNoteGet)
	mux.HandleFunc("PATCH /api/notes/{id}", a.handleNotePatch)
	mux.HandleFunc("DELETE /api/notes/{id}", a.handleNoteDelete)
	mux.HandleFunc("POST /api/notes/{id}/sections/{sectionID}/references", a.handleNoteAddReference)
	mux.HandleFunc("DELETE /api/notes/{id}/sections/{sectionID}/references/{refID}", a.handleNoteTrashReference)
	mux.HandleFunc("GET /api/search", a.handleSearch)
	mux.HandleFunc("GET /api/archived", a.handleArchived)
	mux.HandleFunc("GET /api/agents", a.handleAgents)
	mux.HandleFunc("GET /api/ui", a.handleUIGet)
	mux.HandleFunc("PUT /api/ui", a.handleUIPut)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update", a.handleUpdateApply)
	mux.HandleFunc("GET /api/licenses", handleLicenses)

	return guardMutations(mux)
}

func TestCharacterizationAPIRouteInventory(t *testing.T) {
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

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
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

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
		{"archived missing uid", http.MethodGet, "/api/archived", "", "", false, http.StatusBadRequest},
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
	h := newCharacterizationHandler(t, newTestApp(t, &fakeTmux{}))

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
