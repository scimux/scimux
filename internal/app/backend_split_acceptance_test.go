package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp/muse"
	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
	"codeberg.org/chrberger/scimux/internal/sessionlog"
	"codeberg.org/chrberger/scimux/internal/sessionworker"
	"codeberg.org/chrberger/scimux/internal/tmuxsession"
)

// TestWebGenerationChangePreservesInFlightPermission is the first backend
// split acceptance test: the replaceable web side is rebuilt from scratch
// while the muxer-owned app and its structured transport stay alive. A
// pending permission is useful here because it is both an in-flight turn and
// an externally named piece of harness state; preserving only the node would
// be too weak a test.
func TestWebGenerationChangePreservesInFlightPermission(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	n := seedStructuredNode(t, a, "split-pending", "pi", "acp")
	stub := &stubProc{
		live:           "active",
		hasSession:     true,
		hasPending:     true,
		pending:        allowPending("incarnation-a:7"),
		clearOnDeliver: true,
	}
	a.testProc = stub

	coreMux, err := newCoreMux(a)
	if err != nil {
		t.Fatal(err)
	}
	core, err := backend.Listen(t.TempDir(), coreMux)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })

	first := newTestWebGeneration(t, a, core.Link(), nil)
	before := splitChat(t, first, n.ID)
	if before["turn_in_flight"] != true || before["perm_request_id"] != "incarnation-a:7" {
		t.Fatalf("generation 1 lost in-flight permission: %#v", before)
	}

	// Constructing a new public gateway models replacement of every
	// web-owned object. Only the muxer app and its private Unix-socket API are
	// shared between generations.
	second := newTestWebGeneration(t, a, core.Link(), nil)
	after := splitChat(t, second, n.ID)
	if after["turn_in_flight"] != true || after["perm_request_id"] != "incarnation-a:7" {
		t.Fatalf("generation 2 lost in-flight permission: %#v", after)
	}
	if stub.shutdownCalls.Load() != 0 {
		t.Fatalf("web replacement shut down structured transports %d times", stub.shutdownCalls.Load())
	}

	body := `{"key":"1","request_id":"incarnation-a:7"}`
	rec := routeRequest(second, http.MethodPost, "/api/nodes/"+n.ID+"/key", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer through generation 2 = %d (%s)", rec.Code, rec.Body.String())
	}
	if stub.deliverCalls != 1 || stub.lastPrepareReqID != "incarnation-a:7" {
		t.Fatalf("permission delivery = %d for %q, want once for original request",
			stub.deliverCalls, stub.lastPrepareReqID)
	}
}

