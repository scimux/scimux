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
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
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
	runtime, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1", "SCIMUX_WEB_CHILD_TEST_VERSION=v1.0.0"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	addr := ln.Addr().String()
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
				if tc.transport == "acp" && state.Permission != nil {
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
			if err := workers.Kill(node.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := sessionworker.Discover(data, node.ID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worker locator survived deletion: %v", err)
			}
		})
	}
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
	launcher := `#!/bin/sh
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
while IFS= read -r line; do
  printf '{"type":"user","timestamp":"2026-09-11T00:00:00Z","message":{"role":"user","content":"%s"}}\n' "$line" >> "$transcript"
  printf '{"type":"assistant","timestamp":"2026-09-11T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"worker-reply"}]}}\n' >> "$transcript"
done
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(launcher), 0o755); err != nil {
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
	resp, err := http.DefaultClient.Do(req)
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
