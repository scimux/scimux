package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

func TestVibeProbeBudgetConstants(t *testing.T) {
	if vibeProbeTotal != 12*time.Second || vibeProbePerCall != 4*time.Second {
		t.Fatalf("probe budget = %s total, %s per call", vibeProbeTotal, vibeProbePerCall)
	}
}

func TestVibeCatalogProbeReadsModelsWithoutPrompting(t *testing.T) {
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			menu := vibeSelect("mdl", "Model", "model", "surprise", [][2]string{
				{"alpha", "Alpha"},
				{"beta", "Beta"},
			})
			return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			level := "lvl-low"
			if p.ValueId != nil && string(p.ValueId.Value) == "beta" {
				level = "lvl-max"
			}
			think := vibeSelect("thinking", "Thinking", "thinking", level, [][2]string{{level, "Only"}})
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{think}}, nil
		},
	}
	var cp *countingProcess
	var cwd string
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		cwd = dir
		return countingRunner(agent, &cp)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cwd, "scimux-vibe-probe-") {
		t.Fatalf("probe cwd = %q, want a private temporary directory", cwd)
	}
	if got := cat.Models; len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("models = %v, want alpha then beta without the accidental current value", got)
	}
	if len(cat.Efforts["alpha"]) != 1 || cat.Efforts["alpha"][0] != "lvl-low" {
		t.Fatalf("alpha thinking = %v", cat.Efforts["alpha"])
	}
	if len(cat.Efforts["beta"]) != 1 || cat.Efforts["beta"][0] != "lvl-max" {
		t.Fatalf("beta thinking = %v", cat.Efforts["beta"])
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.prompts != 0 {
		t.Fatalf("probe sent %d prompts", agent.prompts)
	}
	if agent.closes != 1 {
		t.Fatalf("close calls = %d, want 1", agent.closes)
	}
	if agent.lastInit == nil || agent.lastInit.ClientCapabilities.Fs.ReadTextFile || agent.lastInit.ClientCapabilities.Fs.WriteTextFile || agent.lastInit.ClientCapabilities.Terminal {
		t.Fatalf("initialize capabilities = %+v", agent.lastInit)
	}
	if agent.lastNew == nil || agent.lastNew.Cwd != cwd || len(agent.lastNew.McpServers) != 0 {
		t.Fatalf("new session = %+v, want cwd %q and no servers", agent.lastNew, cwd)
	}
	if cp == nil || atomic.LoadInt32(&cp.kills) != 1 || atomic.LoadInt32(&cp.waits) != 1 {
		t.Fatalf("process reap kills/waits = %v", cp)
	}
}

func TestVibeCatalogProbeReapsOnTimeoutAndMalformedSession(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		agent := &fakeAgent{blockNew: true}
		var cp *countingProcess
		_, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
			return countingRunner(agent, &cp)("", "vibe", dir, "", "")
		}, time.Nanosecond, time.Millisecond)
		if err == nil {
			t.Fatal("an expired probe returned a catalog")
		}
		agent.mu.Lock()
		prompts := agent.prompts
		agent.mu.Unlock()
		if prompts != 0 {
			t.Fatalf("timed-out probe sent %d prompts", prompts)
		}
		if cp == nil || atomic.LoadInt32(&cp.kills) != 1 || atomic.LoadInt32(&cp.waits) != 1 {
			t.Fatal("timed-out probe left the process")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		agent := &fakeAgent{newSessionErr: errors.New("not a session")}
		var cp *countingProcess
		_, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
			return countingRunner(agent, &cp)("", "vibe", dir, "", "")
		}, time.Second, 200*time.Millisecond)
		if err == nil {
			t.Fatal("malformed session/new returned a catalog")
		}
		agent.mu.Lock()
		prompts, closes := agent.prompts, agent.closes
		agent.mu.Unlock()
		if prompts != 0 || closes != 0 {
			t.Fatalf("prompts=%d closes=%d, want neither on a failed session", prompts, closes)
		}
		if cp == nil || atomic.LoadInt32(&cp.kills) != 1 || atomic.LoadInt32(&cp.waits) != 1 {
			t.Fatal("malformed probe left the process")
		}
	})
}

