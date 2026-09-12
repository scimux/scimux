package sessionworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeberg.org/chrberger/scimux/internal/sessionlog"
)

// protocolHarness implements exactly the behaviours already exercised by at
// least one current scimux harness. This round trip is deliberately exhaustive:
// a new wire action has to be added here and justified by a production caller.
type protocolHarness struct {
	launch    LaunchRequest
	sent      string
	decision  PermissionDecision
	delivered string
	recorded  string
	appended  sessionlog.Event
	peekMode  string
	auto      bool
	terminate bool
	calls     []string
	state     State
	fail      map[string]error
}

func (h *protocolHarness) call(name string) error {
	h.calls = append(h.calls, name)
	return h.fail[name]
}

func (h *protocolHarness) Launch(_ context.Context, req LaunchRequest) (string, error) {
	h.launch = req
	return "session-1", h.call("launch")
}

func (h *protocolHarness) Send(_ context.Context, text string) (Delivery, error) {
	h.sent = text
	return Delivery{Status: DeliveryAcknowledged}, h.call("send")
}

func (h *protocolHarness) Clear(context.Context) (Delivery, error) {
	return Delivery{Status: DeliveryUnconfirmed, Text: "/clear"}, h.call("clear")
}

func (h *protocolHarness) ResolveDelivery(context.Context) error { return h.call("resolve-delivery") }

func (h *protocolHarness) Interrupt(context.Context) (ActionEvidence, error) {
	return ActionEvidence{Evidence: "terminal before Escape", Keys: []string{"Escape"}}, h.call("interrupt")
}

func (h *protocolHarness) PreparePermission(_ context.Context, decision PermissionDecision) (PreparedPermission, error) {
	h.decision = decision
	return PreparedPermission{Token: "prepared-1", Evidence: "shell command", Keys: []string{"1", "Enter"}}, h.call("prepare-permission")
}

func (h *protocolHarness) DeliverPermission(_ context.Context, token string) error {
	h.delivered = token
	return h.call("deliver-permission")
}

func (h *protocolHarness) State(context.Context) State {
	_ = h.call("state")
	return h.state
}

func (h *protocolHarness) Peek(_ context.Context, mode string) string {
	h.peekMode = mode
	_ = h.call("peek")
	return "diagnostic tail"
}

func (h *protocolHarness) SetAutoApprove(_ context.Context, enabled bool) (AutoApprove, error) {
	h.auto = enabled
	return AutoApprove{Supported: true, Enabled: enabled, Phase: "armed", Count: 2}, h.call("auto-approve")
}

func (h *protocolHarness) RecordStartFailure(_ context.Context, message string) error {
	h.recorded = message
	return h.call("record-start-failure")
}

func (h *protocolHarness) AppendSessionEvent(_ context.Context, event sessionlog.Event) error {
	h.appended = event
	return h.call("append-session-event")
}

func (h *protocolHarness) Stop(_ context.Context, terminate bool) error {
	h.terminate = terminate
	return h.call("stop")
}

func (h *protocolHarness) Conflict(err error) bool { return errors.Is(err, errHarnessConflict) }

var errHarnessConflict = errors.New("harness conflict")

