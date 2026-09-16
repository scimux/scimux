package remote

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func setViewerOriginForTest(t *testing.T, cfg *Config, origin string) {
	t.Helper()
	v := reflect.ValueOf(cfg).Elem().FieldByName("ViewerOrigin")
	if !v.IsValid() {
		t.Fatal("remote.Config has no ViewerOrigin field")
	}
	v.SetString(origin)
}

func TestViewerOriginValidationForDirectConfigCallers(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		ok     bool
	}{
		{"official", "https://my.scimux.com", true},
		{"custom https", "https://view.example:8443", true},
		{"trailing slash", "https://view.example/", true},
		{"ipv6", "https://[2001:db8::1]:8443", true},
		{"empty uses default", "", true},
		{"http loopback", "http://127.0.0.1:8080", false},
		{"insecure remote", "http://view.example", false},
		{"userinfo", "https://user@view.example", false},
		{"path", "https://view.example/p", false},
		{"query", "https://view.example?x=1", false},
		{"empty query", "https://view.example?", false},
		{"fragment", "https://view.example#x", false},
		{"empty fragment", "https://view.example#", false},
		{"scheme relative", "//view.example", false},
		{"bad port", "https://view.example:bad", false},
		{"port too high", "https://view.example:99999", false},
		{"zero port", "https://view.example:0", false},
		{"empty port", "https://view.example:", false},
		{"default port", "https://view.example:443", false},
		{"leading-zero port", "https://view.example:08443", false},
		{"uppercase host", "https://View.Example", false},
		{"uppercase scheme", "HTTPS://view.example", false},
		{"noncanonical ipv6", "https://[2001:0db8:0:0:0:0:0:1]", false},
		{"ipv6 zone", "https://[fe80::1%25lo0]", false},
		{"unicode host", "https://v\u00edew.example", false},
		{"short numeric host", "https://127.1", false},
		{"integer numeric host", "https://2130706433", false},
		{"hex numeric host", "https://0x7f000001", false},
		{"octal numeric host", "https://017700000001", false},
		{"mixed numeric host", "https://0x7f.1", false},
		{"numeric trailing dot", "https://127.1.", false},
		{"numeric final label", "https://example.123", false},
		{"root hostname", "https://.", false},
		{"double slash", "https://view.example//", false},
		{"ftp", "ftp://view.example", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, pc, _ := mintedInvite(t)
			setViewerOriginForTest(t, &c.cfg, tt.origin)
			_, err := c.PairingLink(pc)
			if (err == nil) != tt.ok {
				t.Fatalf("PairingLink with ViewerOrigin %q error = %v, want ok=%v", tt.origin, err, tt.ok)
			}
		})
	}
}

func TestInvalidViewerOriginFailsBeforeRemoteWork(t *testing.T) {
	cfg := clientCfg(t, nil)
	setViewerOriginForTest(t, &cfg, "https://viewer.example/path")
	called := false
	cfg.Hooks.OnRemoteInit = func() { called = true }
	c := NewClient(cfg)
	ctx, cancel := ctxTO(t)
	defer cancel()
	err := c.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "viewer origin") {
		t.Fatalf("Start error = %v, want viewer-origin validation failure", err)
	}
	if called {
		t.Fatal("invalid ViewerOrigin reached remote initialization")
	}
	if _, statErr := os.Stat(c.PrivateDir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid ViewerOrigin touched remote state: stat error = %v", statErr)
	}
}

func TestNilClientHasNoViewerOrigin(t *testing.T) {
	var c *Client
	if _, err := c.validatedViewerOrigin(); err == nil {
		t.Fatal("nil client returned a viewer origin")
	}
}
