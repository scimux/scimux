package app

// Packet 4F retained-feature public-route coverage through NewHandler.
//
// Closes genuine public-binding gaps for the notes family and update routes.
// Detailed direct-handler matrices remain the behavioral authority:
//
//	notes:    notes_http_test.go (+ internal/notestore)
//	update:   update_test.go (download/checksum/install/refusal; never re-exec)
//
// Explicit map of already-covered public representative bindings reused here
// (do not duplicate their detailed matrices merely to grow volume):
//
//	state     — TestCharacterizationRepresentativeBindings, state_api_test.go
//	usage     — TestCharacterizationRepresentativeBindings, usage_test.go
//	search    — TestCharacterizationRepresentativeBindings, search_test.go
//	archived  — TestCharacterizationRepresentativeBindings, archived_test.go
//	agents    — TestPublicRouteAgents (node_api_test.go)
//	UI state  — ui_state_api_test.go public suite
//	licenses  — TestCharacterizationRepresentativeBindings, TestLicensesEmbedded,
//	            TestNewHandlerStaticDoesNotShadowAPI / RepresentativeBindings

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/notestore"
)

func retainedPublicHandler(t *testing.T, a *app) http.Handler {
	t.Helper()
	h, err := NewHandler(a, webFS)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestPublicRouteNotesLifecycle exercises the complete notes route family
// through NewHandler as one private-store lifecycle. Complements — does not
// replace — the detailed direct notestore/HTTP tests in notes_http_test.go.
func TestPublicRouteNotesLifecycle(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := retainedPublicHandler(t, a)

	// GET list — empty sparse list.
	rec := routeRequest(h, http.MethodGet, "/api/notes", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Notes []json.RawMessage `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list empty json: %v", err)
	}
	if len(listResp.Notes) != 0 {
		t.Fatalf("list empty: got %d notes, want 0", len(listResp.Notes))
	}

	// POST create — bodyless + CSRF (no Content-Type required).
	rec = routeRequest(h, http.MethodPost, "/api/notes", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var created notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create json: %v", err)
	}
	if created.ID == "" || len(created.Sections) == 0 {
		t.Fatalf("create shape: %+v", created)
	}
	sid := created.Sections[0].ID

	// GET created note.
	rec = routeRequest(h, http.MethodGet, "/api/notes/"+created.ID, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("get created: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var got notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("get json: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("get id = %q, want %q", got.ID, created.ID)
	}

	// PATCH note title + section body.
	patch := `{"title":"Public Route Note","section":{"id":` + jsonQuote(sid) + `,"body":"lifecycle body"}}`
	rec = routeRequest(h, http.MethodPatch, "/api/notes/"+created.ID, patch, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("patch json: %v", err)
	}
	if got.Title != "Public Route Note" || got.Sections[0].Body != "lifecycle body" {
		t.Fatalf("patch not applied: title=%q body=%q", got.Title, got.Sections[0].Body)
	}

	// POST section reference.
	refBody := `{"source":{"uid":"u-pub","segment":1,"record":2,"node":"n1"},` +
		`"snapshot":{"lane":"#111111","station":"n1","speaker":"user","text":"ref text"}}`
	rec = routeRequest(h, http.MethodPost,
		"/api/notes/"+created.ID+"/sections/"+sid+"/references", refBody, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("add reference: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("add reference json: %v", err)
	}
	if len(got.Sections[0].References) != 1 {
		t.Fatalf("add reference: got %d refs, want 1", len(got.Sections[0].References))
	}
	refID := got.Sections[0].References[0].ID
	if refID == "" {
		t.Fatal("add reference: empty server-minted id")
	}

	// DELETE section reference.
	// Fresh target: empty references use omitempty, so reusing `got` would
	// keep the prior slice when the field is absent from the response body.
	rec = routeRequest(h, http.MethodDelete,
		"/api/notes/"+created.ID+"/sections/"+sid+"/references/"+refID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("trash reference: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var afterTrash notestore.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &afterTrash); err != nil {
		t.Fatalf("trash reference json: %v", err)
	}
	if len(afterTrash.Sections[0].References) != 0 {
		t.Fatalf("trash reference: still %d refs", len(afterTrash.Sections[0].References))
	}

	// DELETE note.
	rec = routeRequest(h, http.MethodDelete, "/api/notes/"+created.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete note: status = %d, body=%q", rec.Code, rec.Body.String())
	}

	// GET after delete → not found.
	rec = routeRequest(h, http.MethodGet, "/api/notes/"+created.ID, "", false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: status = %d, want 404; body=%q", rec.Code, rec.Body.String())
	}

	// List is empty again.
	rec = routeRequest(h, http.MethodGet, "/api/notes", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after delete: status = %d", rec.Code)
	}
	listResp = struct {
		Notes []json.RawMessage `json:"notes"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list after delete json: %v", err)
	}
	if len(listResp.Notes) != 0 {
		t.Fatalf("list after delete: got %d notes, want 0", len(listResp.Notes))
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestPublicRouteUpdateCheckAndApplyValidation exercises update routes through
// NewHandler only:
//   - GET /api/update/check against a private httptest Forgejo
//   - POST /api/update only through the pre-network missing/empty expected_tag path
//
// Detailed download/checksum/install/refusal coverage stays in update_test.go.
// Process-global update seams are sequential (no t.Parallel).
func TestPublicRouteUpdateCheckAndApplyValidation(t *testing.T) {
	// Do not run in parallel: withUpdateSeams mutates package-level releaseAPIBase/version.
	srv := fakeForgejo(t, "v9.9.9", nil)
	withUpdateSeams(t, srv.URL, "v1.0.0")

	a := newTestApp(t, &fakeTmux{})
	h := retainedPublicHandler(t, a)

	// GET check via public wrapper → available update for older current version.
	rec := routeRequest(h, http.MethodGet, "/api/update/check", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("update check: status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var check struct {
		Current, Latest string
		Available       bool
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &check); err != nil {
		t.Fatalf("update check json: %v", err)
	}
	if check.Current != "v1.0.0" || check.Latest != "v9.9.9" || !check.Available {
		t.Fatalf("update check payload = %+v, want current=v1.0.0 latest=v9.9.9 available=true", check)
	}

	// POST apply: missing expected_tag (empty body) → 400 before any network download.
	// Bodyless is allowed by the mutation guard; decodeJSON fails → 400.
	rec = routeRequest(h, http.MethodPost, "/api/update", "", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("apply missing body: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "checksum") ||
		strings.Contains(strings.ToLower(rec.Body.String()), "download") {
		t.Fatalf("apply missing body must not reach download path: %q", rec.Body.String())
	}

	// POST apply: empty expected_tag → 400 pre-network validation.
	rec = routeRequest(h, http.MethodPost, "/api/update", `{"expected_tag":""}`, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("apply empty tag: status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "expected_tag") {
		t.Fatalf("apply empty tag body should mention expected_tag: %q", rec.Body.String())
	}

	// Never exercise install/re-exec through the public wrapper in this packet.
}

// TestPublicRouteRetainedRepresentatives documents and re-asserts the existing
// NewHandler representative bindings for retained/extracted routes that already
// have public coverage elsewhere — a single smoke map, not a behavioral matrix.
func TestPublicRouteRetainedRepresentatives(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	h := retainedPublicHandler(t, a)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		csrf   bool
		want   int
	}{
		// Reused representatives (see file header map).
		{"state", http.MethodGet, "/api/state", "", false, http.StatusOK},
		{"usage", http.MethodGet, "/api/usage", "", false, http.StatusOK},
		{"search short", http.MethodGet, "/api/search?q=a", "", false, http.StatusOK},
		{"archived missing uid", http.MethodGet, "/api/archived", "", false, http.StatusBadRequest},
		{"licenses", http.MethodGet, "/api/licenses", "", false, http.StatusOK},
		{"ui get bootstrap", http.MethodGet, "/api/ui", "", false, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := routeRequest(h, tt.method, tt.path, tt.body, tt.csrf)
			if rec.Code != tt.want {
				t.Fatalf("%s %s status = %d, want %d; body=%q",
					tt.method, tt.path, rec.Code, tt.want, rec.Body.String())
			}
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s reached generic 404", tt.method, tt.path)
			}
		})
	}
}