func TestProtocolRoundTripCoversCurrentHarnessContract(t *testing.T) {
	h := &protocolHarness{state: State{
		HasSession: true, SessionID: "session-1", Live: "active", Attention: "approval",
		LastError: "visible error", Transcript: "/transcript.jsonl", HookID: "hook-1", HookGeneration: 3,
		AXScreenReader: true, Delivery: DeliveryUnconfirmed, TurnDone: true, Supervision: "claude_strict",
		Pending: true, Fallback: true, Source: "transcript", Reason: "tool", Watermark: 19,
		Progress: 2, PendingCalls: 1, WaitingOn: "permission", ReplyReady: true, TurnInFlight: true,
		Compacting: true, CompactTrigger: "auto",
		AutoApprove:      AutoApprove{Supported: true, Enabled: true, Phase: "armed", Count: 1, Error: "later"},
		ElicitationCount: 1, Elicitations: []Elicitation{{Server: "docs", Message: "choose", Mode: "form", URL: "https://example.invalid"}},
		Permission: &PendingPermission{
			RequestID: "incarnation:7", Title: "shell command", ToolKind: "execute", Reason: "workspace",
			Options: []PermissionOption{{Key: "1", Name: "Allow once", Kind: "allow"}}, Dialog: true, Manual: true,
		},
		PermissionBoundary: &PermissionBoundary{Incarnation: "incarnation", MaxSequence: 7},
	}}
	server, err := Listen(t.TempDir(), Identity{WorkerID: "worker-a", Agent: "codex", Build: "build-old"}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Identity != (Identity{WorkerID: "worker-a", Agent: "codex", Build: "build-old"}) ||
		hello.Major != ProtocolMajor || hello.Minor != ProtocolMinor {
		t.Fatalf("hello = %#v", hello)
	}
	if !reflect.DeepEqual(hello.Capabilities, CurrentCapabilities()) {
		t.Fatalf("capabilities = %v, want %v", hello.Capabilities, CurrentCapabilities())
	}

	launch := LaunchRequest{
		NodeID: "node-1", Agent: "codex", Parent: "parent", Title: "Existing", Prompt: "first",
		Description: "description", Rationale: "fork reason", LaneID: "lane", ForkKind: "y-stay",
		Dir: "/work", Model: "cheap", Effort: "low", Transport: "codex",
		SessionID: "existing-session", Existing: true, Transcript: "/old.jsonl", Adopted: true, AXScreenReader: true,
		CreatedAt: "2026-09-11T00:00:00Z", HookID: "hook-1", HookGeneration: 3,
	}
	if sid, err := client.Launch(ctx, launch); err != nil || sid != "session-1" {
		t.Fatalf("Launch = %q, %v", sid, err)
	}
	h.state.Launch = &launch
	if delivery, err := client.Send(ctx, "hello"); err != nil || delivery.Status != DeliveryAcknowledged {
		t.Fatalf("Send = %#v, %v", delivery, err)
	}
	if delivery, err := client.Clear(ctx); err != nil || delivery != (Delivery{Status: DeliveryUnconfirmed, Text: "/clear"}) {
		t.Fatalf("Clear = %#v, %v", delivery, err)
	}
	if err := client.ResolveDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	if evidence, err := client.Interrupt(ctx); err != nil || !reflect.DeepEqual(evidence, ActionEvidence{Evidence: "terminal before Escape", Keys: []string{"Escape"}}) {
		t.Fatalf("Interrupt = %#v, %v", evidence, err)
	}
	prepared, err := client.PreparePermission(ctx, PermissionDecision{RequestID: "incarnation:7", Key: "1"})
	if err != nil {
		t.Fatal(err)
	}
	wantPrepared := PreparedPermission{Token: "prepared-1", Evidence: "shell command", Keys: []string{"1", "Enter"}}
	if !reflect.DeepEqual(prepared, wantPrepared) {
		t.Fatalf("prepared = %#v", prepared)
	}
	if err := client.DeliverPermission(ctx, prepared.Token); err != nil {
		t.Fatal(err)
	}
	state, err := client.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, h.state) {
		t.Fatalf("state = %#v, want %#v", state, h.state)
	}
	if peek, err := client.Peek(ctx, "visible"); err != nil || peek != "diagnostic tail" {
		t.Fatalf("Peek = %q, %v", peek, err)
	}
	auto, err := client.SetAutoApprove(ctx, true)
	if err != nil || !auto.Supported || !auto.Enabled || auto.Count != 2 {
		t.Fatalf("SetAutoApprove = %#v, %v", auto, err)
	}
	if err := client.RecordStartFailure(ctx, "first prompt failed"); err != nil {
		t.Fatal(err)
	}
	event := sessionlog.Event{T: "attention", Attention: &sessionlog.AttentionEvent{Kind: "approval", Status: "start"}}
	if err := client.AppendSessionEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(ctx, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Stopped():
	case <-time.After(time.Second):
		t.Fatal("successful Stop did not notify worker lifetime owner")
	}

	wantCalls := []string{"launch", "send", "clear", "resolve-delivery", "interrupt", "prepare-permission", "deliver-permission", "state", "peek", "auto-approve", "record-start-failure", "append-session-event", "stop"}
	if !reflect.DeepEqual(h.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", h.calls, wantCalls)
	}
	if h.launch != launch || h.sent != "hello" || h.decision != (PermissionDecision{RequestID: "incarnation:7", Key: "1"}) || h.delivered != "prepared-1" || h.recorded != "first prompt failed" || !reflect.DeepEqual(h.appended, event) || h.peekMode != "visible" || !h.auto || !h.terminate {
		t.Fatalf("forwarded values = launch %#v send %q decision %#v deliver %q record %q", h.launch, h.sent, h.decision, h.delivered, h.recorded)
	}
}