func TestVibeCatalogProbeStopsWhenAModelSwitchFails(t *testing.T) {
	var sets atomic.Int32
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			menu := vibeSelect("mdl", "Model", "model", "alpha", [][2]string{
				{"alpha", "Alpha"}, {"beta", "Beta"}, {"gamma", "Gamma"},
			})
			return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		},
		configReply: func(p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			n := sets.Add(1)
			if n >= 2 {
				return sdk.SetSessionConfigOptionResponse{}, errors.New("model switch refused")
			}
			think := vibeSelect("thinking", "Thinking", "thinking", "lvl-low", [][2]string{{"lvl-low", "Low"}})
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{think}}, nil
		},
	}
	var cp *countingProcess
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return countingRunner(agent, &cp)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 3 {
		t.Fatalf("models = %v, want every advertised id", cat.Models)
	}
	if len(cat.Efforts["alpha"]) != 1 || cat.Efforts["beta"] != nil || cat.Efforts["gamma"] != nil {
		t.Fatalf("efforts = %+v, want only the snapshot taken before the failure", cat.Efforts)
	}
	if atomic.LoadInt32(&cp.kills) != 1 {
		t.Fatal("partial probe left the process")
	}
}

func TestVibeCatalogProbeStopsAtTheModelBound(t *testing.T) {
	prev := vibeProbeModelLimit
	vibeProbeModelLimit = 1
	t.Cleanup(func() { vibeProbeModelLimit = prev })
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			menu := vibeSelect("mdl", "Model", "model", "", [][2]string{{"alpha", "Alpha"}, {"beta", "Beta"}})
			return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		},
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			think := vibeSelect("thinking", "Thinking", "thinking", "lvl-low", [][2]string{{"lvl-low", "Low"}})
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{think}}, nil
		},
	}
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return fakeRunner(agent)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 2 || cat.Efforts["beta"] != nil || len(cat.Efforts["alpha"]) != 1 {
		t.Fatalf("catalog = %+v, want both models and only the first thinking snapshot", cat)
	}
}

func TestVibeCatalogProbeEmptyMenuIsALaunchableDefault(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{SessionId: "probe-sess"}
	}}
	var cp *countingProcess
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return countingRunner(agent, &cp)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 0 || len(cat.Efforts) != 0 {
		t.Fatalf("empty advertisement produced %+v", cat)
	}
	agent.mu.Lock()
	closes := agent.closes
	agent.mu.Unlock()
	if closes != 1 || atomic.LoadInt32(&cp.kills) != 1 {
		t.Fatal("empty catalog skipped close or reap")
	}
}

func TestVibeProbeClientDeclinesHostCapabilities(t *testing.T) {
	c := probeClient{}
	perm, err := c.RequestPermission(context.Background(), sdk.RequestPermissionRequest{})
	if err != nil || perm.Outcome.Cancelled == nil {
		t.Fatalf("permission = %+v, %v; discovery must not approve", perm, err)
	}
	if _, err := c.ReadTextFile(context.Background(), sdk.ReadTextFileRequest{}); err == nil {
		t.Fatal("probe client read a file")
	}
	if _, err := c.WriteTextFile(context.Background(), sdk.WriteTextFileRequest{}); err == nil {
		t.Fatal("probe client wrote a file")
	}
	if _, err := c.CreateTerminal(context.Background(), sdk.CreateTerminalRequest{}); err == nil {
		t.Fatal("probe client created a terminal")
	}
	if _, err := c.KillTerminal(context.Background(), sdk.KillTerminalRequest{}); err == nil {
		t.Fatal("probe client killed a terminal")
	}
	if _, err := c.TerminalOutput(context.Background(), sdk.TerminalOutputRequest{}); err == nil {
		t.Fatal("probe client read a terminal")
	}
	if _, err := c.ReleaseTerminal(context.Background(), sdk.ReleaseTerminalRequest{}); err == nil {
		t.Fatal("probe client released a terminal")
	}
	if _, err := c.WaitForTerminalExit(context.Background(), sdk.WaitForTerminalExitRequest{}); err == nil {
		t.Fatal("probe client waited on a terminal")
	}
	if err := c.SessionUpdate(context.Background(), sdk.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
}

func TestVibeCatalogProbeRejectsAnEmptySessionID(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		return sdk.NewSessionResponse{}
	}}
	var cp *countingProcess
	_, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return countingRunner(agent, &cp)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err == nil {
		t.Fatal("session/new with no id returned a catalog")
	}
	agent.mu.Lock()
	closes := agent.closes
	agent.mu.Unlock()
	if closes != 0 || atomic.LoadInt32(&cp.kills) != 1 || atomic.LoadInt32(&cp.waits) != 1 {
		t.Fatalf("closes=%d process=%v", closes, cp)
	}
}

