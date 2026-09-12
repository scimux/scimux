package app

import (
	"context"
	"errors"
	"sync"

	"codeberg.org/chrberger/scimux/internal/backend"
)

// splitRuntime is the process boundary in one place: the muxer, its private
// API, and the web-server supervisor have independent lifetimes, while the
// public listener remains owned by the muxer process throughout rotations.
type splitRuntime struct {
	muxer       *muxerBackend
	core        *backend.Server
	web         *webSupervisor
	stopRecover context.CancelFunc
	command     *Command
	closeOnce   sync.Once
	closeErr    error
}

type splitRuntimeOptions struct {
	// configureWeb is a process-test seam. Production must leave it nil so the
	// only child argv is the literal hidden role "web-child".
	configureWeb   func(*webSupervisor)
	requestStop    func()
	requestRestart func()
	report         func(error)
}

func startSplitRuntime(ctx context.Context, a *app, cmd *Command, executable string, opts splitRuntimeOptions) (*splitRuntime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || cmd == nil {
		return nil, errors.New("split runtime: nil application or command")
	}
	if executable == "" {
		return nil, errors.New("split runtime: empty executable")
	}
	if cmd.ownership == nil {
		return nil, errors.New("split runtime: command does not own its data directory")
	}
	muxer, err := newMuxerBackend(a)
	if err != nil {
		return nil, err
	}
	muxer.enableRemote(cmd.Config.Remote)
	coreHandler, err := muxer.handler(opts.requestStop)
	if err != nil {
		return nil, err
	}
	core, err := backend.Listen("", coreHandler)
	if err != nil {
		return nil, err
	}
	web, err := newWebSupervisor(cmd.Listener(), core.Link(), cmd)
	if err != nil {
		_ = core.Close()
		return nil, err
	}
	if opts.configureWeb != nil {
		opts.configureWeb(web)
	}
	r := &splitRuntime{muxer: muxer, core: core, web: web, command: cmd}
	// Install the handoff before HTTP can become reachable. An update request
	// from the first accepted connection must never fall back to the legacy
	// uncoordinated exec path.
	a.prepareWebUpdate = func(updateCtx context.Context, path string) (webUpdateHandoff, error) {
		prepared, err := web.Prepare(updateCtx, path)
		if err != nil {
			return webUpdateHandoff{}, err
		}
		return webUpdateHandoff{commit: func() error {
			if err := prepared.Commit(); err != nil {
				return err
			}
			if opts.requestRestart != nil {
				opts.requestRestart()
			}
			return nil
		}, abort: prepared.Abort}, nil
	}
	if err := web.Start(ctx, executable); err != nil {
		a.prepareWebUpdate = nil
		_ = web.Close()
		_ = core.Close()
		muxer.releaseHarnessesAfterStartupFailure()
		return nil, err
	}
	if err := cmd.ownership.Publish(core.Link()); err != nil {
		a.prepareWebUpdate = nil
		_ = web.Close()
		_ = core.Close()
		muxer.releaseHarnessesAfterStartupFailure()
		return nil, err
	}

	recoverCtx, stopRecover := context.WithCancel(ctx)
	r.stopRecover = stopRecover
	go web.Recover(recoverCtx, executable, opts.report)
	return r, nil
}

// PrepareExec duplicates the only two kernel resources the next muxer cannot
// reacquire while this one is alive. Both copies remain close-on-exec until
// replaceMuxerProcess enters the fork lock.
func (r *splitRuntime) PrepareExec() (*muxerExecFiles, error) {
	if r == nil || r.command == nil {
		return nil, errors.New("split runtime: no muxer to replace")
	}
	return prepareMuxerExecFiles(r.command)
}

// QuiesceForExec retires only replaceable coordination processes. The public
// listener and ownership lock stay open for exec, while session workers lose
// merely their client connections and continue running their agent sessions.
func (r *splitRuntime) QuiesceForExec() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.stopRecover != nil {
			r.stopRecover()
		}
		if r.muxer != nil && r.muxer.app != nil {
			r.muxer.app.prepareWebUpdate = nil
		}
		if err := r.web.Close(); err != nil {
			r.closeErr = err
		}
		if err := r.core.Close(); err != nil && r.closeErr == nil {
			r.closeErr = err
		}
		if r.muxer != nil && r.muxer.app != nil && r.muxer.app.workers != nil {
			r.muxer.app.workers.Detach()
		}
	})
	return r.closeErr
}

// Close stops presentation first, then the private API, and only then the
// structured harnesses. tmux sessions are intentionally not part of this
// ownership graph and continue to survive a full muxer shutdown.
func (r *splitRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.stopRecover != nil {
			r.stopRecover()
		}
		if err := r.web.Close(); err != nil {
			r.closeErr = err
		}
		if err := r.core.Close(); err != nil && r.closeErr == nil {
			r.closeErr = err
		}
		if r.command != nil {
			r.command.closeListener()
		}
		r.muxer.shutdownHarnesses()
		if r.command != nil && r.command.ownership != nil {
			if err := r.command.ownership.Close(); err != nil && r.closeErr == nil {
				r.closeErr = err
			}
			r.command.ownership = nil
		}
	})
	return r.closeErr
}