func authenticatedRequest(t *testing.T, client *Client, method, path, body string) *http.Response {
	t.Helper()
	req, err := client.request(context.Background(), method, path, []byte(body), ProtocolMajor, ProtocolMinor)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func probeHello(ctx context.Context, link Link, major, minor int) (int, error) {
	c, err := NewClient(link)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	req, err := c.request(ctx, http.MethodGet, "/v1/hello", nil, major, minor)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func TestProtocolRejectsMalformedBoundedAndTrailingRequests(t *testing.T) {
	h := &protocolHarness{}
	server, err := Listen("", Identity{WorkerID: "w", Agent: "pi"}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, _ := NewClient(server.Link())
	defer client.Close()

	for _, tc := range []struct{ path, body string }{
		{"/v1/launch", "{"}, {"/v1/send", "{"},
		{"/v1/permission/prepare", "{"}, {"/v1/permission/deliver", "{"},
		{"/v1/auto-approve", "{"}, {"/v1/start-failure", "{"}, {"/v1/stop", "{"},
		{"/v1/session-event", "{"},
		{"/v1/send", `{"text":"one"}{"text":"two"}`},
		{"/v1/send", `{"text":"` + strings.Repeat("x", maxBody) + `"}`},
	} {
		resp := authenticatedRequest(t, client, http.MethodPost, tc.path, tc.body)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s malformed status = %d", tc.path, resp.StatusCode)
		}
	}
	if len(h.calls) != 0 {
		t.Fatalf("malformed requests reached harness: %v", h.calls)
	}
}

func TestProtocolPreservesHarnessErrorClass(t *testing.T) {
	h := &protocolHarness{fail: map[string]error{"send": errHarnessConflict}}
	server, err := Listen(t.TempDir(), Identity{WorkerID: "w", Agent: "pi"}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, _ := NewClient(server.Link())
	defer client.Close()
	if _, err := client.Send(context.Background(), "x"); !errors.Is(err, ErrConflict) || err.Error() != errHarnessConflict.Error() {
		t.Fatalf("Send error = %v, want classified remote conflict", err)
	}

	h.fail["send"] = errors.New("disk failed")
	if _, err := client.Send(context.Background(), "x"); err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("Send error = %v, want ordinary remote failure", err)
	}
	h.fail["clear"] = errHarnessConflict
	if _, err := client.Clear(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("Clear error = %v, want classified conflict", err)
	}
}

func TestProtocolVersionAndCapabilityCompatibility(t *testing.T) {
	h := &protocolHarness{}
	server, err := Listen(t.TempDir(), Identity{WorkerID: "w", Agent: "pi"}, h)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	link := server.Link()
	tests := []struct {
		name         string
		link         Link
		major, minor int
		want         int
	}{
		{"current", link, ProtocolMajor, ProtocolMinor, http.StatusOK},
		{"future minor", link, ProtocolMajor, ProtocolMinor + 99, http.StatusOK},
		{"wrong token", Link{Socket: link.Socket, Token: "wrong"}, ProtocolMajor, ProtocolMinor, http.StatusForbidden},
		{"old major", link, ProtocolMajor - 1, ProtocolMinor, http.StatusUpgradeRequired},
		{"future major", link, ProtocolMajor + 1, ProtocolMinor, http.StatusUpgradeRequired},
		{"negative minor", link, ProtocolMajor, -1, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, err := probeHello(context.Background(), tc.link, tc.major, tc.minor)
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

func TestProtocolConstructionValidationAndClose(t *testing.T) {
	if _, err := Listen(t.TempDir(), Identity{}, &protocolHarness{}); err == nil {
		t.Fatal("Listen accepted empty identity")
	}
	if _, err := Listen(t.TempDir(), Identity{WorkerID: "w", Agent: "pi"}, nil); err == nil {
		t.Fatal("Listen accepted nil harness")
	}
	if _, err := NewClient(Link{Token: "x"}); err == nil {
		t.Fatal("NewClient accepted empty socket")
	}
	if _, err := NewClient(Link{Socket: "/tmp/x"}); err == nil {
		t.Fatal("NewClient accepted empty token")
	}
	var nilClient *Client
	if _, err := nilClient.Hello(context.Background()); err == nil {
		t.Fatal("nil client succeeded")
	}
	if err := nilClient.Close(); err != nil {
		t.Fatal(err)
	}
	var nilServer *Server
	if nilServer.Link() != (Link{}) || nilServer.Stopped() != nil || nilServer.Close() != nil {
		t.Fatal("nil server methods are not inert")
	}

	parent := t.TempDir()
	server, err := Listen(parent, Identity{WorkerID: "w", Agent: "pi"}, &protocolHarness{})
	if err != nil {
		t.Fatal(err)
	}
	dir, socket := server.dir, server.Link().Socket
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	for _, path := range []string{socket, dir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("runtime path survived Close: %s: %v", path, err)
		}
	}
	dead, _ := NewClient(Link{Socket: socket, Token: "token"})
	if _, err := dead.Peek(context.Background(), ""); err == nil {
		t.Fatal("Peek unexpectedly reached a closed socket")
	}
	if _, err := dead.Hello(context.Background()); err == nil {
		t.Fatal("Hello unexpectedly reached a closed socket")
	}
	if _, err := probeHello(context.Background(), Link{}, ProtocolMajor, ProtocolMinor); err == nil {
		t.Fatal("probeHello accepted an empty link")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientRejectsMalformedResponsesAndHelloMismatch(t *testing.T) {
	response := func(status int, body string) *Client {
		return &Client{link: Link{Socket: "/ignored", Token: "token"}, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status, Status: http.StatusText(status), Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)),
			}, nil
		})}}
	}
	if _, err := response(http.StatusBadGateway, "not-json").Send(context.Background(), "x"); err == nil || err.Error() != "Bad Gateway" {
		t.Fatalf("malformed error response = %v", err)
	}
	if _, err := response(http.StatusOK, "not-json").State(context.Background()); err == nil {
		t.Fatal("State accepted malformed success JSON")
	}
	badHello := fmt.Sprintf(`{"major":%d,"minor":0}`, ProtocolMajor+1)
	if _, err := response(http.StatusOK, badHello).Hello(context.Background()); err == nil {
		t.Fatal("Hello accepted a mismatched response major")
	}
	if _, err := response(http.StatusForbidden, "denied").Peek(context.Background(), ""); err == nil {
		t.Fatal("Peek accepted non-200 response")
	}
	transportFailure := &Client{link: Link{Socket: "/ignored", Token: "token"}, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}}
	if _, err := transportFailure.Send(context.Background(), "x"); err == nil {
		t.Fatal("Send hid transport failure")
	}
}

