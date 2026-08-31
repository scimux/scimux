package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/chrberger/scimux/internal/remote"
)

// errFlagsReported marks an argv error the FlagSet has already reported to
// Stderr, message and usage both. Run() (main.go) exits on it silently: -h is
// a request rather than a failure and exits 0, anything else keeps the status
// 2 that flag.ExitOnError produced before Phase 3 removed the duplicate parse.
var errFlagsReported = errors.New("scimux: invalid command line")

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
	listener    net.Listener
	socket      string

	// tunnelHandlerFor is the S3 tunnel boundary factory this run handed the
	// remote client, or nil for a purely local run. It is kept so the join is
	// observable from the product entry point rather than only from the
	// constructor.
	tunnelHandlerFor func(remote.TunnelPeer) http.Handler
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
	// This is the only parse in the process, so it owns -h. Without
	// configureUsage the flag package prints its bare "Usage of scimux:"
	// list, dropping the product summary and the usage line the binary has
	// always printed.
	configureUsage(fs, args[0])
	addr := fs.String("addr", addrDefault, "listen address (loopback only; use an SSH tunnel for remote access)")
	data := fs.String("data", dataDefault, "data directory for the node store")
	socket := fs.String("socket", socketDefault, "tmux socket name (tmux -L) for the private server")
	doRemote := fs.Bool("remote", c.Config.Remote, "enable remote access")
	inviteFile := fs.String("invite-file", c.Config.InviteFile, "read invite from a 0600 owner-only file")
	inviteStdin := fs.Bool("invite-stdin", c.Config.InviteStdin, "read invite from stdin")
	rvURL := fs.String("rendezvous-url", c.Config.Origin, "rendezvous base URL (default "+remote.DefaultOrigin+"); also the origin bound into pairing transcripts")
	var trustedHosts stringList
	fs.Var(&trustedHosts, "trusted-host", "additional Host name or IP allowed at the request boundary (repeatable; not authentication)")
	if err := fs.Parse(args[1:]); err != nil {
		// fs has already written the message and the usage to Stderr. Wrap so
		// the process entry point can exit without printing a second copy —
		// that is what flag.ExitOnError did while Run() owned the parse.
		return fmt.Errorf("%w: %w", errFlagsReported, err)
	}

	origin, err := normalizeRendezvousURL(*rvURL)
	if err != nil {
		return err
	}
	c.Config.Origin = origin

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

	// One owned mux, two boundaries (S3). The local chain is byte-for-byte
	// what NewHandler has always returned (FR-15); the factory binds the same
	// mux to whichever device the wait loop later proves.
	local, tunnelFor, err := newBoundaryFactory(a, webFS)
	if err != nil {
		return err
	}
	if c.handler == nil {
		c.handler = local
	}

	// Bind before enrolling. An invite is single-use, so redeeming one is the
	// only step of startup that cannot be retried; the listener is the step
	// most likely to fail, because a port is the one resource another process
	// can already hold. Ordered the other way round — as it was on 2026-08-27
	// — a second scimux on the default port cost a real invite and served
	// nothing. The bound listener is handed to the caller rather than the
	// address, so the check and the server cannot be about different sockets.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	c.listener = ln

	if !c.Config.Remote {
		return nil
	}

	// Join 3: without this the whole tunnel half is unreachable — a device
	// could pair, negotiate a session and frame a request, and the computer
	// would answer ClassUnavailable because it serves no handler. The peer is
	// translated rather than shared: internal/remote cannot import this
	// package, and this package must not grow a pion-adjacent type.
	c.tunnelHandlerFor = func(p remote.TunnelPeer) http.Handler {
		return tunnelFor(tunnelPeer{DeviceID: p.DeviceID, RID: p.RID})
	}

	rc := c.Config
	rc.TunnelHandlerFor = c.tunnelHandlerFor
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
		c.application.mu.Lock()
		c.application.hostedPairing = cli
		c.application.mu.Unlock()
	}
	if err != nil {
		switch remoteClass(err) {
		case remote.ClassRevoked, remote.ClassDisabled, remote.ClassUnavailable:
			// scimux still starts: losing remote access must never lock an
			// operator out of the chats on the computer in front of them.
			// What it must not do is start *quietly*. Until 2026-08-27 this
			// was a bare `return nil`, so a revoked computer came up looking
			// exactly like a healthy one and only /api/remote/status knew
			// otherwise — which is no use to the person reading the terminal
			// and wondering why their phone cannot reach it.
			c.reportDegradedRemote(err)
			return nil
		default:
			c.closeListener()
			return err
		}
	}
	return nil
}

// reportDegradedRemote prints the one line that says remote is off and why.
// It prints Guidance, which is the operator-facing half of remote.Error and
// is already forbidden from carrying invite plaintext, private keys or
// rendezvous ids; the wrapped cause is deliberately not printed, because at
// this point the operator's next move is a decision, not a diagnosis.
func (c *Command) reportDegradedRemote(err error) {
	if c == nil || c.Stderr == nil {
		return
	}
	var re *remote.Error
	why := ""
	if errors.As(err, &re) {
		why = re.Guidance
	}
	if why == "" {
		why = string(remoteClass(err))
	}
	fmt.Fprintf(c.Stderr, "scimux: remote access is off: %s\n", why)
	fmt.Fprintln(c.Stderr, "scimux: chats on this computer are unaffected; the web UI is local as usual")
}

func (c *Command) closeListener() {
	if c == nil || c.listener == nil {
		return
	}
	_ = c.listener.Close()
	c.listener = nil
}

// Listener is the bound localhost listener after a successful Run. The caller
// serves on it rather than on the address: re-binding would defeat the whole
// point of binding before the invite is spent.
func (c *Command) Listener() net.Listener {
	if c == nil {
		return nil
	}
	return c.listener
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

// normalizeRendezvousURL trims the trailing slash a browser paste carries and
// insists on an absolute http(s) URL. rvBase() concatenates "/v1/..." onto
// this, and the origin it also becomes is compared byte for byte against the
// device's copy inside the pairing transcript — so a value that merely "works"
// as an address is not enough.
func normalizeRendezvousURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("-rendezvous-url %q is not a URL: %w", raw, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("-rendezvous-url %q must be an absolute https:// URL", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}
