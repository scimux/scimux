package codex

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestPeerNotify(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	if err := p.notify("client/note", map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	line := out.lines()[0]
	if !strings.Contains(line, `"method":"client/note"`) || strings.Contains(line, `"id"`) {
		t.Fatalf("notify frame should have method and no id: %s", line)
	}
}

func TestDispatchRequestNoHandler(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil) // onRequest left nil
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte(`{"id":3,"method":"whatever","params":{}}` + "\n"))
	waitFor(t, func() bool {
		for _, l := range out.lines() {
			if strings.Contains(l, "no handler for whatever") {
				return true
			}
		}
		return false
	})
}

func TestStartThreadRPCError(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	res := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		_, err := c.StartThread(ctx, StartThreadParams{})
		res <- err
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	r := ms.nextReq(t)
	idb, _ := json.Marshal(r.ID)
	ms.writeRaw(t, `{"id":`+string(idb)+`,"error":{"code":-32600,"message":"bad cwd"}}`)
	if err := <-res; err == nil || !strings.Contains(err.Error(), "bad cwd") {
		t.Fatalf("want rpc error, got %v", err)
	}
}

func TestStartThreadDecodeError(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	res := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		_, err := c.StartThread(ctx, StartThreadParams{Model: "gpt-x"})
		res <- err
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	r := ms.nextReq(t)
	// Config with model override should have been sent.
	if !strings.Contains(string(r.Params), `"model":"gpt-x"`) {
		t.Fatalf("model override not forwarded: %s", r.Params)
	}
	ms.reply(t, r.ID, `12345`) // result is not an object → decode error
	if err := <-res; err == nil || !strings.Contains(err.Error(), "decode thread/start") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestRunTurnStartError(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	ctx := context.Background()
	res := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{})
		res <- c.RunTurn(ctx, ti.ID, "x")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	r := ms.nextReq(t)
	idb, _ := json.Marshal(r.ID)
	ms.writeRaw(t, `{"id":`+string(idb)+`,"error":{"code":-32000,"message":"turn refused"}}`)
	if err := <-res; err == nil || !strings.Contains(err.Error(), "turn refused") {
		t.Fatalf("want turn/start error, got %v", err)
	}
}

func TestApprovalHandlerDeclines(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		return "", nil, false // explicit decline
	})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{})
		done <- c.RunTurn(ctx, ti.ID, "x")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.serverRequest(t, 0, "applyPatchApproval", syntheticApproval)
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Error), "approval rejected by handler") {
		t.Fatalf("want handler-rejection error, got %+v", dec)
	}
	ms.note(t, "turn/completed", `{"turn":{}}`)
	<-done
}

func TestApprovalWithAmendmentPayload(t *testing.T) {
	c, ms, _ := newClientWithMock(t)
	// Choose the object-variant decision, exercising payload round-trip.
	c.SetApprovalHandler(func(a Approval) (string, json.RawMessage, bool) {
		for _, d := range a.AvailableDecisions {
			if d.Payload != nil {
				return d.Key, d.Payload, true
			}
		}
		return "", nil, false
	})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, _ = c.Initialize(ctx, "t", "1")
		ti, _ := c.StartThread(ctx, StartThreadParams{})
		done <- c.RunTurn(ctx, ti.ID, "x")
	}()
	ms.reply(t, ms.nextReq(t).ID, `{}`)
	ms.reply(t, ms.nextReq(t).ID, threadStartResult)
	ms.reply(t, ms.nextReq(t).ID, `{"turn":{}}`)
	ms.serverRequest(t, 0, "item/commandExecution/requestApproval", syntheticApproval)
	dec := ms.nextResp(t)
	if !strings.Contains(string(dec.Result), "acceptWithExecpolicyAmendment") ||
		!strings.Contains(string(dec.Result), "execpolicy_amendment") {
		t.Fatalf("amendment payload not round-tripped: %s", dec.Result)
	}
	ms.note(t, "turn/completed", `{"turn":{}}`)
	<-done
}

func TestDecodeItemAgentMessageViaContent(t *testing.T) {
	// agentMessage with no top-level text but content blocks.
	raw := []byte(`{"item":{"type":"agentMessage","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`)
	ev := decodeItem(raw, true)
	if ev == nil || ev.Text != "ab" {
		t.Fatalf("content join = %+v", ev)
	}
}