func TestCurrentCapabilitiesReturnsIndependentCopy(t *testing.T) {
	first := CurrentCapabilities()
	first[0] = "mutated"
	if second := CurrentCapabilities(); second[0] == "mutated" {
		t.Fatal("caller mutated protocol capabilities")
	}
}

func TestProtocolCarriesMaximumPublicPrompts(t *testing.T) {
	const prefix = `{"title":"Large","agent":"opencode","prompt":"`
	const suffix = `"}`
	unicodePrompt := "x" + strings.Repeat("\u2028", (publicJSONBodyMax-len(prefix)-len(suffix)-2)/3) + "x"
	unicodeBody := prefix + unicodePrompt + suffix
	if len(unicodeBody) > publicJSONBodyMax || !json.Valid([]byte(unicodeBody)) {
		t.Fatal("invalid maximum-size public Unicode fixture")
	}

	for _, tc := range []struct {
		name   string
		prompt string
	}{
		{name: "HTML-sensitive", prompt: strings.Repeat("<", publicJSONBodyMax-(4<<10))},
		{name: "Unicode-line-separator", prompt: unicodePrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch := LaunchRequest{
				NodeID: "large", Agent: "opencode", Title: "Large", Dir: "/work",
				Prompt: tc.prompt, Description: tc.prompt,
			}
			var wire bytes.Buffer
			enc := json.NewEncoder(&wire)
			enc.SetEscapeHTML(false)
			if err := enc.Encode(canonicalLaunch(launch)); err != nil {
				t.Fatal(err)
			}
			h := &protocolHarness{}
			server, err := Listen("", Identity{WorkerID: "large", Agent: launch.Agent}, h)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := NewClient(server.Link())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			if _, err := client.Launch(context.Background(), launch); err != nil {
				t.Fatalf("Launch rejected a public-size prompt (%d wire bytes): %v", wire.Len(), err)
			}
			if h.launch.Prompt != tc.prompt || h.launch.Description != "" {
				t.Fatal("Launch did not preserve the prompt in canonical recovery form")
			}
			h.state = State{Launch: &launch, HasSession: true, Live: "quiet"}
			state, err := client.State(context.Background())
			if err != nil {
				t.Fatalf("State rejected public-size recovery metadata: %v", err)
			}
			if state.Launch == nil || state.Launch.Prompt != tc.prompt || state.Launch.Description != "" {
				t.Fatal("State did not preserve the prompt in canonical recovery form")
			}
		})
	}
}

