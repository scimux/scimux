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
	configureWeb func(*webSupervisor)
	report       func(error)
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
	muxer, err := newMuxerBackend(a)
	if err != nil {
		return nil, err
	}
	muxer.enableRemote(cmd.Config.Remote)
	coreHandler, err := muxer.handler()
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
	if err := web.Start(ctx, executable); err != nil {
		_ = web.Close()
		_ = core.Close()
		muxer.shutdownHarnesses()
		return nil, err
	}

	r := &splitRuntime{muxer: muxer, core: core, web: web, command: cmd}
	a.prepareWebUpdate = func(updateCtx context.Context, path string) (webUpdateHandoff, error) {
		prepared, err := web.Prepare(updateCtx, path)
		if err != nil {
			return webUpdateHandoff{}, err
		}
		return webUpdateHandoff{commit: prepared.Commit, abort: prepared.Abort}, nil
	}
	recoverCtx, stopRecover := context.WithCancel(ctx)
	r.stopRecover = stopRecover
	go web.Recover(recoverCtx, executable, opts.report)
	return r, nil
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
	})
	return r.closeErr
}
