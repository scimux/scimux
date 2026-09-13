package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSplitGatewayOwnsOrForwardsEveryExistingAPIRoute is the control-flow
// completeness check for the split. It derives its cases from the frozen
// route inventory: every action the current web UI can invoke must either be
// implemented by the web generation or cross the muxer capability unchanged.
func TestSplitGatewayOwnsOrForwardsEveryExistingAPIRoute(t *testing.T) {
	core := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux, err := newWebMux(webFS, core, nil)
	if err != nil {
		t.Fatal(err)
	}
	webOwned := map[string]bool{
		"GET /api/update/check":                   true,
		"GET /api/licenses":                       true,
		"POST /api/remote/pairing":                true,
		"GET /api/remote/pairing/{code}":          true,
		"POST /api/remote/pairing/{code}/confirm": true,
		"POST /api/remote/pairing/{code}/cancel":  true,
		"GET /api/remote/devices":                 true,
		"PATCH /api/remote/devices/{id}":          true,
		"DELETE /api/remote/devices/{id}":         true,
		"POST /api/remote/unenroll":               true,
		"GET /api/remote/status":                  true,
		"GET /api/remote/bootstrap":               true,
	}

	for _, route := range characterizationAPIRoutes() {
		t.Run(route.method+" "+route.pattern, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			_, pattern := mux.Handler(req)
			key := route.method + " " + route.pattern
			if webOwned[key] {
				if pattern != key {
					t.Fatalf("web-owned route resolved as %q, want %q", pattern, key)
				}
				return
			}
			if pattern != "/api/" {
				t.Fatalf("muxer-owned route resolved as %q, want /api/ proxy", pattern)
			}
		})
	}
}

func TestCoreMuxExposesOnlyAPI(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	core, err := newCoreMux(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/js/app.js", "/css/base.css", "/assets/agents/openai.svg"} {
		rec := httptest.NewRecorder()
		core.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("core GET %s = %d, want 404", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	core.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("core GET /api/state = %d (%s)", rec.Code, rec.Body.String())
	}
}
