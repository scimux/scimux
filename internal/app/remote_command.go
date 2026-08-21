package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// Command is the injectable startup seam for remote-aware scimux (S5).
// Tests call Run; they must not spawn the scimux binary or os.Exit.
type Command struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Home   string

	Config remote.Config

	handler     http.Handler
	application *app
	client      *remote.Client
	listenAddr  string
	socket      string
}

// Run starts the process according to Args.
func (c *Command) Run(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("scimux: nil command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	args := c.Args
	if len(args) == 0 {
		args = []string{"scimux"}
	}

	if len(args) > 1 {
		switch args[1] {
		case claudeSessionHookCmd, claudePermissionHookCmd, claudeStopHookCmd, claudeNotifyHookCmd:
			if c.Config.Hooks.OnHookDispatch != nil {
				c.Config.Hooks.OnHookDispatch(args[1])
			}
			return nil
		}
	}

	home := c.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = h
	}

	dataDefault := c.Config.DataDir
	if dataDefault == "" {
		dataDefault = filepath.Join(home, ".scimux")
	}
	addrDefault := "127.0.0.1:8787"
	socketDefault := "scimux"
	if c.socket != "" {
		socketDefault = c.socket
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	if c.Stderr != nil {
		fs.SetOutput(c.Stderr)
	} else {
		fs.SetOutput(io.Discard)
	}
	addr := fs.String("addr", addrDefault, "listen address (loopback only; use an SSH tunnel for remote access)")
	data := fs.String("data", dataDefault, "data directory for the node store")
	socket := fs.String("socket", socketDefault, "tmux socket name (tmux -L) for the private server")
	doRemote := fs.Bool("remote", c.Config.Remote, "enable remote access")
	inviteFile := fs.String("invite-file", c.Config.InviteFile, "read invite from a 0600 owner-only file")
	inviteStdin := fs.Bool("invite-stdin", c.Config.InviteStdin, "read invite from stdin")
	var trustedHosts stringList
	fs.Var(&trustedHosts, "trusted-host", "additional Host name or IP allowed at the request boundary (repeatable; not authentication)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	c.listenAddr = *addr
	c.socket = *socket
	c.Config.DataDir = *data
	c.Config.Remote = *doRemote
	if *inviteFile != "" {
		c.Config.InviteFile = *inviteFile
	}
	c.Config.InviteStdin = *inviteStdin
	if c.Config.Stdin == nil {
		c.Config.Stdin = c.Stdin
	}
	if c.Config.Stdout == nil {
		c.Config.Stdout = c.Stdout
	}
	if c.Config.Stderr == nil {
		c.Config.Stderr = c.Stderr
	}

	if err := prepareDataDir(*data); err != nil {
		return err
	}

	a, err := NewApp(Config{
		Home:    home,
		DataDir: *data,
		Socket:  *socket,
	})
	if err != nil {
		return err
	}
	c.application = a
	policy, err := newRequestPolicy(*addr, trustedHosts)
	if err != nil {
		return fmt.Errorf("host policy: %w", err)
	}
	a.requestPolicy = policy

	if c.handler == nil {
		h, err := NewHandler(a, webFS)
		if err != nil {
			return err
		}
		c.handler = h
	}

	if !c.Config.Remote {
		return nil
	}

	rc := c.Config
	if rc.DataDir == "" {
		rc.DataDir = *data
	}
	rc.Remote = true
	if rc.NewTerminal == nil {
		rc.NewTerminal = remote.OpenOwnerTerminal
	}
	if rc.Backoff.Initial == 0 {
		rc.Backoff = remote.BackoffConfig{
			Initial:    100 * time.Millisecond,
			Max:        1600 * time.Millisecond,
			Factor:     2,
			Jitter:     0.2,
			SuccessFor: 5 * time.Second,
		}
	}
	cli := remote.NewClient(rc)
	c.client = cli
	err = cli.Start(ctx)
	if c.application != nil {
		c.application.setHostedRemote(cli)
	}
	if err != nil {
		switch remoteClass(err) {
		case remote.ClassRevoked, remote.ClassDisabled, remote.ClassUnavailable:
			return nil
		default:
			return err
		}
	}
	return nil
}

func (a *app) setHostedRemote(src interface{ HostedStatus() string }) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.hostedRemote = src
	a.mu.Unlock()
}

func remoteClass(err error) remote.Class {
	var re *remote.Error
	if errors.As(err, &re) {
		return re.Class
	}
	return ""
}

// Handler is the localhost HTTP handler after a successful Run.
func (c *Command) Handler() http.Handler {
	if c == nil {
		return nil
	}
	return c.handler
}

// DisableAll is FR-32 through the startup seam.
func (c *Command) DisableAll(ctx context.Context) error {
	if c.client != nil {
		return c.client.DisableAll(ctx)
	}
	cli := remote.NewClient(c.Config)
	c.client = cli
	return cli.DisableAll(ctx)
}

// Reenable is the explicit reverse of DisableAll.
func (c *Command) Reenable(ctx context.Context) error {
	if c.client != nil {
		return c.client.Reenable(ctx)
	}
	cli := remote.NewClient(c.Config)
	c.client = cli
	return cli.Reenable(ctx)
}

// Client is the remote client after Run, if any.
func (c *Command) Client() *remote.Client {
	if c == nil {
		return nil
	}
	return c.client
}
