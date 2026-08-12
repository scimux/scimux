// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is set by the command package, whose symbol is stamped at build time
// with go build -ldflags "-X main.version=v0.5.0".
var version = "dev"

// SetVersion supplies the build version owned by cmd/scimux.
func SetVersion(v string) {
	version = v
}

const appSummary = "scimux supervises agent chats from a local web page."

// stringList is a repeatable flag value (used by -trusted-host).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// hostname is resolved once at startup; shown in the UI statusbar.
var hostname = "scimux"

// ---------- background poller ----------

// warmStartup pays the cold replay cost before the browser can ask for it:
// one poll discovers/catches up tmux transcript mirrors, then the per-node
// LogCache is populated (segment, fare, assets) so the first /api/state or
// chat poll reads settled logs from memory instead of parsing on demand.
func (a *app) warmStartup() {
	a.poll()

	a.mu.Lock()
	nodes := make([]*Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.Unlock()
	for _, n := range nodes {
		a.segment(n) // warms all four LogCache products in one walk
	}
}

func configureUsage(fs *flag.FlagSet, name string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, appSummary)
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Usage: %s [options]\n\n", name)
		fs.PrintDefaults()
	}
}

type startupStatus struct {
	w       io.Writer
	label   string
	start   time.Time
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func startStatus(w io.Writer, label string, animate bool) *startupStatus {
	s := &startupStatus{w: w, label: label, start: time.Now()}
	if !animate {
		fmt.Fprintf(w, "%s ...\n", label)
		return s
	}
	s.stop = make(chan struct{})
	s.stopped = make(chan struct{})
	go func() {
		defer close(s.stopped)
		frames := []byte{'|', '/', '-', '\\'}
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			fmt.Fprintf(w, "\r%s %c", label, frames[i%len(frames)])
			i++
			select {
			case <-s.stop:
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *startupStatus) Done() {
	s.once.Do(func() {
		elapsed := time.Since(s.start).Round(time.Millisecond)
		if s.stop != nil {
			close(s.stop)
			<-s.stopped
			fmt.Fprintf(s.w, "\r\033[K%s done (%s)\n", s.label, elapsed)
			return
		}
		fmt.Fprintf(s.w, "%s done (%s)\n", s.label, elapsed)
	})
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// ---------- command runtime ----------

// Run starts the local scimux server and blocks until it exits.
func Run() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}
	configureUsage(flag.CommandLine, os.Args[0])
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (loopback only; use an SSH tunnel for remote access)")
	data := flag.String("data", filepath.Join(home, ".scimux"), "data directory for the node store")
	socket := flag.String("socket", "scimux", "tmux socket name (tmux -L) for the private server")
	var trustedHosts stringList
	flag.Var(&trustedHosts, "trusted-host", "additional Host name or IP allowed at the request boundary (repeatable; not authentication)")
	flag.Parse()

	if err := prepareDataDir(*data); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a, err := NewApp(Config{
		Home:    home,
		DataDir: *data,
		Socket:  *socket,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	policy, err := newRequestPolicy(*addr, trustedHosts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux: host policy:", err)
		os.Exit(1)
	}
	a.requestPolicy = policy
	status := startStatus(os.Stderr, "scimux: preparing chats before opening the web UI", isTerminal(os.Stderr))
	a.warmStartup()
	status.Done()

	// Structured-protocol subprocesses (ACP, codex app-server) are ours: unlike
	// tmux sessions (which deliberately survive scimux exit), they must not
	// orphan. Kill every such process group on shutdown. tmux sessions are
	// untouched.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		a.acp.Shutdown()
		a.codex.Shutdown()
		os.Exit(0)
	}()

	go func() {
		for {
			a.poll()
			// Subscription usage refreshes only while the user is active; the
			// cache no-ops outside the prompt-driven window, so this cannot
			// probe providers overnight (usage.go).
			a.maybeRefreshUsageAsync()
			time.Sleep(2 * time.Second)
		}
	}()
	// Warm the harness/model probe (it shells out to the agent CLIs) so the
	// first new-activity dialog doesn't wait on subprocesses.
	go detectAgents()
	// Learn the concrete claude model ids (the CLI mis-resolves its own family
	// aliases) — cached for claudeCacheTTL, so this billed call runs at most
	// weekly. Background: launches fall back to the bare alias until it returns.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		a.refreshClaudeModels(ctx)
	}()

	handler, err := NewHandler(a, webFS)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}

	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", *addr, *socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", *socket)
	// -addr may be bound wider than loopback, so give the server real
	// timeouts (slowloris defense). No ReadTimeout/WriteTimeout: legitimate
	// handlers can be slow (structured sends, the self-update download);
	// ReadHeaderTimeout covers the attack that matters.
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
}