func TestVibeCatalogProbeKeepsTheCatalogWhenCloseFails(t *testing.T) {
	agent := &fakeAgent{
		closeErr: errors.New("already gone"),
		newSession: func() sdk.NewSessionResponse {
			menu := vibeSelect("mdl", "Model", "model", "", [][2]string{{"alpha", "Alpha"}})
			return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		},
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			think := vibeSelect("thinking", "Thinking", "thinking", "lvl-low", [][2]string{{"lvl-low", "Low"}})
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{think}}, nil
		},
	}
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return fakeRunner(agent)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil || len(cat.Models) != 1 || len(cat.Efforts["alpha"]) != 1 {
		t.Fatalf("catalog after a failed close = %+v, %v", cat, err)
	}
}

func TestVibeCatalogProbeHonorsACancelledParentAndThePerCallBudget(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var cp *countingProcess
		_, err := probeVibeCatalog(ctx, func(dir string) (Process, error) {
			return countingRunner(&fakeAgent{}, &cp)("", "vibe", dir, "", "")
		}, time.Second, 200*time.Millisecond)
		if err == nil || atomic.LoadInt32(&cp.kills) != 1 {
			t.Fatalf("cancelled probe err=%v process=%v", err, cp)
		}
	})
	t.Run("per-call", func(t *testing.T) {
		agent := &fakeAgent{blockNew: true}
		var cp *countingProcess
		started := time.Now()
		_, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
			return countingRunner(agent, &cp)("", "vibe", dir, "", "")
		}, time.Second, 30*time.Millisecond)
		if err == nil {
			t.Fatal("a blocked session/new was treated as a catalog")
		}
		if time.Since(started) > 500*time.Millisecond {
			t.Fatalf("per-call budget took %s", time.Since(started))
		}
		if atomic.LoadInt32(&cp.kills) != 1 || atomic.LoadInt32(&cp.waits) != 1 {
			t.Fatal("per-call timeout left the process")
		}
	})
	t.Run("clock", func(t *testing.T) {
		prev := vibeProbeClock
		var checks atomic.Int32
		base := time.Now()
		vibeProbeClock = func() time.Time {
			if checks.Add(1) >= 4 {
				return base.Add(time.Hour)
			}
			return base
		}
		t.Cleanup(func() { vibeProbeClock = prev })
		agent := &fakeAgent{
			newSession: func() sdk.NewSessionResponse {
				menu := vibeSelect("mdl", "Model", "model", "", [][2]string{{"alpha", "Alpha"}, {"beta", "Beta"}})
				return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
			},
			configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
				think := vibeSelect("thinking", "Thinking", "thinking", "lvl-low", [][2]string{{"lvl-low", "Low"}})
				return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{think}}, nil
			},
		}
		cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
			return fakeRunner(agent)("", "vibe", dir, "", "")
		}, time.Second, 200*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if cat.Efforts["beta"] != nil || len(cat.Efforts["alpha"]) != 1 {
			t.Fatalf("clock stop catalog = %+v", cat)
		}
	})
}

func TestProbeHasRoomWithoutADeadline(t *testing.T) {
	if !probeHasRoom(context.Background(), time.Second) {
		t.Fatal("a context with no deadline was treated as exhausted")
	}
}

func TestVibeCatalogProbeStartFailureRemovesItsDirectory(t *testing.T) {
	var dir string
	_, err := probeVibeCatalog(context.Background(), func(d string) (Process, error) {
		dir = d
		return nil, errors.New("binary missing")
	}, time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("a failed start returned a catalog")
	}
	if dir == "" {
		t.Fatal("probe did not choose a directory")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("probe directory survived a failed start: %v", statErr)
	}
}

func TestProbeVibeCatalogRejectsAnEmptyBinary(t *testing.T) {
	if _, err := ProbeVibeCatalog(context.Background(), ""); err == nil {
		t.Fatal("empty binary was probed")
	}
}

