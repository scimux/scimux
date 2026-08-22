package app

import (
	"io/fs"
	"net/http"
)

func NewHandler(a *app, web fs.FS) (http.Handler, error) {
	mux, err := newMux(a, web)
	if err != nil {
		return nil, err
	}

	// withGzip is outermost so every completed response can opt in (it only
	// wraps the ResponseWriter). withRequestBoundary then sees every request
	// — Host, Fetch Metadata, anti-framing — before the mutation guard and
	// the mux, so a hostile Host cannot read the token-bearing index or
	// reach a 404/405. 304/204 and already-compressed types are skipped
	// inside the gzip wrapper — see gzip.go.
	return withLocalBoundary(a, mux), nil
}

// withLocalBoundary is the browser/TCP chain, byte-for-byte what NewHandler
// has always returned (FR-15). It is a named function only so the two S3
// boundary constructors can wrap one owned mux without duplicating it; the
// composition itself is unchanged.
func withLocalBoundary(a *app, mux http.Handler) http.Handler {
	return withGzip(withRequestBoundary(a.requestPolicy, guardMutations(mux)))
}

// newMux registers the single owned route table. The inventory, patterns,
// methods and handlers are pinned by the frozen characterization suite; this
// function exists so the same mux can be wrapped by both S3 boundaries
// without the route table being written twice.
func newMux(a *app, web fs.FS) (*http.ServeMux, error) {
	webHandlers, err := newWebHandlers(web)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", webHandlers.assets)
	mux.Handle("GET /css/", webHandlers.css)
	mux.Handle("GET /js/", webHandlers.js)
	mux.Handle("GET /{$}", webHandlers.index)
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
	mux.HandleFunc("POST /api/nodes/{id}/auto-approve", a.handleAutoApprove)
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
	mux.HandleFunc("GET /api/preview", a.handlePreview)
	mux.HandleFunc("GET /api/agents", a.handleAgents)
	mux.HandleFunc("GET /api/ui", a.handleUIGet)
	mux.HandleFunc("PUT /api/ui", a.handleUIPut)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update", a.handleUpdateApply)
	mux.HandleFunc("GET /api/licenses", handleLicenses)

	return mux, nil
}