func TestHelloCompatibilityRequiresOnlyTheVersionOneCommonDenominator(t *testing.T) {
	base := Hello{Major: ProtocolMajor, Minor: ProtocolMinor, Capabilities: CurrentCapabilities()}
	if err := CheckCompatibility(base); err != nil {
		t.Fatal(err)
	}
	future := base
	future.Minor += 100
	future.Capabilities = append(future.Capabilities, "future-observation-v1")
	if err := CheckCompatibility(future); err != nil {
		t.Fatalf("additive future worker rejected: %v", err)
	}
	wrongMajor := base
	wrongMajor.Major++
	if err := CheckCompatibility(wrongMajor); err == nil {
		t.Fatal("mismatched major accepted")
	}
	for i, capability := range base.Capabilities {
		missing := base
		missing.Capabilities = append([]string(nil), base.Capabilities[:i]...)
		missing.Capabilities = append(missing.Capabilities, base.Capabilities[i+1:]...)
		if err := CheckCompatibility(missing); err == nil || !strings.Contains(err.Error(), capability) {
			t.Fatalf("missing %q = %v", capability, err)
		}
	}
}

func TestCompatibilityDoesNotRequireFutureMinorCapability(t *testing.T) {
	oldCurrent := capabilities
	capabilities = append(append([]string(nil), versionOneCapabilities...), "future-observation-v1")
	t.Cleanup(func() { capabilities = oldCurrent })
	oldWorker := Hello{Major: ProtocolMajor, Minor: ProtocolMinor, Capabilities: append([]string(nil), versionOneCapabilities...)}
	if err := CheckCompatibility(oldWorker); err != nil {
		t.Fatalf("older major-v1 worker rejected after additive capability: %v", err)
	}
}