func TestVibeCatalogProbeDropsBlankAndDuplicateIds(t *testing.T) {
	agent := &fakeAgent{
		newSession: func() sdk.NewSessionResponse {
			menu := vibeSelect("mdl", "Model", "model", "", [][2]string{
				{"", "blank"},
				{"alpha", "Alpha"},
				{"alpha", "Alpha again"},
			})
			return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
		},
		configReply: func(sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
			return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
				{Boolean: &sdk.SessionConfigOptionBoolean{Id: "toggle", Name: "Toggle"}},
			}}, nil
		},
	}
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return fakeRunner(agent)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 1 || cat.Models[0] != "alpha" || cat.Efforts != nil {
		t.Fatalf("catalog = %+v, want only alpha and no invented thinking levels", cat)
	}
}

func TestVibeCatalogProbeBlankModelMenuIsTheDefault(t *testing.T) {
	agent := &fakeAgent{newSession: func() sdk.NewSessionResponse {
		menu := vibeSelect("mdl", "Model", "model", "", [][2]string{{"", "blank"}, {"", "also blank"}})
		return sdk.NewSessionResponse{SessionId: "probe-sess", ConfigOptions: []sdk.SessionConfigOption{menu}}
	}}
	cat, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return fakeRunner(agent)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Models) != 0 || cat.Efforts != nil {
		t.Fatalf("blank menu = %+v, want a launchable default", cat)
	}
	agent.mu.Lock()
	sets := len(agent.configSets)
	agent.mu.Unlock()
	if sets != 0 {
		t.Fatalf("blank menu selected %d models", sets)
	}
}

func TestVibeCatalogProbeStopsAfterInitializeWhenTheBudgetIsGone(t *testing.T) {
	prev := vibeProbeClock
	var n atomic.Int32
	vibeProbeClock = func() time.Time {
		if n.Add(1) >= 2 {
			return time.Now().Add(time.Hour)
		}
		return time.Now()
	}
	t.Cleanup(func() { vibeProbeClock = prev })
	agent := &fakeAgent{}
	var cp *countingProcess
	_, err := probeVibeCatalog(context.Background(), func(dir string) (Process, error) {
		return countingRunner(agent, &cp)("", "vibe", dir, "", "")
	}, time.Second, 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "probe budget exhausted") {
		t.Fatalf("err = %v, want the budget exhausted after initialize", err)
	}
	agent.mu.Lock()
	prompts := agent.prompts
	opened := agent.lastNew != nil
	agent.mu.Unlock()
	if prompts != 0 || opened || atomic.LoadInt32(&cp.kills) != 1 {
		t.Fatalf("prompts=%d opened=%v kills=%d", prompts, opened, atomic.LoadInt32(&cp.kills))
	}
}

func TestVibeCatalogProbeDirectoryCannotBeCreated(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", file)
	_, err := probeVibeCatalog(context.Background(), func(string) (Process, error) {
		t.Fatal("probe started a process without a directory")
		return nil, errors.New("unreachable")
	}, time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("probe continued without a directory")
	}
}

func TestProbeVibeCatalogClosureReapsEveryPath(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		missing := filepath.Join(tmp, "missing-vibe-acp")
		_, err := ProbeVibeCatalog(context.Background(), missing)
		if err == nil {
			t.Fatal("missing binary returned a catalog")
		}
		if strings.Contains(err.Error(), "start ") {
			t.Fatalf("probe wrapped a start error it should leave raw: %v", err)
		}
		left, globErr := filepath.Glob(filepath.Join(tmp, "scimux-vibe-probe-*"))
		if globErr != nil || len(left) != 0 {
			t.Fatalf("probe directory left behind: %v %v", left, globErr)
		}
	})
	t.Run("non-protocol process", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		script := filepath.Join(tmp, "stay-up")
		// A sleep, not an agent. It speaks no ACP, so initialize fails and
		// the probe still has to kill the process group and remove its directory.
		if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err := ProbeVibeCatalog(context.Background(), script)
		if err == nil {
			t.Fatal("a process that speaks no ACP returned a catalog")
		}
		if time.Since(started) > 7*time.Second {
			t.Fatalf("non-protocol probe took %s", time.Since(started))
		}
		left, globErr := filepath.Glob(filepath.Join(tmp, "scimux-vibe-probe-*"))
		if globErr != nil || len(left) != 0 {
			t.Fatalf("probe directory left behind: %v %v", left, globErr)
		}
	})
}
