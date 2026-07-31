package main

import (
	"fmt"
	"io/fs"
	"net/http"
)

func NewHandler(a *app, web fs.FS) (http.Handler, error) {
	assetsInfo, err := fs.Stat(web, "web/assets")
	if err != nil {
		return nil, fmt.Errorf("embedded web/assets missing: %w", err)
	}
	if !assetsInfo.IsDir() {
		return nil, fmt.Errorf("embedded web/assets is not a directory")
	}
	assets, err := fs.Sub(web, "web/assets")
	if err != nil {
		return nil, fmt.Errorf("embedded web/assets missing: %w", err)
	}
	indexHTML, err := csrfIndex(web)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b := indexHTML
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The UI is embedded in the binary and changes with every build;
		// a cached copy after a scimux upgrade is a recurring dogfooding
		// trap (especially iPad Safari). It's one small local page: always
		// fetch fresh.
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

	return guardMutations(mux), nil
}
