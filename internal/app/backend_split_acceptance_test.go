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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/acp"
	"codeberg.org/chrberger/scimux/internal/backend"
	"codeberg.org/chrberger/scimux/internal/remote"
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
func TestWebUpdatePreservesRealACPProcessAndPermission(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	statusPath := filepath.Join(t.TempDir(), "agent-status")
	spawned := make(chan *syntheticACPProcess, 1)
	m := acp.NewManagerWithRunner(a.sessionsDir, syntheticACPRunner(t, statusPath, spawned))
	m.SetAssetHook(a.assetHook)
	a.acp = acpManager{m}

	data := filepath.Dir(a.storePath)
	if err := os.Chmod(data, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cmd := &Command{
		Stdout: io.Discard, Stderr: io.Discard,
		listener: ln, listenAddr: ln.Addr().String(), Config: remote.Config{DataDir: data},
	}
	claimCommandOwnership(t, cmd)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := startSplitRuntime(context.Background(), a, cmd, exe, splitRuntimeOptions{
		configureWeb: func(s *webSupervisor) {
			s.readyTimeout, s.drainTimeout = 10*time.Second, 10*time.Second
			s.childArgs = []string{"-test.run=^TestWebChildHelperProcess$"}
			s.extraEnv = []string{"SCIMUX_WEB_CHILD_TEST=1"}
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
	var proc *syntheticACPProcess
	select {
	case proc = <-spawned:
	case <-time.After(3 * time.Second):
		t.Fatal("ACP manager did not spawn the synthetic protocol process")
	}
	harnessPID := proc.cmd.Process.Pid

	before := waitForPublicPermission(t, addr, created.ID)
	requestID, _ := before["perm_request_id"].(string)
	if requestID == "" || before["turn_in_flight"] != true {
		t.Fatalf("pending permission before update = %#v", before)
	}

	// Drive rotation through POST /api/update, including verified download,
	// standby preparation, response-safe drain, and activation. The candidate
	// is a tiny launcher for this same test binary, not a fake supervisor call.
	launcher := []byte("#!/bin/sh\nexec \"" + strings.ReplaceAll(exe, "\"", "\\\"") + "\" \"$@\"\n")
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
	if got := proc.cmd.Process.Pid; got != harnessPID {
		t.Fatalf("ACP harness PID changed across web update: %d -> %d", harnessPID, got)
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
	select {
	case <-proc.done:
	case <-time.After(3 * time.Second):
		t.Fatal("full muxer stop did not reap the ACP harness")
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

type syntheticACPProcess struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Reader
	done     chan struct{}
	waitOnce sync.Once
	killOnce sync.Once
	waitErr  error
	killErr  error
}

func (p *syntheticACPProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *syntheticACPProcess) Stdout() io.Reader     { return p.stdout }
func (p *syntheticACPProcess) Kill() error {
	p.killOnce.Do(func() {
		_ = p.stdin.Close()
		p.killErr = p.cmd.Process.Kill()
		if errors.Is(p.killErr, os.ErrProcessDone) {
			p.killErr = nil
		}
	})
	return p.killErr
}
func (p *syntheticACPProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	})
	return p.waitErr
}

func syntheticACPRunner(t *testing.T, statusPath string, spawned chan<- *syntheticACPProcess) acp.Runner {
	t.Helper()
	return func(_, _, dir, _, _ string) (acp.Process, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSyntheticACPAgentHelperProcess$")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SCIMUX_SYNTHETIC_ACP=1", "SCIMUX_SYNTHETIC_ACP_STATUS="+statusPath)
		cmd.Stderr = io.Discard
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		proc := &syntheticACPProcess{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan struct{})}
		spawned <- proc
		return proc, nil
	}
}

func TestSyntheticACPAgentHelperProcess(t *testing.T) {
	if os.Getenv("SCIMUX_SYNTHETIC_ACP") != "1" {
		return
	}
	if err := runSyntheticACPAgent(os.Stdin, os.Stdout, os.Getenv("SCIMUX_SYNTHETIC_ACP_STATUS")); err != nil {
		t.Fatal(err)
	}
}

func runSyntheticACPAgent(in io.Reader, out io.Writer, statusPath string) error {
	type message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
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
