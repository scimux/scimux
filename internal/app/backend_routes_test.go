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
	mux, err := newWebMux(webFS, core, newFakeWebRemote("enrolled"))
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

func TestSplitGatewayHidesRemoteNamespaceWhenInactive(t *testing.T) {
	coreCalls := 0
	core := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { coreCalls++ })
	mux, err := newWebMux(webFS, core, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/remote"},
		{http.MethodGet, "/api/remote/status"},
		{http.MethodPost, "/api/remote/pairing"},
		{http.MethodGet, "/api/remote/bootstrap"},
		{http.MethodDelete, "/api/remote/devices/device"},
		{http.MethodGet, "/api/remote/not-a-route"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
		if location := rec.Header().Get("Location"); location != "" {
			t.Errorf("%s %s redirected to %q, want no redirect", tc.method, tc.path, location)
		}
	}
	if coreCalls != 0 {
		t.Fatalf("inactive remote namespace reached the core %d times", coreCalls)
	}
}

func TestSplitGatewayPreservesActiveRemoteMethodSemantics(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	core, err := newCoreMux(a)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := newWebMux(webFS, core, newFakeWebRemote("enrolled"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodGet, "/api/remote/pairing", http.MethodPost},
		{http.MethodPost, "/api/remote/status", http.MethodGet + ", " + http.MethodHead},
		{http.MethodGet, "/api/remote/devices/device", http.MethodDelete + ", " + http.MethodPatch},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, got, tc.allow)
		}
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