func splitChat(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	rec := routeRequest(h, http.MethodGet, "/api/nodes/"+id+"/chat", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET chat = %d (%s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func newTestWebGeneration(t *testing.T, a *app, link backend.Link, pairing hostedPairingClient) http.Handler {
	t.Helper()
	client, err := backend.NewClient(link)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := newWebBackend(webBackendConfig{
		Web: webFS, Core: client.Proxy(), RequestPolicy: a.requestPolicy, Pairing: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.local
}

// TestWebUpdatePreservesRealACPProcessAndPermission crosses every production
// boundary involved in the split: public HTTP, the real web child process,
// the private Unix API, app routing, the real ACP manager, and a synthetic ACP
// subprocess speaking line-delimited JSON-RPC. No vendor CLI or model runs.
func TestWebUpdatePreservesRealSessionWorkerAndPermission(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	statusPath := filepath.Join(t.TempDir(), "agent-status")
	data := filepath.Dir(a.storePath)
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	agentLauncher := "#!/bin/sh\nexec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" -test.run=^TestSyntheticACPAgentHelperProcess$\n"
	if err := os.WriteFile(filepath.Join(binDir, "opencode"), []byte(agentLauncher), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCIMUX_SYNTHETIC_ACP", "1")
	t.Setenv("SCIMUX_SYNTHETIC_ACP_STATUS", statusPath)
	workers := newWorkerManager(exe, data, "worker-v1.0.0")
	workers.startOptions = sessionWorkerStartOptions{
		args:   []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
		env:    []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
		stderr: os.Stderr,
	}
	a.workers = workers

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	claimCommandOwnership(t, cmd)
	addr := ln.Addr().String()
	restartGeneration := make(chan string, 1)
	runtime, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1", "SCIMUX_WEB_CHILD_TEST_VERSION=v1.0.0"}
		},
		requestRestart: func() {
			resp, err := testHTTPClient.Get("http://" + addr + "/api/state")
			if err != nil {
				restartGeneration <- "request failed: " + err.Error()
				return
			}
			_ = resp.Body.Close()
			restartGeneration <- resp.Header.Get("X-Scimux-Web-Generation")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	csrf := fetchCSRF(t, addr)

	createBody, _ := json.Marshal(map[string]any{
		"title": "split acceptance", "prompt": "ask for approval",
		"agent": "opencode", "dir": a.home,
	})
	code, body := publicJSON(t, addr, http.MethodPost, "/api/nodes", createBody, csrf)
	if code != http.StatusOK {
		t.Fatalf("create node = %d (%s)", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		t.Fatalf("created node = %q, %v", body, err)
	}
	locator, err := sessionworker.Discover(data, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	workerPID := locator.PID

	before := waitForPublicPermission(t, addr, created.ID)
	requestID, _ := before["perm_request_id"].(string)
	if requestID == "" || before["turn_in_flight"] != true {
		t.Fatalf("pending permission before update = %#v", before)
	}

	// Drive rotation through POST /api/update, including verified download,
	// standby preparation, response-safe drain, and activation. The candidate
	// is a tiny launcher for this same test binary, not a fake supervisor call.
	launcher := []byte("#!/bin/sh\nSCIMUX_WEB_CHILD_TEST_VERSION=v9.9.9 exec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" \"$@\"\n")
	assetName := "scimux-" + goosArch()
	releases := fakeForgejo(t, "v9.9.9", map[string][]byte{
		assetName: launcher, "SHA256SUMS": []byte(shaSums(assetName, launcher)),
	})
	withUpdateSeams(t, releases.URL, "v1.0.0")
	withTestAssetPolicy(t, releases)
	installed := filepath.Join(t.TempDir(), "scimux")
	if err := os.WriteFile(installed, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe, oldExec := executablePath, execSelf
	executablePath = func() (string, error) { return installed, nil }
	execSelf = func(string) error { return errors.New("muxer exec fallback used") }
	t.Cleanup(func() { executablePath, execSelf = oldExe, oldExec })
	updateBody := []byte(`{"expected_tag":"v9.9.9"}`)
	code, body = publicJSON(t, addr, http.MethodPost, "/api/update", updateBody, csrf)
	if code != http.StatusOK {
		t.Fatalf("public update = %d (%s)", code, body)
	}
	assertWebGenerationEventually(t, addr, "2")
	select {
	case generation := <-restartGeneration:
		if generation != "2" {
			t.Fatalf("muxer replacement requested while web generation %q was serving, want 2", generation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("public update activated the web candidate without requesting muxer replacement")
	}
	code, body = publicJSON(t, addr, http.MethodPost, "/api/update", updateBody, csrf)
	if code != http.StatusConflict || !strings.Contains(string(body), "already up to date") {
		t.Fatalf("active generation accepted its own update again: %d (%s)", code, body)
	}
	locator, err = sessionworker.Discover(data, created.ID)
	if err != nil || locator.PID != workerPID {
		t.Fatalf("session worker changed across web update: PID %d -> %#v (%v)", workerPID, locator, err)
	}
	after := waitForPublicPermission(t, addr, created.ID)
	if after["perm_request_id"] != requestID || after["turn_in_flight"] != true {
		t.Fatalf("pending permission changed across update: before=%#v after=%#v", before, after)
	}

	answer, _ := json.Marshal(map[string]string{"key": "1", "request_id": requestID})
	code, body = publicJSON(t, addr, http.MethodPost, "/api/nodes/"+created.ID+"/key", answer, csrf)
	if code != http.StatusOK {
		t.Fatalf("answer after update = %d (%s)", code, body)
	}
	waitForPublicText(t, addr, created.ID, "continued-after-approval")
	code, _ = publicJSON(t, addr, http.MethodPost, "/api/nodes/"+created.ID+"/key", answer, csrf)
	if code != http.StatusConflict {
		t.Fatalf("duplicate answer status = %d, want 409", code)
	}
	status, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(status), "answer:allow\n"); got != 1 {
		t.Fatalf("synthetic agent received %d answers, want exactly one; status=%q", got, status)
	}

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionworker.Discover(data, created.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("full muxer stop left session worker registered: %v", err)
	}
}

// TestEveryStructuredHarnessRunsBehindRealSessionWorker is the compact
// transport matrix. It crosses the production worker factory and each real
// subprocess launcher, while tiny protocol peers stand in for vendor CLIs so
// the suite spends no model quota.
func TestEveryStructuredHarnessRunsBehindRealSessionWorker(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		agent, binary, transport, helper, sessionID string
	}{
		{agent: "pi", binary: "pi-acp", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess", sessionID: "split-session"},
		{agent: "opencode", binary: "opencode", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess", sessionID: "split-session"},
		{agent: "grok", binary: "grok", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess", sessionID: "split-session"},
		{agent: "codex", binary: "codex", transport: "codex", helper: "TestSyntheticCodexAgentHelperProcess", sessionID: "codex-thread"},
		{agent: "muse", binary: "muse", transport: "muse", helper: "TestSyntheticMuseAgentHelperProcess", sessionID: "muse-session-1"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			data, bin := t.TempDir(), t.TempDir()
			if err := os.Chmod(data, 0o700); err != nil {
				t.Fatal(err)
			}
			launcher := "#!/bin/sh\nexec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" -test.run=^" + tc.helper + "$\n"
			if err := os.WriteFile(filepath.Join(bin, tc.binary), []byte(launcher), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SCIMUX_SYNTHETIC_ACP", "1")
			t.Setenv("SCIMUX_SYNTHETIC_CODEX", "1")
			t.Setenv("SCIMUX_SYNTHETIC_MUSE", "1")
			t.Setenv("SCIMUX_SYNTHETIC_ACP_STATUS", filepath.Join(data, "agent-status"))

			workers := newWorkerManager(exe, data, "worker-contract")
			workers.startOptions = sessionWorkerStartOptions{
				args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
				env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
			}
			t.Cleanup(workers.Shutdown)
			node := &Node{
				ID: "chat-" + tc.agent, Title: tc.agent + " worker", Prompt: "ping", Agent: tc.agent,
				Dir: data, Model: "cheap", Effort: "low", Transport: tc.transport, CreatedAt: "2026-09-11T21:00:00Z",
			}
			if sid, err := workers.LaunchNode(node, node.Model); err != nil || sid != tc.sessionID {
				t.Fatalf("LaunchNode = %q, %v; want %q", sid, err, tc.sessionID)
			}
			if err := workers.Send(node.ID, "ping"); err != nil {
				t.Fatal(err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				state := workers.State(node.ID)
				peek := workers.Peek(node.ID)
				if tc.transport == "codex" && state.Live == "quiet" && strings.Contains(peek, "pong") {
					break
				}
				if (tc.transport == "acp" || tc.transport == "muse") && state.Permission != nil {
					prepared, err := workers.PreparePermission(node.ID, state.Permission.RequestID, "1")
					if err != nil {
						t.Fatal(err)
					}
					if err := workers.Deliver(node.ID, prepared.Token); err != nil {
						t.Fatal(err)
					}
					for time.Now().Before(deadline) {
						if workers.Live(node.ID) == "quiet" && strings.Contains(workers.Peek(node.ID), "continued-after-approval") {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					if workers.Live(node.ID) == "quiet" && strings.Contains(workers.Peek(node.ID), "continued-after-approval") {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s worker did not complete: state=%#v peek=%q", tc.agent, state, peek)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if state := workers.State(node.ID); !state.TurnDone {
				t.Fatalf("completed %s turn was not latched in worker state: %#v", tc.agent, state)
			} else if state.SessionID != tc.sessionID {
				t.Fatalf("%s worker session id = %q, want %q", tc.agent, state.SessionID, tc.sessionID)
			}
			locator, err := sessionworker.Discover(data, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			workers.Detach()
			replacement := syntheticWorkerManager(t, data)
			if err := replacement.Reconcile([]*Node{node}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(replacement.Shutdown)
			after, err := sessionworker.Discover(data, node.ID)
			if err != nil || after.PID != locator.PID {
				t.Fatalf("%s worker changed across muxer reconnect: %#v -> %#v (%v)", tc.agent, locator, after, err)
			}
			if state := replacement.State(node.ID); !state.TurnDone {
				t.Fatalf("reattached %s completion was lost: %#v", tc.agent, state)
			}
			workers = replacement
			if err := workers.Kill(node.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := sessionworker.Discover(data, node.ID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worker locator survived deletion: %v", err)
			}
		})
	}
}

type structuredHarnessAcceptance struct {
	agent, binary, transport, helper string
}

// TestStructuredHarnessesMatchMonolithThroughPublicAPI drives the same
// browser-visible create/chat/approval/peek/send/clear/delete lifecycle through
// both compositions. The monolith owns the production protocol manager in the
// HTTP process; the split owns that same adapter in a real per-chat worker and
// reaches it through the muxer's Unix API. Deterministic protocol peers keep
// this an offline acceptance test rather than a provider-quota test.
func TestStructuredHarnessesMatchMonolithThroughPublicAPI(t *testing.T) {
	for _, harness := range []structuredHarnessAcceptance{
		{agent: "pi", binary: "pi-acp", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess"},
		{agent: "opencode", binary: "opencode", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess"},
		{agent: "grok", binary: "grok", transport: "acp", helper: "TestSyntheticACPAgentHelperProcess"},
		{agent: "codex", binary: "codex", transport: "codex", helper: "TestSyntheticCodexAgentHelperProcess"},
		{agent: "muse", binary: "muse", transport: "muse", helper: "TestSyntheticMuseAgentHelperProcess"},
	} {
		t.Run(harness.agent, func(t *testing.T) {
			workDir := t.TempDir()
			var monolith, split []string
			t.Run("monolith", func(t *testing.T) {
				monolith = runStructuredHarnessPublicE2E(t, harness, workDir, false)
			})
			t.Run("split", func(t *testing.T) {
				split = runStructuredHarnessPublicE2E(t, harness, workDir, true)
			})
			if strings.Join(monolith, "\n") != strings.Join(split, "\n") {
				t.Fatalf("public lifecycle differs\nmonolith=%q\nsplit=%q", monolith, split)
			}
		})
	}
}

func runStructuredHarnessPublicE2E(t *testing.T, harness structuredHarnessAcceptance, workDir string, split bool) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	launcher := "#!/bin/sh\nexec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" -test.run=^" + harness.helper + "$\n"
	if err := os.WriteFile(filepath.Join(bin, harness.binary), []byte(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCIMUX_SYNTHETIC_ACP", "1")
	t.Setenv("SCIMUX_SYNTHETIC_CODEX", "1")
	t.Setenv("SCIMUX_SYNTHETIC_MUSE", "1")
	t.Setenv("SCIMUX_SYNTHETIC_ACP_STATUS", filepath.Join(t.TempDir(), "agent-status"))

	a := newShortAcceptanceApp(t)
	if harness.agent == "muse" {
		if err := os.WriteFile(a.settingsPath, []byte(`{"muse_approval_judge_consent":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		a.museCatalog = func(context.Context) ([]muse.Model, error) {
			return []muse.Model{{ID: "cheap", IsDefault: true}}, nil
		}
		a.museClassify = func(id string) string {
			if id == "cheap" {
				return "standard"
			}
			return ""
		}
	}
	var handler http.Handler
	if split {
		data := filepath.Dir(a.storePath)
		workers := newWorkerManager(exe, data, "worker-contract")
		workers.startOptions = sessionWorkerStartOptions{
			args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
			env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
		}
		a.workers = workers
		t.Cleanup(workers.Shutdown)
		coreHandler, err := newCoreMux(a)
		if err != nil {
			t.Fatal(err)
		}
		core, err := backend.Listen("", coreHandler)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = core.Close() })
		handler = newTestWebGeneration(t, a, core.Link(), nil)
	} else {
		handler, err = NewHandler(a, webFS)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			a.acp.Shutdown()
			a.codex.Shutdown()
			a.muse.Shutdown()
		})
	}

	create := fmt.Sprintf(`{"title":%q,"prompt":"ping","agent":%q,"model":"cheap","effort":"low","dir":%q}`,
		harness.agent+" acceptance", harness.agent, workDir)
	rec := routeRequest(handler, http.MethodPost, "/api/nodes", create, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create %s = %d (%s)", harness.agent, rec.Code, rec.Body.String())
	}
	var created Node
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created node = %q, %v", rec.Body.String(), err)
	}
	observed := []string{"create:200"}
	completeStructuredPublicTurn(t, a, handler, created.ID, harness.transport, 1)
	observed = append(observed, "initial:complete")

	if rec := routeRequest(handler, http.MethodGet, "/api/state", "", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("state after create = %d (%s)", rec.Code, rec.Body.String())
	}
	observed = append(observed, "state:visible")
	if rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+created.ID+"/peek", "", false); rec.Code != http.StatusOK {
		t.Fatalf("peek = %d (%s)", rec.Code, rec.Body.String())
	}
	observed = append(observed, "peek:200")

	rec = routeRequest(handler, http.MethodPost, "/api/nodes/"+created.ID+"/send", `{"text":"ping again"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("second send = %d (%s)", rec.Code, rec.Body.String())
	}
	completeStructuredPublicTurn(t, a, handler, created.ID, harness.transport, 2)
	observed = append(observed, "second:complete")

	rec = routeRequest(handler, http.MethodPost, "/api/nodes/"+created.ID+"/send", `{"text":"/clear"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d (%s)", rec.Code, rec.Body.String())
	}
	observed = append(observed, "clear:200")
	rec = routeRequest(handler, http.MethodPost, "/api/nodes/"+created.ID+"/send", `{"text":"after clear"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("send after clear = %d (%s)", rec.Code, rec.Body.String())
	}
	completeStructuredPublicTurn(t, a, handler, created.ID, harness.transport, 1)
	observed = append(observed, "post-clear:complete")

	var attention []string
	for _, event := range sessionlog.ReadEvents(a.sessionLogPath(created.ID)) {
		if event.T == "attention" && event.Attention != nil {
			attention = append(attention, event.Attention.Status)
		}
	}
	if harness.transport == "acp" || harness.transport == "muse" {
		want := []string{"start", "end", "start", "end", "start", "end"}
		if !reflect.DeepEqual(attention, want) {
			t.Fatalf("durable attention transitions = %v, want %v", attention, want)
		}
	}
	observed = append(observed, "attention:"+strings.Join(attention, ","))

	rec = routeRequest(handler, http.MethodDelete, "/api/nodes/"+created.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+created.ID+"/chat", "", false); rec.Code != http.StatusNotFound {
		t.Fatalf("chat after delete = %d (%s)", rec.Code, rec.Body.String())
	}
	return append(observed, "delete:gone")
}

func newShortAcceptanceApp(t *testing.T) *app {
	t.Helper()
	root, err := os.MkdirTemp("", "scmx-harness-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeTmux{}
	a, err := newApp(Config{Home: home, DataDir: data, LaunchGrace: 40 * time.Millisecond, LaunchPoll: 5 * time.Millisecond}, appDeps{
		Server: tmuxsession.NewServerWithRunner("testsock", fake.run),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.usage = nil
	return a
}

func completeStructuredPublicTurn(t *testing.T, a *app, handler http.Handler, nodeID, transport string, wantReplies int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	answered := ""
	for time.Now().Before(deadline) {
		a.poll()
		rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+nodeID+"/chat", "", false)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat = %d (%s)", rec.Code, rec.Body.String())
		}
		if transport == "codex" && strings.Count(rec.Body.String(), "pong") >= wantReplies && !strings.Contains(rec.Body.String(), `"turn_in_flight":true`) {
			return
		}
		if transport == "acp" || transport == "muse" {
			var chat map[string]any
			if json.Unmarshal(rec.Body.Bytes(), &chat) == nil {
				if requestID, _ := chat["perm_request_id"].(string); requestID != "" && requestID != answered {
					waitForAppAttention(t, a, nodeID, "approval")
					answer := fmt.Sprintf(`{"key":"1","request_id":%q}`, requestID)
					answerRec := routeRequest(handler, http.MethodPost, "/api/nodes/"+nodeID+"/key", answer, true)
					if answerRec.Code != http.StatusOK {
						t.Fatalf("answer = %d (%s)", answerRec.Code, answerRec.Body.String())
					}
					answered = requestID
				}
			}
			if strings.Count(rec.Body.String(), "continued-after-approval") >= wantReplies && !strings.Contains(rec.Body.String(), `"turn_in_flight":true`) {
				waitForAppAttention(t, a, nodeID, "")
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s turn did not complete", transport)
}

func waitForAppAttention(t *testing.T, a *app, nodeID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.poll()
		a.mu.Lock()
		got := a.attn[nodeID]
		a.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %s attention did not become %q", nodeID, want)
}

const syntheticClaudeLauncher = `#!/bin/sh
sid=
settings=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --session-id) sid=$2; shift 2 ;;
    --settings) settings=$2; shift 2 ;;
    *) shift ;;
  esac
done
bundle=${settings%/*}
project="$SCIMUX_FAKE_CLAUDE_HOME/.claude/projects/-fake"
mkdir -p "$project"
transcript="$project/$sid.jsonl"
: > "$transcript"
printf '{"hook_event_name":"SessionStart","session_id":"%s","transcript_path":"%s","cwd":"%s","source":"startup"}\n' "$sid" "$transcript" "$SCIMUX_FAKE_CLAUDE_HOME" > "$bundle/inbox/start.json"
turn=0
while IFS= read -r line; do
	turn=$((turn + 1))
	printf '{"type":"user","timestamp":"2026-09-11T00:00:00Z","uuid":"u%s","sessionId":"%s","session_id":"%s","message":{"role":"user","content":"%s"}}\n' "$turn" "$sid" "$sid" "$line" >> "$transcript"
	printf '{"type":"assistant","timestamp":"2026-09-11T00:00:01Z","uuid":"a%s","parentUuid":"u%s","sessionId":"%s","session_id":"%s","message":{"id":"m%s","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"worker-reply"}]}}\n' "$turn" "$turn" "$sid" "$sid" "$turn" >> "$transcript"
	turn_id=$(sed -n 's/.*"turn":"\([^"]*\)".*/\1/p' "$bundle/perm/turn.json")
	turn_gen=$(sed -n 's/.*"gen":\([0-9]*\).*/\1/p' "$bundle/perm/turn.json")
	if [ -n "$turn_id" ]; then
	  printf '{"turn":"%s","turn_gen":%s,"hook_event_name":"Stop","session_id":"%s"}\n' "$turn_id" "$turn_gen" "$sid" > "$bundle/stop/fixture-$turn_id.json"
	fi
done
`

// TestClaudeMatchesMonolithThroughPublicAPI applies the same public lifecycle
// comparison to the tmux/hook adapter. Each side gets a private real tmux
// server; the shell peer emits the official SessionStart evidence and a
// synthetic transcript, so no Claude binary or provider request is involved.
func TestClaudeMatchesMonolithThroughPublicAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	workDir := t.TempDir()
	var monolith, split []string
	t.Run("monolith", func(t *testing.T) {
		monolith = runClaudeHarnessPublicE2E(t, workDir, false)
	})
	t.Run("split", func(t *testing.T) {
		split = runClaudeHarnessPublicE2E(t, workDir, true)
	})
	if strings.Join(monolith, "\n") != strings.Join(split, "\n") {
		t.Fatalf("public Claude lifecycle differs\nmonolith=%q\nsplit=%q", monolith, split)
	}
}

func runClaudeHarnessPublicE2E(t *testing.T, workDir string, split bool) []string {
	t.Helper()
	root, err := os.MkdirTemp("", "scmx-claude-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home, data, bin := filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "bin")
	for _, dir := range []string{home, data, bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(syntheticClaudeLauncher), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCIMUX_FAKE_CLAUDE_HOME", home)

	socket := fmt.Sprintf("scimux-claude-public-%d", time.Now().UnixNano())
	tmux := tmuxsession.NewServer(socket)
	tmux.PasteDelay, tmux.AckPoll = 0, 5*time.Millisecond
	t.Cleanup(func() {
		_ = tmux.KillServer()
		if err := os.Remove(tmux.SocketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove tmux socket: %v", err)
		}
	})
	a, err := newApp(Config{Home: home, DataDir: data, Socket: socket}, appDeps{Server: tmux})
	if err != nil {
		t.Fatal(err)
	}
	a.usage = nil
	a.claudeInitialPoll = 10 * time.Millisecond
	if err := os.MkdirAll(a.sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	var handler http.Handler
	if split {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		workers := newWorkerManager(exe, data, "worker-contract")
		workers.home, workers.socket = home, socket
		workers.startOptions = sessionWorkerStartOptions{
			args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
			env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
		}
		a.workers = workers
		t.Cleanup(workers.Shutdown)
		coreHandler, err := newCoreMux(a)
		if err != nil {
			t.Fatal(err)
		}
		core, err := backend.Listen("", coreHandler)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = core.Close() })
		handler = newTestWebGeneration(t, a, core.Link(), nil)
	} else {
		handler, err = NewHandler(a, webFS)
		if err != nil {
			t.Fatal(err)
		}
	}

	create := fmt.Sprintf(`{"title":"claude acceptance","prompt":"ping","agent":"claude","dir":%q}`, workDir)
	rec := routeRequest(handler, http.MethodPost, "/api/nodes", create, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("create Claude = %d (%s)", rec.Code, rec.Body.String())
	}
	var created Node
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("created Claude = %q, %v", rec.Body.String(), err)
	}
	waitForClaudePublicReplies(t, a, handler, created.ID, 1)
	observed := []string{"create:200", "initial:complete"}
	if rec := routeRequest(handler, http.MethodGet, "/api/state", "", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("Claude state = %d (%s)", rec.Code, rec.Body.String())
	}
	observed = append(observed, "state:visible")
	if rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+created.ID+"/peek", "", false); rec.Code != http.StatusOK {
		t.Fatalf("Claude peek = %d (%s)", rec.Code, rec.Body.String())
	}
	observed = append(observed, "peek:200")
	rec = routeRequest(handler, http.MethodPost, "/api/nodes/"+created.ID+"/send", `{"text":"ping again"}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("second Claude send = %d (%s)", rec.Code, rec.Body.String())
	}
	waitForClaudePublicReplies(t, a, handler, created.ID, 2)
	observed = append(observed, "second:complete")
	rec = routeRequest(handler, http.MethodDelete, "/api/nodes/"+created.ID, "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete Claude = %d (%s)", rec.Code, rec.Body.String())
	}
	if tmux.Session(created.ID).Alive() {
		t.Fatal("deleted Claude chat left its private tmux pane alive")
	}
	return append(observed, "delete:gone")
}

func waitForClaudePublicReplies(t *testing.T, a *app, handler http.Handler, nodeID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		a.poll()
		rec := routeRequest(handler, http.MethodGet, "/api/nodes/"+nodeID+"/chat", "", false)
		last = rec.Body.String()
		workerSettled := true
		if a.workers != nil {
			state := a.workers.State(nodeID)
			workerSettled = !state.TurnInFlight && state.Delivery == ""
		}
		if rec.Code == http.StatusOK && strings.Count(rec.Body.String(), "worker-reply") >= want &&
			!strings.Contains(rec.Body.String(), `"turn_in_flight":true`) && workerSettled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.mu.Lock()
	node := a.byID[nodeID]
	live, send, launchErr := a.live[nodeID], a.sendState[nodeID], a.claudeLaunchErr[nodeID]
	a.mu.Unlock()
	pane, paneErr := a.server.Session(nodeID).Capture()
	transcript := []byte(nil)
	sessionLog := []byte(nil)
	sessionLogPath, sessionLogErr := "", error(nil)
	if node != nil {
		transcript, _ = os.ReadFile(node.Transcript)
		sessionLogPath = a.sessionLogPath(node.ID)
		sessionLog, sessionLogErr = os.ReadFile(sessionLogPath)
	}
	var workerState sessionworker.State
	if a.workers != nil {
		workerState = a.workers.State(nodeID)
	}
	t.Fatalf("Claude chat did not expose %d completed synthetic replies: node=%#v live=%q send=%q launch=%q worker=%#v pane=%q paneErr=%v transcript=%q sessionsDir=%q sessionlog=%q path=%q err=%v chat=%s",
		want, node, live, send, launchErr, workerState, pane, paneErr, transcript, a.sessionsDir, sessionLog, sessionLogPath, sessionLogErr, last)
}

// TestClaudeSessionWorkerSurvivesMuxerDisconnect uses a real worker process,
// a private real tmux server, and a shell fixture standing in for Claude. It
// proves Claude now crosses the same reattachment boundary as ACP/Codex while
// spending no provider quota.
func TestClaudeSessionWorkerSurvivesMuxerDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	data := t.TempDir()
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(syntheticClaudeLauncher), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCIMUX_FAKE_CLAUDE_HOME", data)
	socket := fmt.Sprintf("scimux-claude-worker-%d", time.Now().UnixNano())
	tmux := tmuxsession.NewServer(socket)
	t.Cleanup(func() {
		_ = tmux.KillServer()
		if err := os.Remove(tmux.SocketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove tmux socket: %v", err)
		}
	})

	workers := newWorkerManager(exe, data, "worker-old")
	workers.home, workers.socket = data, socket
	workers.startOptions = sessionWorkerStartOptions{
		args: []string{"-test.run=^TestProductionSessionWorkerHelperProcess$"},
		env:  []string{"SCIMUX_PRODUCTION_SESSION_WORKER_TEST=1"},
	}
	node := &Node{
		ID: "claude-worker", Title: "Claude worker", Prompt: "hello", Agent: "claude",
		Dir: data, SessionID: hookSIDOwn, Transport: "tmux", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if sid, err := workers.LaunchNode(node, ""); err != nil || sid != hookSIDOwn {
		t.Fatalf("LaunchNode = %q, %v", sid, err)
	}
	locator, err := sessionworker.Discover(data, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid := locator.PID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		state := workers.State(node.ID)
		if state.Transcript != "" && state.Delivery == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if state := workers.State(node.ID); state.Transcript == "" || state.Delivery != "" {
		t.Fatalf("Claude worker did not bind and confirm prompt: %#v", state)
	}

	workers.Detach()
	replacement := newWorkerManager(exe, data, "muxer-new")
	if err := replacement.Reconcile([]*Node{node}); err != nil {
		t.Fatal(err)
	}
	if !replacement.manages(node.ID) {
		t.Fatal("replacement muxer did not reattach Claude worker")
	}
	after, err := sessionworker.Discover(data, node.ID)
	if err != nil || after.PID != pid {
		t.Fatalf("Claude worker PID changed across muxer loss: %d -> %#v (%v)", pid, after, err)
	}
	if got := replacement.PeekMode(node.ID, "visible"); got == "" {
		t.Fatal("reattached worker returned an empty pane snapshot")
	}
	if err := replacement.Kill(node.ID); err != nil {
		t.Fatal(err)
	}
}

func publicJSON(t *testing.T, addr, method, path string, body []byte, csrf string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-Scimux-CSRF", csrf)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func waitForPublicPermission(t *testing.T, addr, nodeID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body := publicJSON(t, addr, http.MethodGet, "/api/nodes/"+nodeID+"/chat", nil, "")
		var got map[string]any
		if code == http.StatusOK && json.Unmarshal(body, &got) == nil && got["perm_request_id"] != nil {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending permission did not appear through public HTTP")
	return nil
}

func waitForPublicText(t *testing.T, addr, nodeID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body := publicJSON(t, addr, http.MethodGet, "/api/nodes/"+nodeID+"/chat", nil, "")
		if code == http.StatusOK && strings.Contains(string(body), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("public chat did not contain %q", want)
}

func TestSyntheticACPAgentHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_SYNTHETIC_ACP") != "1" {
		return
	}
	if err := runSyntheticACPAgent(os.Stdin, os.Stdout, os.Getenv("SCIMUX_SYNTHETIC_ACP_STATUS")); err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticCodexAgentHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_SYNTHETIC_CODEX") != "1" {
		return
	}
	if err := runSyntheticCodexAgent(os.Stdin, os.Stdout); err != nil {
		t.Fatal(err)
	}
}

func TestSyntheticMuseAgentHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_SYNTHETIC_MUSE") != "1" {
		return
	}
	if err := runSyntheticMuseAgent(os.Stdin, os.Stdout); err != nil {
		t.Fatal(err)
	}
}

// runSyntheticMuseAgent is an offline MSP peer. The acceptance tests execute
// it as `muse serve`, crossing the real launcher, worker process, private
// worker protocol, muxer API, and public HTTP surface without spending model
// quota or reading provider credentials.
func runSyntheticMuseAgent(in io.Reader, out io.Writer) error {
	type message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	scan := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	sessionN, turnN := 0, 0
	sessionID, turnID, approvalID := "", "", ""
	respond := func(id json.RawMessage, result any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	}
	notify := func(method string, params any) error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	for scan.Scan() {
		var msg message
		if err := json.Unmarshal(scan.Bytes(), &msg); err != nil {
			return err
		}
		switch msg.Method {
		case "initialize":
			if err := respond(msg.ID, map[string]any{
				"protocolVersion": "1", "serverInfo": map[string]any{"name": "muse", "version": muse.ObservedMuseVersion},
				"schema": map[string]any{"version": muse.ObservedMSPSchema, "fingerprint": muse.PinnedFingerprint},
			}); err != nil {
				return err
			}
		case "initialized":
			// Client readiness notification; no response belongs to it.
		case "session/start":
			sessionN++
			sessionID = fmt.Sprintf("muse-session-%d", sessionN)
			if err := respond(msg.ID, map[string]any{"sessionId": sessionID, "viewCursor": fmt.Sprintf("cursor-%d", sessionN)}); err != nil {
				return err
			}
		case "turn/start":
			turnN++
			turnID = fmt.Sprintf("muse-turn-%d", turnN)
			approvalID = fmt.Sprintf("muse-approval-%d", turnN)
			if err := respond(msg.ID, map[string]any{"turnId": turnID, "disposition": "started", "startedNewTurn": true}); err != nil {
				return err
			}
			if err := notify("approval/requested", map[string]any{
				"approvalId": approvalID, "sessionId": sessionID, "turnId": turnID,
				"subject":              map[string]any{"kind": "shell", "command": "printf synthetic"},
				"currentRequirementId": map[string]any{"approvalId": approvalID, "sourceIndex": 0},
				"availableChoices": []map[string]any{
					{"choiceId": "allow_once", "decision": "approved", "scope": "once", "label": "Allow once"},
					{"choiceId": "deny", "decision": "denied", "scope": "once", "label": "Reject"},
				},
			}); err != nil {
				return err
			}
		case "approval/decide":
			if err := respond(msg.ID, map[string]any{}); err != nil {
				return err
			}
			if err := notify("approval/resolved", map[string]any{
				"approvalId": approvalID, "sessionId": sessionID, "decision": "approved", "resolvedBy": "user",
			}); err != nil {
				return err
			}
			if err := notify("item/completed", map[string]any{
				"sessionId": sessionID,
				"item":      map[string]any{"itemId": "answer-" + turnID, "kind": "agentMessage", "revision": 1, "status": "completed", "text": "continued-after-approval"},
			}); err != nil {
				return err
			}
			if err := notify("turn/completed", map[string]any{"sessionId": sessionID, "turnId": turnID, "terminal": "completed"}); err != nil {
				return err
			}
		case "turn/interrupt":
			if err := respond(msg.ID, map[string]any{}); err != nil {
				return err
			}
		default:
			if len(msg.ID) != 0 {
				if err := respond(msg.ID, map[string]any{}); err != nil {
					return err
				}
			}
		}
	}
	return scan.Err()
}

func runSyntheticCodexAgent(in io.Reader, out io.Writer) error {
	type request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	scan := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	for scan.Scan() {
		var req request
		if err := json.Unmarshal(scan.Bytes(), &req); err != nil {
			return err
		}
		respond := func(result any) error {
			return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		}
		switch req.Method {
		case "initialize":
			if err := respond(map[string]any{"userAgent": "synthetic-codex"}); err != nil {
				return err
			}
		case "thread/start":
			if err := respond(map[string]any{
				"thread": map[string]any{"id": "codex-thread", "path": "/synthetic/rollout.jsonl"},
				"model":  "cheap", "approvalPolicy": "on-request",
			}); err != nil {
				return err
			}
		case "turn/start":
			if err := respond(map[string]any{"turn": map[string]any{"id": "codex-turn"}}); err != nil {
				return err
			}
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "method": "item/completed",
				"params": map[string]any{"item": map[string]any{"type": "agentMessage", "text": "pong"}},
			}); err != nil {
				return err
			}
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{}}); err != nil {
				return err
			}
		case "turn/interrupt":
			if err := respond(map[string]any{}); err != nil {
				return err
			}
		}
	}
	return scan.Err()
}

func runSyntheticACPAgent(in io.Reader, out io.Writer, statusPath string) error {
	type message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
	}
	if statusPath != "" {
		f, err := os.OpenFile(statusPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(f, "pid:%d\n", os.Getpid())
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	scan := bufio.NewScanner(in)
	enc := json.NewEncoder(out)
	var promptID json.RawMessage
	for scan.Scan() {
		var msg message
		if err := json.Unmarshal(scan.Bytes(), &msg); err != nil {
			return err
		}
		respond := func(result any) error {
			return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": result})
		}
		switch msg.Method {
		case "initialize":
			if err := respond(map[string]any{"protocolVersion": 1, "authMethods": []any{}}); err != nil {
				return err
			}
		case "session/new":
			if err := respond(map[string]any{"sessionId": "split-session"}); err != nil {
				return err
			}
		case "session/prompt":
			promptID = append(promptID[:0], msg.ID...)
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "id": 9001, "method": "session/request_permission",
				"params": map[string]any{
					"sessionId": "split-session",
					"toolCall":  map[string]any{"toolCallId": "call-1", "title": "split approval", "kind": "execute"},
					"options": []map[string]any{
						{"optionId": "allow", "name": "Allow once", "kind": "allow_once"},
						{"optionId": "reject", "name": "Reject", "kind": "reject_once"},
					},
				},
			}); err != nil {
				return err
			}
		case "":
			if string(msg.ID) != "9001" || len(promptID) == 0 {
				continue
			}
			var result struct {
				Outcome struct {
					OptionID string `json:"optionId"`
				} `json:"outcome"`
			}
			if err := json.Unmarshal(msg.Result, &result); err != nil {
				return err
			}
			f, err := os.OpenFile(statusPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(f, "answer:%s\n", result.Outcome.OptionID)
			_ = f.Close()
			if err != nil {
				return err
			}
			if err := enc.Encode(map[string]any{
				"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]any{
					"sessionId": "split-session",
					"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "continued-after-approval"}},
				},
			}); err != nil {
				return err
			}
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(promptID), "result": map[string]any{"stopReason": "end_turn"}}); err != nil {
				return err
			}
			promptID = nil
		case "session/cancel":
			// Notification; shutdown will close stdin immediately afterward.
		default:
			if len(msg.ID) != 0 {
				if err := respond(map[string]any{}); err != nil {
					return err
				}
			}
		}
	}
	return scan.Err()
}
