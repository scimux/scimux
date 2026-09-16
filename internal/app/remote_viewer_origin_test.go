package app

import (
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
)

func reflectedString(t *testing.T, value any, field string) string {
	t.Helper()
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	v = v.FieldByName(field)
	if !v.IsValid() {
		t.Fatalf("%T has no %s field", value, field)
	}
	return v.String()
}

func TestViewerOriginFlagReachesRemoteConfig(t *testing.T) {
	cmd := runCommand(t, "-viewer-origin", "https://viewer.example/")
	if got := reflectedString(t, cmd.Config, "ViewerOrigin"); got != "https://viewer.example" {
		t.Fatalf("Config.ViewerOrigin = %q, want normalized custom viewer", got)
	}
	if cmd.Config.Origin != "" {
		t.Fatalf("viewer flag changed cryptographic Origin to %q", cmd.Config.Origin)
	}
}

func TestViewerOriginFlagRejectsUnsafeForms(t *testing.T) {
	for _, bad := range []string{
		"http://viewer.example", "https://user@viewer.example", "https://viewer.example/p",
		"https://viewer.example?q=1", "https://viewer.example#fragment", "//viewer.example",
	} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := flagCommand(t, loopback("-viewer-origin", bad)...)
			if err == nil || !strings.Contains(err.Error(), "viewer-origin") {
				t.Fatalf("error = %v, want viewer-origin refusal", err)
			}
		})
	}
}

func TestViewerOriginSurvivesSupervisorAndConfigurationPipe(t *testing.T) {
	cmd := runCommand(t, "-viewer-origin", "https://viewer.example")
	s, err := newWebSupervisor(testListener(t), testBackendLink(t), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := reflectedString(t, s.config, "ViewerOrigin"); got != "https://viewer.example" {
		t.Fatalf("supervisor ViewerOrigin = %q", got)
	}
	s.config.Generation = 1
	b, err := json.Marshal(s.config)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readWebChildConfig(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if got := reflectedString(t, cfg, "ViewerOrigin"); got != "https://viewer.example" {
		t.Fatalf("pipe ViewerOrigin = %q", got)
	}
}

func TestOlderWebChildConfigurationGetsOfficialViewer(t *testing.T) {
	cfg := validWebChildConfigForViewerTest(t)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readWebChildConfig(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if viewer := reflectedString(t, got, "ViewerOrigin"); viewer != "https://my.scimux.com" {
		t.Fatalf("legacy config ViewerOrigin = %q, want official viewer", viewer)
	}
}

func TestConfigurationPipeRejectsUnsafeViewerOrigin(t *testing.T) {
	cfg := validWebChildConfigForViewerTest(t)
	cfg.ViewerOrigin = "https://viewer.example/path"
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readWebChildConfig(strings.NewReader(string(b))); err == nil || !strings.Contains(err.Error(), "viewer origin") {
		t.Fatalf("readWebChildConfig error = %v, want viewer-origin refusal", err)
	}
}

func TestSupervisorRejectsUnsafeViewerOrigin(t *testing.T) {
	cmd := &Command{Config: remote.Config{DataDir: t.TempDir(), ViewerOrigin: "https://viewer.example/path"}}
	if _, err := newWebSupervisor(testListener(t), testBackendLink(t), cmd); err == nil || !strings.Contains(err.Error(), "viewer origin") {
		t.Fatalf("newWebSupervisor error = %v, want viewer-origin refusal", err)
	}
}

func testListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func testBackendLink(t *testing.T) backend.Link {
	t.Helper()
	return backend.Link{Socket: "/tmp/scimux-test.sock", Token: strings.Repeat("a", 64)}
}

func validWebChildConfigForViewerTest(t *testing.T) webChildConfig {
	t.Helper()
	return webChildConfig{Link: testBackendLink(t), Generation: 1, ListenAddr: "127.0.0.1:8787", DataDir: t.TempDir(), CSRFToken: strings.Repeat("b", 64)}
}
