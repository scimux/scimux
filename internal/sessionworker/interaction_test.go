package sessionworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestClaudeInteractionCapabilityStaysOptional(t *testing.T) {
	found := false
	for _, capability := range CurrentCapabilities() {
		if capability == CapabilityClaudeInteractionV1 {
			found = true
		}
	}
	if !found {
		t.Fatal("new workers do not advertise claude-interaction-v1")
	}
	for _, capability := range versionOneCapabilities {
		if capability == CapabilityClaudeInteractionV1 {
			t.Fatal("claude-interaction-v1 became a major-1 requirement")
		}
	}
	old := Hello{Major: ProtocolMajor, Minor: ProtocolMinor - 1, Capabilities: append([]string(nil), versionOneCapabilities...)}
	if err := CheckCompatibility(old); err != nil {
		t.Fatalf("worker without %s was rejected: %v", CapabilityClaudeInteractionV1, err)
	}
	with := Hello{Major: ProtocolMajor, Minor: ProtocolMinor, Capabilities: CurrentCapabilities()}
	if err := CheckCompatibility(with); err != nil {
		t.Fatalf("current hello rejected: %v", err)
	}
	var decoded State
	if err := json.Unmarshal([]byte(`{"live":"quiet","has_session":true}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ClaudeInteraction != nil || decoded.Live != "quiet" || !decoded.HasSession {
		t.Fatalf("old state gained an interaction: %#v", decoded)
	}
}

type interactionRPCHarness struct {
	protocolHarness
	result InteractionResult
	got    InteractionAction
	fail   error
}

func (h *interactionRPCHarness) ClaudeInteraction(context.Context) (InteractionView, error) {
	if h.state.ClaudeInteraction == nil {
		return InteractionView{}, ErrInteractionUnsupported
	}
	return *h.state.ClaudeInteraction, nil
}

func (h *interactionRPCHarness) SubmitClaudeInteraction(_ context.Context, action InteractionAction) (InteractionResult, error) {
	h.got = action
	if h.fail != nil {
		return InteractionResult{}, h.fail
	}
	return h.result, nil
}

func TestClaudeInteractionRPCOptionalAndCompatible(t *testing.T) {
	ctx := context.Background()
	plain := &protocolHarness{}
	plainServer, err := Listen(t.TempDir(), Identity{WorkerID: "plain", Agent: "codex", Build: "test"}, plain)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plainServer.Close() })
	plainClient, err := NewClient(plainServer.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plainClient.Close() })

	resp := authenticatedRequest(t, plainClient, http.MethodPost, "/v1/claude-interaction", `{"request_id":"req-1","action":"cancel"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var wire struct {
		Class string `json:"class"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &wire)
	if resp.StatusCode != http.StatusNotFound || wire.Class != "unsupported" {
		t.Fatalf("missing owner = %d class %q body %s", resp.StatusCode, wire.Class, body)
	}
	if len(plain.calls) != 0 {
		t.Fatalf("missing owner invoked harness calls %v", plain.calls)
	}

	owner := &interactionRPCHarness{
		result: InteractionResult{Status: "accepted", Delivery: "recorded", RequestID: "req-1", Emitted: false, Phase: "recorded"},
	}
	owner.state.ClaudeInteraction = &InteractionView{
		RequestID: "req-1", Phase: "pending", Tool: "AskUserQuestion", NativeKeys: false,
		Questions: []InteractionQuestion{{
			ID: "q1", Text: "Pick a letter?", Multi: false,
			Options: []InteractionOption{{ID: "q1o1", Label: "Alpha"}, {ID: "q1o2", Label: "Beta"}},
		}},
	}
	server, err := Listen(t.TempDir(), Identity{WorkerID: "claude", Agent: "claude", Build: "test"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	hello, err := client.Hello(ctx)
	if err != nil {
		t.Fatal(err)
	}
	advertised := false
	for _, capability := range hello.Capabilities {
		if capability == CapabilityClaudeInteractionV1 {
			advertised = true
		}
	}
	if !advertised {
		t.Fatal("hello omitted claude-interaction-v1")
	}
	bad := authenticatedRequest(t, client, http.MethodPost, "/v1/claude-interaction", `{`)
	badBody, _ := io.ReadAll(bad.Body)
	bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed worker body = %d %s", bad.StatusCode, badBody)
	}

	view, err := client.State(ctx)
	if err != nil || view.ClaudeInteraction == nil || view.ClaudeInteraction.RequestID != "req-1" || view.ClaudeInteraction.NativeKeys {
		t.Fatalf("state interaction = %#v %v", view.ClaudeInteraction, err)
	}
	again, err := NewClient(server.Link())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	againState, err := again.State(ctx)
	if err != nil || againState.ClaudeInteraction == nil || againState.ClaudeInteraction.RequestID != "req-1" {
		t.Fatalf("reconnect state = %#v %v", againState.ClaudeInteraction, err)
	}

	action := InteractionAction{
		RequestID: "req-1", Action: "answer",
		Answers: []InteractionSelection{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}},
	}
	result, err := client.SubmitClaudeInteraction(ctx, action)
	if err != nil || result.Status != "accepted" || result.Delivery != "recorded" || result.Emitted || result.Phase != "recorded" {
		t.Fatalf("submit = %#v %v", result, err)
	}
	if !reflect.DeepEqual(owner.got, action) {
		t.Fatalf("worker saw %#v", owner.got)
	}

	owner.fail = ErrInteractionUnsupported
	if _, err := client.SubmitClaudeInteraction(ctx, action); !errors.Is(err, ErrInteractionUnsupported) {
		t.Fatalf("unsupported err = %v", err)
	}
	owner.fail = ErrInteractionBadRequest
	if _, err := client.SubmitClaudeInteraction(ctx, action); !errors.Is(err, ErrInteractionBadRequest) {
		t.Fatalf("bad action err = %v", err)
	}
	owner.fail = ErrConflict
	if _, err := client.SubmitClaudeInteraction(ctx, action); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	owner.fail = errors.New("audit failed")
	if _, err := client.SubmitClaudeInteraction(ctx, action); err == nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrInteractionBadRequest) {
		t.Fatalf("storage err = %v", err)
	}

	dead, err := NewClient(Link{Socket: "/tmp/sciw-absent.sock", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dead.Close() })
	if _, err := dead.SubmitClaudeInteraction(ctx, action); err == nil {
		t.Fatal("missing socket produced a result")
	} else if _, ok := err.(*remoteError); ok {
		t.Fatalf("transport failure was classified as a worker status: %v", err)
	}
}
