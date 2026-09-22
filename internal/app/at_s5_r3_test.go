package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote"
)

func TestS5R3_F1_ProductionDefaultChoosesHiddenTTY(t *testing.T) {
	data := t.TempDir()
	var opened int
	tty := recordingTTYForInvite(t)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal: func() (remote.Terminal, error) {
				opened++
				return tty, nil
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = cmd.Run(ctx)
	if opened == 0 {
		t.Fatal("F1: production --remote with no invite-file/stdin did not open hidden TTY")
	}
}

func TestS5R3_F1_InviteFileAndStdinBypassTTY(t *testing.T) {
	t.Run("invite-file", func(t *testing.T) {
		data := t.TempDir()
		path := filepath.Join(data, "invite")
		if err := os.WriteFile(path, []byte("0410-6105-0R3G-G28A-1C60-T3GF\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var opened int
		cmd := &Command{
			ExperimentalRemote: true,
			Args:               []string{"scimux", "--remote", "--invite-file", path, "-data", data, "-addr", "127.0.0.1:0"},
			Stdout:             io.Discard,
			Stderr:             new(bytes.Buffer),
			Home:               t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
				NewTerminal: func() (remote.Terminal, error) {
					opened++
					return recordingTTYForInvite(t), nil
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cmd.Run(ctx)
		if opened != 0 {
			t.Fatal("F1: --invite-file opened hidden TTY")
		}
	})
	t.Run("invite-stdin", func(t *testing.T) {
		data := t.TempDir()
		var opened int
		cmd := &Command{
			ExperimentalRemote: true,
			Args:               []string{"scimux", "--remote", "--invite-stdin", "-data", data, "-addr", "127.0.0.1:0"},
			Stdin:              strings.NewReader("0410-6105-0R3G-G28A-1C60-T3GF\n"),
			Stdout:             io.Discard,
			Stderr:             new(bytes.Buffer),
			Home:               t.TempDir(),
			Config: remote.Config{
				DataDir:       data,
				RendezvousURL: "http://127.0.0.1:1",
				HTTPClient:    unreachableHTTP(),
				NewTerminal: func() (remote.Terminal, error) {
					opened++
					return recordingTTYForInvite(t), nil
				},
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cmd.Run(ctx)
		if opened != 0 {
			t.Fatal("F1: --invite-stdin opened hidden TTY")
		}
	})
}

func TestS5R3_F8_RevokedRemoteLeavesLocalhost(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    constantStatusHTTP(http.StatusNotFound),
			NewTerminal:   noTestTerminal,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("F8: revoked remote killed localhost startup: %v", err)
	}
	h := cmd.Handler()
	if h == nil {
		t.Fatal("F8: handler missing after revoked remote")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("F8: GET /api/state = %d after revoked remote", rec.Code)
	}
	cli := cmd.Client()
	if cli == nil {
		t.Fatal("F8: remote client missing")
	}
	st, err := cli.State()
	if err != nil {
		t.Fatal(err)
	}
	if st != remote.StateRevoked {
		t.Fatalf("F8: remote state = %q, want revoked", st)
	}
}

func TestS5R3_F8_DisabledRemoteLeavesLocalhost(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	c := remote.NewClient(remote.Config{DataDir: data, Remote: true})
	if err := c.DisableAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config:             remote.Config{DataDir: data, Remote: true, RendezvousURL: "http://127.0.0.1:1", HTTPClient: unreachableHTTP(), NewTerminal: noTestTerminal},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("F8: disabled remote killed localhost startup: %v", err)
	}
	h := cmd.Handler()
	if h == nil {
		t.Fatal("F8: handler missing after disabled remote")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("F8: GET /api/state = %d after disabled remote", rec.Code)
	}
	st, err := cmd.Client().State()
	if err != nil {
		t.Fatal(err)
	}
	if st != remote.StateDisabled {
		t.Fatalf("F8: remote state = %q, want disabled", st)
	}
}

func TestS5R3_F8_UnavailableRendezvousLeavesLocalhost(t *testing.T) {
	data := t.TempDir()
	writeEnrolledState(t, data)
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal:   noTestTerminal,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.Run(ctx); err != nil {
		t.Fatalf("F8: unavailable rendezvous killed localhost: %v", err)
	}
	h := cmd.Handler()
	if h == nil {
		t.Fatal("F8: handler missing")
	}
	rec := routeRequest(h, http.MethodGet, "/api/state", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("F8: GET /api/state = %d", rec.Code)
	}
	st, err := cmd.Client().State()
	if err != nil {
		t.Fatal(err)
	}
	if st == remote.StateRevoked {
		t.Fatal("F8: transport failure persisted as revoked")
	}
}

func recordingTTYForInvite(t *testing.T) remote.Terminal {
	t.Helper()
	return &stubTTY{line: "0410-6105-0R3G-G28A-1C60-T3GF"}
}

type stubTTY struct {
	line string
	echo bool
}

func (t *stubTTY) DisableEcho() error { t.echo = false; return nil }
func (t *stubTTY) RestoreEcho() error { t.echo = true; return nil }
func (t *stubTTY) EchoEnabled() bool  { return t.echo }
func (t *stubTTY) ReadLine() (string, error) {
	return t.line, nil
}
func (t *stubTTY) WritePrompt([]byte) error { return nil }

func writeEnrolledState(t *testing.T, data string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := remote.NewClient(remote.Config{DataDir: data})
	if err := os.MkdirAll(c.PrivateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	st := remote.PersistedState{
		V:          1,
		Status:     remote.StateEnrolled,
		Handle:     "ih_041061050R3GG28A",
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     remote.DefaultOrigin,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func constantStatusHTTP(code int) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: code,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found\n")),
			}, nil
		}),
		Timeout: 50 * time.Millisecond,
	}
}

func TestS5R3_F8_NeedInviteStillFailsLocally(t *testing.T) {
	data := t.TempDir()
	cmd := &Command{
		ExperimentalRemote: true,
		Args:               []string{"scimux", "--remote", "-data", data, "-addr", "127.0.0.1:0"},
		Stdout:             io.Discard,
		Stderr:             new(bytes.Buffer),
		Home:               t.TempDir(),
		Config: remote.Config{
			DataDir:       data,
			RendezvousURL: "http://127.0.0.1:1",
			HTTPClient:    unreachableHTTP(),
			NewTerminal: func() (remote.Terminal, error) {
				return nil, errors.New("no controlling terminal")
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cmd.Run(ctx)
	if err == nil {
		t.Fatal("F8: --remote with no enrollment succeeded")
	}
	var re *remote.Error
	if !errors.As(err, &re) || re.Class != remote.ClassNeedInvite {
		t.Fatalf("F8: want ClassNeedInvite, got %v", err)
	}
}
