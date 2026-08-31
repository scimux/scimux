package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

func TestS5R4_F6_TerminalOpenFailure(t *testing.T) {
	data := t.TempDir()
	cmd := &Command{
		Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal: func() (remote.Terminal, error) {
				return nil, errors.New("open /dev/tty: no such device")
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cmd.Run(ctx)
	if err == nil {
		t.Fatal("F6: terminal-open failure succeeded")
	}
	g := strings.ToLower(err.Error())
	for _, w := range []string{"invite", "--invite-file", "--invite-stdin"} {
		if !strings.Contains(g, w) {
			t.Errorf("F6: terminal-open guidance missing %q: %v", w, err)
		}
	}
	if !strings.Contains(g, "no such device") {
		t.Errorf("F6: terminal-open cause not surfaced: %v", err)
	}
}

func TestS5R4_F6_InviteFileAndStdinBypassTTY(t *testing.T) {
	t.Run("invite-file", func(t *testing.T) {
		data := t.TempDir()
		path := writeInvitePath(t, data)
		var opened int
		cmd := &Command{
			Args:   []string{"scimux", "--remote", "--invite-file", path, "-data", data, "-addr", "127.0.0.1:0"},
			Stdout: io.Discard,
			Stderr: new(bytes.Buffer),
			Home:   t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
				NewTerminal: func() (remote.Terminal, error) {
					opened++
					return nil, errors.New("tty should not open")
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cmd.Run(ctx)
		if opened != 0 {
			t.Fatal("F6: --invite-file opened hidden TTY")
		}
	})
	t.Run("invite-stdin", func(t *testing.T) {
		data := t.TempDir()
		var opened int
		cmd := &Command{
			Args:   []string{"scimux", "--remote", "--invite-stdin", "-data", data, "-addr", "127.0.0.1:0"},
			Stdin:  strings.NewReader("0410-6105-0R3G-G28A-1C60-T3GF\n"),
			Stdout: io.Discard,
			Stderr: new(bytes.Buffer),
			Home:   t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
				NewTerminal: func() (remote.Terminal, error) {
					opened++
					return nil, errors.New("tty should not open")
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cmd.Run(ctx)
		if opened != 0 {
			t.Fatal("F6: --invite-stdin opened hidden TTY")
		}
	})
}

func TestS5R4_F9_HostedRemoteStatusProjection(t *testing.T) {
	t.Run("enrolled", func(t *testing.T) {
		srv := newCoopRV(t)
		data := t.TempDir()
		writeEnrolledState(t, data)
		cmd := remoteCmd(t, data, srv.URL, srv.Client())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.Run(ctx); err != nil {
			t.Fatalf("F9: enrolled run: %v", err)
		}
		assertHostedStatus(t, cmd, "enrolled")
	})
	t.Run("unavailable", func(t *testing.T) {
		data := t.TempDir()
		writeEnrolledState(t, data)
		cmd := remoteCmd(t, data, "http://127.0.0.1:1", unreachableHTTP())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.Run(ctx); err != nil {
			t.Fatalf("F9: unavailable killed localhost: %v", err)
		}
		assertHostedStatus(t, cmd, "unavailable")
	})
	t.Run("revoked", func(t *testing.T) {
		data := t.TempDir()
		writeEnrolledState(t, data)
		cmd := remoteCmd(t, data, "http://127.0.0.1:1", constantStatusHTTP(http.StatusNotFound))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.Run(ctx); err != nil {
			t.Fatalf("F9: revoked killed localhost: %v", err)
		}
		assertHostedStatus(t, cmd, "revoked")
	})
	t.Run("disabled", func(t *testing.T) {
		data := t.TempDir()
		writeEnrolledState(t, data)
		c := remote.NewClient(remote.Config{DataDir: data, Remote: true})
		if err := c.DisableAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		cmd := remoteCmd(t, data, "http://127.0.0.1:1", unreachableHTTP())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.Run(ctx); err != nil {
			t.Fatalf("F9: disabled killed localhost: %v", err)
		}
		assertHostedStatus(t, cmd, "disabled")
	})
	t.Run("missing-enrollment-still-fails-locally", func(t *testing.T) {
		data := t.TempDir()
		cmd := remoteCmd(t, data, "http://127.0.0.1:1", unreachableHTTP())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := cmd.Run(ctx)
		if err == nil {
			t.Fatal("F9: missing enrollment succeeded")
		}
		var re *remote.Error
		if !errors.As(err, &re) || re.Class != remote.ClassNeedInvite {
			t.Fatalf("F9: want ClassNeedInvite, got %v", err)
		}
	})
}

func remoteCmd(t *testing.T, data, url string, hc *http.Client) *Command {
	t.Helper()
	return &Command{
		Args:   []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout: io.Discard,
		Stderr: new(bytes.Buffer),
		Home:   t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: url,
			HTTPClient:    hc,
			NewTerminal:   noTestTerminal,
		},
	}
}

func assertHostedStatus(t *testing.T, cmd *Command, want string) {
	t.Helper()
	h := cmd.Handler()
	if h == nil {
		t.Fatal("F9: handler missing")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("F9: GET /api/state = %d, want 200 (localhost operational)", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("F9: state JSON: %v", err)
	}
	remoteObj, ok := payload["remote"].(map[string]any)
	if !ok {
		t.Fatalf("F9: payload missing remote status object: %s", rec.Body.Bytes())
	}
	got, _ := remoteObj["status"].(string)
	if got != want {
		t.Fatalf("F9: remote.status = %q, want %q; body=%s", got, want, rec.Body.Bytes())
	}
}

func writeInvitePath(t *testing.T, data string) string {
	t.Helper()
	path := data + "/invite"
	if err := os.WriteFile(path, []byte("0410-6105-0R3G-G28A-1C60-T3GF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newCoopRV(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/challenge":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"challenge":"%s","v":1}`+"\n", strings.Repeat("22", 32))
		case "/v1/verify":
			w.WriteHeader(http.StatusNoContent)
		case "/v1/wait":
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Rv-Challenge", strings.Repeat("33", 32))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
