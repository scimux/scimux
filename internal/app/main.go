// scimux — supervise tmux-wrapped agent chats (Claude Code, Codex) from a
// local web page. Each chat is a node in a research tree; prompts go in via
// tmux, replies come back from the transcript files the agent CLIs write.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scimux/scimux/internal/remote"
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
// LogCache is populated (segment, fare, assets, blocked imports) so the first /api/state or
// chat poll reads settled logs from memory instead of parsing on demand.
func (a *app) warmStartup() {
	// Panes that outlived scimux keep whatever their hook bundle proves, so
	// the permission capability is re-derived from disk before anything can
	// consult it.
	a.refreshClaudePermCaps()
	a.poll()

	a.mu.Lock()
	nodes := make([]*Node, len(a.nodes))
	copy(nodes, a.nodes)
	a.mu.Unlock()
	for _, n := range nodes {
		a.segment(n) // warms all five LogCache products in one walk
	}
}

func configureUsage(fs *flag.FlagSet, name string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, appSummary)
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Usage:")
		fmt.Fprintf(out, "  %s [options]\n", name)
		fmt.Fprintf(out, "  %s stop [options]\n\n", name)
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
	if len(os.Args) > 1 && os.Args[1] == claudeSessionHookCmd {
		os.Exit(runClaudeSessionHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudePermissionHookCmd {
		os.Exit(runClaudePermissionHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudeStopHookCmd {
		os.Exit(runClaudeStopHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudeNotifyHookCmd {
		os.Exit(runClaudeNotifyHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudeCompactHookCmd {
		os.Exit(runClaudeCompactHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudeElicitationHookCmd {
		os.Exit(runClaudeElicitationHookMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == claudeUsageStatusLineCmd {
		os.Exit(runClaudeUsageStatusLineMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == webChildCmd {
		os.Exit(runWebChildMain())
	}
	if len(os.Args) > 1 && os.Args[1] == sessionWorkerCmd {
		os.Exit(runSessionWorkerMain())
	}
	if len(os.Args) > 1 && os.Args[1] == stopCmd {
		os.Exit(runStopMain(os.Args[2:], os.Stdout, os.Stderr))
	}
	handoff, err := loadMuxerExecFiles(os.Getenv)
	_ = os.Unsetenv(envMuxerPublicFD)
	_ = os.Unsetenv(envMuxerLockFD)
	_ = os.Unsetenv(envMuxerCSRFToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}
	// Command.Run owns the flag table. This function used to parse the same
	// eight flags first, on flag.CommandLine, purely to seed the Config that
	// Run then re-derived from the identical argv — and it discarded -addr
	// and -trusted-host while doing so. Two tables over one argv can only
	// drift; the one that binds the listener and builds the request policy is
	// the one that survives.
	cmd := &Command{
		Args:               os.Args,
		Stdin:              os.Stdin,
		Stdout:             os.Stdout,
		Stderr:             os.Stderr,
		Home:               home,
		ExperimentalRemote: experimentalRemoteEnabled(os.Getenv),
		Config: remote.Config{
			Stdin:  os.Stdin,
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		},
		muxerOnly: true,
		handoff:   handoff,
	}
	if err := cmd.Run(context.Background()); err != nil {
		if errors.Is(err, errFlagsReported) {
			// Command.Run's FlagSet already wrote the message and the usage.
			// -h is a request, not a failure, so it exits 0; every other argv
			// error keeps flag.ExitOnError's status 2. Both are what this
			// binary did while the parse lived here.
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	a := cmd.application
	if a == nil {
		fmt.Fprintln(os.Stderr, "scimux: startup produced no application")
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	dataDir := filepath.Dir(a.storePath)
	workerExe, err := pinWorkerExecutable(exe, dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}
	workers := newWorkerManager(workerExe, dataDir, version)
	workers.home, workers.socket = a.home, cmd.socket
	if err := workers.Reconcile(a.nodes); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: reconnect session workers:", err)
	}
	if err := workers.RecoverUnknown(a); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: recover interrupted session-worker transactions:", err)
	}
	if err := workers.RecoverOwnedClaudePanes(a); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: recover owned Claude chats with session workers:", err)
	}
	if err := sweepWorkerExecutables(dataDir, workerExe); err != nil {
		fmt.Fprintln(os.Stderr, "scimux: clean obsolete session-worker binaries:", err)
	}
	a.workers = workers
	status := startStatus(os.Stderr, "scimux: preparing chats before opening the web UI", isTerminal(os.Stderr))
	a.warmStartup()
	status.Done()
	stopRequested := make(chan struct{}, 1)
	restartRequested := make(chan struct{}, 1)
	runtimeOpts := splitRuntimeOptions{
		requestStop: func() {
			select {
			case stopRequested <- struct{}{}:
			default:
			}
		},
		requestRestart: func() {
			select {
			case restartRequested <- struct{}{}:
			default:
			}
		},
		report: func(err error) {
			fmt.Fprintln(os.Stderr, "scimux: restart web child:", err)
		},
	}
	runtime, err := startSplitRuntime(context.Background(), a, cmd, exe, runtimeOpts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scimux:", err)
		os.Exit(1)
	}

	// The split runtime owns graceful worker shutdown. Signals only select that
	// path here; muxer replacement uses the distinct detach path so live chats
	// survive an update.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

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
	// The permission fast lane answers armed Claude tool calls between polls.
	// A tick with no armed lease costs one map walk and no filesystem work.
	go a.claudePermissionLane()
	// Warm the harness/model probe (it shells out to the agent CLIs) so the
	// first new-activity dialog doesn't wait on subprocesses.
	go detectAgents()
	// Same reason, separate probe: the burger menu reads `<bin> --version`
	// for every harness, and the menu should open on an answer.
	go harnessInventory()
	// Learn the concrete claude model ids (the CLI mis-resolves its own family
	// aliases). The probe spends no tokens, but it does spend throwaway
	// sessions, so it is cached against the CLI's version and refreshed only
	// when that changes. Background: launches fall back to the bare alias until
	// it returns.
	a.installClaudeModelProbe()
	a.ensureClaudeModels()
	a.museCatalog = probeMuseCatalog
	a.museClassify = classifyMuseStandard
	// Same shape as the claude probe above, and for the same reason: the muse
	// catalog costs a `muse serve` spawn, so it is learned in the background
	// and read from cache by GET /api/agents. Warming it here means the first
	// new-activity dialog usually opens on an answer instead of on nothing.
	a.ensureMuseCatalog()

	// Run already bound this, before it spent anything at the rendezvous.
	// The muxer retains it while web generations inherit duplicates, so the
	// port is never unbound during replacement.
	ln := cmd.Listener()
	if ln == nil {
		fmt.Fprintln(os.Stderr, "scimux: startup bound no listener")
		os.Exit(1)
	}
	fmt.Printf("scimux: http://%s/  (tmux socket %q, store %s)\n", ln.Addr(), cmd.socket, a.storePath)
	fmt.Printf("scimux: attach to a chat by hand: tmux -L %s attach -t <node-id>\n", cmd.socket)

	for {
		select {
		case <-stop:
			signal.Stop(stop)
			_ = runtime.Close()
			return
		case <-stopRequested:
			signal.Stop(stop)
			_ = runtime.Close()
			return
		case <-restartRequested:
			files, prepareErr := runtime.PrepareExec()
			if prepareErr != nil {
				fmt.Fprintln(os.Stderr, "scimux: prepare muxer update:", prepareErr)
				continue
			}
			if quiesceErr := runtime.QuiesceForExec(); quiesceErr != nil {
				_ = files.Close()
				fmt.Fprintln(os.Stderr, "scimux: quiesce muxer update:", quiesceErr)
				return
			}
			replaceErr := replaceMuxerProcess(exe, files)
			_ = files.Close()
			// Exec returns only on failure. Reattach the still-running workers
			// and restore service with the installed binary's web child.
			if reconcileErr := workers.Reconcile(a.nodes); reconcileErr != nil {
				fmt.Fprintln(os.Stderr, "scimux: reconnect workers after failed muxer update:", reconcileErr)
			}
			recovered, recoverErr := startSplitRuntime(context.Background(), a, cmd, exe, runtimeOpts)
			if recoverErr != nil {
				fmt.Fprintln(os.Stderr, "scimux: muxer update failed:", errors.Join(replaceErr, recoverErr))
				cmd.closeListener()
				cmd.closeOwnership()
				return
			}
			runtime = recovered
			fmt.Fprintln(os.Stderr, "scimux: muxer update exec failed; restored current muxer:", replaceErr)
		}
	}
}
