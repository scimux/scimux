package muse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRPCErrorRegistry(t *testing.T) {
	entries := []struct {
		code      int
		kind      string
		retryable bool
	}{
		{-32700, "parseError", false},
		{-32600, "invalidRequest", false},
		{-32601, "methodNotFound", false},
		{-32602, "invalidParams", false},
		{-32603, "internal", false},
		{-32001, "overloaded", true},
		{-32002, "inputTooLarge", false},
		{-32010, "capabilityRequired", false},
		{-32011, "notFound", false},
		{-32013, "interrupted", false},
		{-32014, "cancelled", false},
		{-32020, "sessionNotFound", false},
		{-32021, "sessionInUse", false},
		{-32022, "sessionAmbiguous", false},
		{-32023, "forkBoundaryInvalid", false},
		{-32024, "sessionNotLoaded", false},
		{-32025, "sessionStreamMismatch", false},
		{-32030, "commandRejected", false},
		{-32031, "backpressured", true},
		{-32040, "viewTruncated", false},
		{-32041, "outputUnavailable", false},
		{-32042, "boundaryPruned", false},
		{-32050, "approvalNotFound", false},
		{-32051, "approvalAlreadyResolved", false},
		{-32052, "approvalChoiceInvalid", false},
		{-32053, "approvalRequirementStale", false},
		{-32054, "approvalReviewerUnavailable", false},
		{-32055, "userInputNotFound", false},
		{-32056, "userInputAlreadySettled", false},
		{-32057, "userInputAnswerInvalid", false},
	}
	seen := map[int]bool{}
	for _, e := range entries {
		if seen[e.code] {
			t.Fatalf("duplicate registry code %d", e.code)
		}
		seen[e.code] = true
		err := &RPCError{Code: e.code, Message: "x", Method: "m"}
		if err.Kind() != e.kind {
			t.Errorf("code %d Kind()=%q, want %q", e.code, err.Kind(), e.kind)
		}
		if err.Retryable() != e.retryable {
			t.Errorf("code %d Retryable()=%v, want %v", e.code, err.Retryable(), e.retryable)
		}
		if err.Error() == "" {
			t.Errorf("code %d empty Error()", e.code)
		}
		if !strings.Contains(err.Error(), "m") {
			t.Errorf("Error() %q missing method", err.Error())
		}
	}
	unknown := &RPCError{Code: -1, Message: "nope"}
	if unknown.Kind() != "" {
		t.Errorf("unknown Kind()=%q, want empty", unknown.Kind())
	}
	if unknown.Retryable() {
		t.Error("unknown code must not be retryable")
	}
	if !strings.Contains(unknown.Error(), "code -1") {
		t.Errorf("Error()=%q", unknown.Error())
	}
}

func TestRPCErrorWireRetryableDoesNotOverride(t *testing.T) {
	s := newScriptedPeer(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(ctx, "x", nil)
		done <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"error":{"code":-32601,"message":"no","data":{"retryable":true}}}`)
	err := <-done
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("err = %v, want RPCError", err)
	}
	if rpc.Retryable() {
		t.Fatal("registry forbids retryable on methodNotFound even if wire data says otherwise")
	}
	if rpc.Kind() != "methodNotFound" {
		t.Fatalf("Kind=%q", rpc.Kind())
	}
	if string(rpc.Data) == "" || !bytes.Contains(rpc.Data, []byte("retryable")) {
		t.Fatalf("raw data not preserved: %s", rpc.Data)
	}
	if rpc.Code != -32601 || rpc.Message != "no" || rpc.Method != "x" {
		t.Fatalf("RPCError = %+v", rpc)
	}
}

func TestCanonIDMatching(t *testing.T) {
	cases := []struct {
		a, b  string
		match bool
	}{
		{`1`, `"1"`, true},
		{`1`, `1`, true},
		{`"1"`, `"1"`, true},
		{`1`, `null`, false},
		{`"null"`, `null`, false},
		{`"null"`, `"null"`, true},
		{`1`, `"01"`, false},
		{`1`, `"2"`, false},
		{`"abc"`, `"abc"`, true},
		{`1`, `"1.0"`, false},
		{`"0"`, `0`, true},
		{`"00"`, `0`, false},
		{`""`, `""`, true},
	}
	for _, tc := range cases {
		ka := canonID(json.RawMessage(tc.a))
		kb := canonID(json.RawMessage(tc.b))
		if tc.match && (ka == "" || ka != kb) {
			t.Errorf("canonID(%s)=%q canonID(%s)=%q, want match", tc.a, ka, tc.b, kb)
		}
		if !tc.match && ka != "" && ka == kb {
			t.Errorf("canonID(%s)=canonID(%s)=%q, want distinct", tc.a, tc.b, ka)
		}
		if string(bytes.TrimSpace([]byte(tc.a))) == "null" && ka != "" {
			t.Errorf("JSON null must not be an ID, got %q", ka)
		}
	}
}

func TestCallNumericIDSatisfiedByStringEcho(t *testing.T) {
	s := newScriptedPeer(t)
	ctx := context.Background()
	got := make(chan json.RawMessage, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := s.peer.Call(ctx, "echo", map[string]any{"k": 1})
		got <- res
		errc <- err
	}()
	req := s.readJSON(t)
	if req["method"] != "echo" {
		t.Fatalf("method=%v", req["method"])
	}
	if _, ok := req["id"].(float64); !ok {
		t.Fatalf("client id is %T %v, want number", req["id"], req["id"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":"1","result":{"ok":true}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(<-got, []byte(`"ok"`)) {
		t.Fatalf("result missing")
	}
}

func TestCallResultNullIsSuccess(t *testing.T) {
	s := newScriptedPeer(t)
	ctx := context.Background()
	errc := make(chan error, 1)
	resc := make(chan json.RawMessage, 1)
	go func() {
		res, err := s.peer.Call(ctx, "n", nil)
		resc <- res
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":null}`)
	if err := <-errc; err != nil {
		t.Fatalf("null result must succeed: %v", err)
	}
	res := bytes.TrimSpace(<-resc)
	if string(res) != "null" {
		t.Fatalf("result = %s, want null", res)
	}
}

func TestCallContextCancelReleasesWaiter(t *testing.T) {
	s := newScriptedPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(ctx, "slow", nil)
		errc <- err
	}()
	_ = s.readJSON(t)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled Call leaked")
	}
	// A late response must not crash the peer or complete a new call with the stale id.
	s.writeRaw(t, `{"jsonrpc":"2.0","id":1,"result":{"late":true}}`)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	errc2 := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(ctx2, "next", nil)
		errc2 <- err
	}()
	req := s.readJSON(t)
	if req["method"] != "next" {
		t.Fatalf("second call method=%v", req["method"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"ok":1}}`)
	if err := <-errc2; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCallsOutOfOrder(t *testing.T) {
	s := newScriptedPeer(t)
	const n = 16
	type req struct {
		id json.RawMessage
		i  int
	}
	seen := make(chan req, n)
	go func() {
		for i := 0; i < n; i++ {
			raw := s.readLine(t)
			var m struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					I int `json:"i"`
				} `json:"params"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("decode: %v", err)
				return
			}
			seen <- req{id: m.ID, i: m.Params.I}
		}
	}()
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			res, err := s.peer.Call(context.Background(), "echo", map[string]int{"i": i})
			if err != nil {
				errc <- err
				return
			}
			var got struct {
				I int `json:"i"`
			}
			if json.Unmarshal(res, &got) != nil || got.I != i {
				errc <- errors.New("bad result")
				return
			}
			errc <- nil
		}()
	}
	var reqs []req
	for i := 0; i < n; i++ {
		reqs = append(reqs, <-seen)
	}
	for i := len(reqs) - 1; i >= 0; i-- {
		r := reqs[i]
		s.writeJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      r.id,
			"result":  map[string]int{"i": r.i},
		})
	}
	for i := 0; i < n; i++ {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloseFailsWaitersAndIsIdempotent(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := s.peer.Call(context.Background(), "x", nil)
			errc <- err
		}()
	}
	for i := 0; i < 3; i++ {
		_ = s.readJSON(t)
	}
	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.peer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	for i := 0; i < 3; i++ {
		select {
		case err := <-errc:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("err=%v, want ErrClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiter not released")
		}
	}
	_, err := s.peer.Call(context.Background(), "after", nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close: %v", err)
	}
}

func TestConcurrentClose(t *testing.T) {
	s := newScriptedPeer(t)
	var wg sync.WaitGroup
	wg.Add(8)
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			_ = s.peer.Close()
		}()
	}
	wg.Wait()
}

func TestMalformedAndUnknownFramesDoNotCrash(t *testing.T) {
	s := newScriptedPeer(t)
	s.writeRaw(t, `{not json`)
	s.writeRaw(t, `[]`)
	s.writeRaw(t, `"str"`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"nope"}`)     // unknown notification
	s.writeRaw(t, `{"jsonrpc":"2.0","id":99,"result":{}}`) // unknown response
	s.writeRaw(t, `{"jsonrpc":"2.0"}`)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "still", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestNotificationsArriveInOrder(t *testing.T) {
	s := newScriptedPeer(t)
	var got []string
	var mu sync.Mutex
	unblocked := make(chan struct{})
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "hold" {
			<-unblocked
		}
		mu.Lock()
		got = append(got, method)
		mu.Unlock()
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"hold"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"a"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"b"}`)
	// Read loop must still accept a Call while hold is blocked.
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "ping", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	if req["method"] != "ping" {
		t.Fatalf("read loop blocked by notify handler; method=%v", req["method"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call blocked behind notify handler")
	}
	close(unblocked)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		snap := append([]string(nil), got...)
		mu.Unlock()
		if n >= 3 {
			want := []string{"hold", "a", "b"}
			if strings.Join(snap, ",") != strings.Join(want, ",") {
				t.Fatalf("order=%v, want %v", snap, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notify timeout, got %v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerRequestOffReadLoopAndAlwaysAnswered(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		close(entered)
		<-release
		return map[string]any{"ok": true}, nil
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":"null","method":"approval/request","params":{"x":1}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not entered")
	}
	// Read loop must still work while the request handler is blocked.
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "ping", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	if req["method"] != "ping" {
		t.Fatalf("method=%v", req["method"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	close(release)
	resp := s.readJSON(t)
	if resp["error"] != nil {
		t.Fatalf("response error=%v", resp["error"])
	}
	if resp["id"] != "null" {
		t.Fatalf("id=%v, want string null", resp["id"])
	}
}

func TestUnknownServerRequestMethodNotFound(t *testing.T) {
	s := newScriptedPeer(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":7,"method":"no/such"}`)
	resp := s.readJSON(t)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("want error, got %v", resp)
	}
	if errObj["code"].(float64) != -32601 {
		t.Fatalf("code=%v", errObj["code"])
	}
}

func TestRequestHandlerPanicDoesNotKillPeer(t *testing.T) {
	s := newScriptedPeer(t)
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		panic("boom")
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":3,"method":"x"}`)
	resp := s.readJSON(t)
	if resp["error"] == nil {
		t.Fatalf("want error response after panic, got %v", resp)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "still", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestHandlerErrorBecomesRPCError(t *testing.T) {
	s := newScriptedPeer(t)
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		return nil, &RPCError{Code: -32602, Message: "bad"}
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":4,"method":"y"}`)
	resp := s.readJSON(t)
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(float64) != -32602 {
		t.Fatalf("code=%v", errObj["code"])
	}
}

func TestLargeMessageRoundTrip(t *testing.T) {
	s := newScriptedPeer(t)
	payload := strings.Repeat("a", 1<<20)
	errc := make(chan error, 1)
	resc := make(chan json.RawMessage, 1)
	go func() {
		res, err := s.peer.Call(context.Background(), "big", map[string]string{"p": payload})
		resc <- res
		errc <- err
	}()
	req := s.readJSON(t)
	params := req["params"].(map[string]any)
	if params["p"].(string) != payload {
		t.Fatal("outbound large payload truncated")
	}
	s.writeJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      req["id"],
		"result":  map[string]string{"p": payload},
	})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(<-resc, []byte(payload[:32])) {
		t.Fatal("inbound large payload missing")
	}
}

func TestOversizedMessageReleasesWaiters(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "x", nil)
		errc <- err
	}()
	_ = s.readJSON(t)
	// 8 MiB + a bit, as one line. Write from another goroutine: the scanner
	// stops at maxFrame and will not drain the rest of the pipe.
	huge := bytes.Repeat([]byte("x"), 8<<20+64)
	go func() {
		_, _ = s.toPeer.Write(huge)
		_, _ = s.toPeer.Write([]byte("\n"))
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want error for oversized frame")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not released after oversized frame")
	}
}

func TestSerializedWrites(t *testing.T) {
	s := newScriptedPeer(t)
	const n = 32
	errc := make(chan error, 1)
	go func() {
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			i := i
			go func() {
				defer wg.Done()
				if err := s.peer.Notify("n", map[string]int{"i": i}); err != nil {
					errc <- err
				}
			}()
		}
		wg.Wait()
		errc <- nil
	}()
	dec := json.NewDecoder(s.fromPeer)
	for i := 0; i < n; i++ {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m["method"] != "n" {
			t.Fatalf("frame %d method=%v (invalid JSON interleaving?)", i, m["method"])
		}
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notify did not finish")
	}
}

func TestOutgoingJSONRPCVersionAndOneObjectPerLine(t *testing.T) {
	s := newScriptedPeer(t)
	go func() { _, _ = s.peer.Call(context.Background(), "m", map[string]int{"a": 1}) }()
	line := s.readLine(t)
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("want exactly one newline-terminated object, got %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if m["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc=%v", m["jsonrpc"])
	}
	if m["method"] != "m" {
		t.Fatalf("method=%v", m["method"])
	}
	s.peer.Close()
}

func TestNotifyPreservesUnknownFieldsForHandler(t *testing.T) {
	s := newScriptedPeer(t)
	got := make(chan json.RawMessage, 1)
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		got <- append(json.RawMessage(nil), params...)
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"x","kind":"mystery","extra":1},"_meta":{"k":true}}}`)
	select {
	case p := <-got:
		if !bytes.Contains(p, []byte(`"extra"`)) || !bytes.Contains(p, []byte(`"_meta"`)) {
			t.Fatalf("unknown fields dropped: %s", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notify not delivered")
	}
}

func TestRPCErrorNilMethods(t *testing.T) {
	var e *RPCError
	if e.Error() == "" || e.Kind() != "" || e.Retryable() {
		t.Fatalf("nil RPCError methods: %q %q %v", e.Error(), e.Kind(), e.Retryable())
	}
}

func TestCallWriteFailureAndPreCanceledContext(t *testing.T) {
	s := newScriptedPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.peer.Call(ctx, "x", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: %v", err)
	}
	if _, err := s.peer.Call(context.Background(), "x", make(chan int)); err == nil {
		t.Fatal("unmarshalable params must fail")
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "y", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, "\n\n")
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":null}`)
	if err := <-errc; err != nil {
		t.Fatalf("null result should succeed: %v", err)
	}
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		return nil, &RPCError{Code: -32602, Message: "bad", Data: json.RawMessage(`{"k":1}`)}
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":9,"method":"z"}`)
	resp := s.readJSON(t)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["data"] == nil {
		t.Fatalf("want data on handler RPCError: %v", resp)
	}
	_ = s.peer.Close()
	s.peer.enqueueNotify(notifyItem{in: inbound{Method: "late"}})
}

func TestCallNilContextAndClosedPeer(t *testing.T) {
	s := newScriptedPeer(t)
	s.peer.trace = tracerFunc(func(string, []byte) {})
	go func() {
		req := s.readJSON(t)
		s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	}()
	if _, err := s.peer.Call(nil, "ping", map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.peer.Close()
	if _, err := s.peer.Call(context.Background(), "x", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after close: %v", err)
	}
	if err := s.peer.Notify("n", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Notify after close: %v", err)
	}
}

func TestRequestHandlerGenericErrorAndNilResult(t *testing.T) {
	s := newScriptedPeer(t)
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		if method == "fail" {
			return nil, errors.New("boom")
		}
		return nil, nil
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":1,"method":"fail"}`)
	resp := s.readJSON(t)
	if resp["error"] == nil {
		t.Fatal("want internal error")
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":2,"method":"ok"}`)
	resp = s.readJSON(t)
	if resp["error"] != nil {
		t.Fatalf("error=%v", resp["error"])
	}
}

func TestCanonIDOddValues(t *testing.T) {
	if canonID(json.RawMessage(`true`)) != "" {
		t.Fatal("boolean id must be invalid")
	}
	if canonID(json.RawMessage(`{}`)) != "" || canonID(json.RawMessage(`[]`)) != "" {
		t.Fatal("object/array ids must be invalid")
	}
	if canonID(json.RawMessage(`"not-int"`)) != "s:not-int" {
		t.Fatalf("got %q", canonID(json.RawMessage(`"not-int"`)))
	}
	if canonID(nil) != "" {
		t.Fatal("nil id")
	}
}

func TestInvalidJSONRPCVersionIgnored(t *testing.T) {
	s := newScriptedPeer(t)
	called := int32(0)
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		atomic.AddInt32(&called, 1)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "ping", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"1.0","id":`+rawID(t, req)+`,"result":{"stolen":true}}`)
	s.writeRaw(t, `{"id":`+rawID(t, req)+`,"result":{"stolen":true}}`)
	s.writeRaw(t, `{"jsonrpc":"1.0","method":"ghost2"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"ok":true}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("wrong-version notification invoked handler")
	}
}

func TestInvalidResponseDoesNotStealWaiter(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	resc := make(chan json.RawMessage, 1)
	go func() {
		res, err := s.peer.Call(context.Background(), "ping", nil)
		resc <- res
		errc <- err
	}()
	req := s.readJSON(t)
	id := rawID(t, req)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{"ok":1},"error":{"code":-1,"message":"x"}}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":true,"result":{"ok":1}}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{"ok":1}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(<-resc, []byte(`"ok"`)) {
		t.Fatal("valid result lost")
	}
}

func TestInvalidRequestGetsInvalidRequestError(t *testing.T) {
	s := newScriptedPeer(t)
	s.writeRaw(t, `{"jsonrpc":"1.0","id":5,"method":"x"}`)
	resp := s.readJSON(t)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32600 {
		t.Fatalf("want -32600, got %v", resp)
	}
	if resp["id"] != nil {
		t.Fatalf("invalid request response id=%v, want null", resp["id"])
	}
}

func TestPeerCloseInterruptsBlockedRead(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := newPeer(inR, outW)
	p.start()
	t.Cleanup(func() { _ = inW.Close() })
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.readFinished():
	case <-time.After(2 * time.Second):
		t.Fatal("read loop still running after Close")
	}
	_ = outR
}

type tracerFunc func(string, []byte)

func (f tracerFunc) Trace(dir string, line []byte) { f(dir, line) }

func TestNullIDIsNotARequest(t *testing.T) {
	s := newScriptedPeer(t)
	called := int32(0)
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		atomic.AddInt32(&called, 1)
		return map[string]any{}, nil
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":null,"method":"approval/request"}`)
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("JSON null id must not be treated as a request")
	}
}

type scriptedPeer struct {
	peer     *peer
	toPeer   *io.PipeWriter
	fromPeer *bufio.Reader
}

func newScriptedPeer(t *testing.T) *scriptedPeer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := newPeer(inR, outW)
	p.start()
	t.Cleanup(func() {
		_ = p.Close()
		_ = inW.Close()
	})
	return &scriptedPeer{peer: p, toPeer: inW, fromPeer: bufio.NewReader(outR)}
}

func (s *scriptedPeer) writeRaw(t *testing.T, line string) {
	t.Helper()
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(s.toPeer, line); err != nil {
		t.Fatal(err)
	}
}

func (s *scriptedPeer) writeJSON(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s.writeRaw(t, string(b))
}

func (s *scriptedPeer) readLine(t *testing.T) []byte {
	t.Helper()
	line, err := s.fromPeer.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func (s *scriptedPeer) readJSON(t *testing.T) map[string]any {
	t.Helper()
	line := s.readLine(t)
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", line, err)
	}
	return m
}

func (s *scriptedPeer) tryReadJSON(t *testing.T, d time.Duration) (map[string]any, bool) {
	t.Helper()
	type res struct {
		m   map[string]any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := s.fromPeer.ReadBytes('\n')
		if err != nil {
			ch <- res{err: err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{m: m}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.m, true
	case <-time.After(d):
		return nil, false
	}
}

func rawID(t *testing.T, req map[string]any) string {
	t.Helper()
	b, err := json.Marshal(req["id"])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBlockedNotifyDoesNotStallOrdinaryCall(t *testing.T) {
	s := newScriptedPeer(t)
	unblocked := make(chan struct{})
	entered := make(chan struct{})
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "hold" {
			close(entered)
			<-unblocked
		}
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"hold"}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hold not entered")
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "ping", nil)
		errc <- err
	}()
	req := s.readJSON(t)
	if req["method"] != "ping" {
		t.Fatalf("read loop blocked; method=%v", req["method"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary Call blocked behind notify handler")
	}
	close(unblocked)
}

func TestNotificationInputOrderRetained(t *testing.T) {
	s := newScriptedPeer(t)
	got := make(chan string, 8)
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		got <- method
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"n1"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"n2"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"n3"}`)
	var order []string
	deadline := time.After(2 * time.Second)
	for len(order) < 3 {
		select {
		case m := <-got:
			order = append(order, m)
		case <-deadline:
			t.Fatalf("order=%v", order)
		}
	}
	if strings.Join(order, ",") != "n1,n2,n3" {
		t.Fatalf("order=%v", order)
	}
}

func TestJSONRPCIDClassificationDispatch(t *testing.T) {
	type spec struct {
		name        string
		frame       string
		wantRequest bool
		wantInvalid bool
	}
	cases := []spec{
		{name: "method string id", frame: `{"jsonrpc":"2.0","id":"abc","method":"n"}`, wantRequest: true},
		{name: "method string null", frame: `{"jsonrpc":"2.0","id":"null","method":"n"}`, wantRequest: true},
		{name: "method integer id", frame: `{"jsonrpc":"2.0","id":1,"method":"n"}`, wantRequest: true},
		{name: "method negative id", frame: `{"jsonrpc":"2.0","id":-7,"method":"n"}`, wantRequest: true},
		{name: "method decimal id", frame: `{"jsonrpc":"2.0","id":1.5,"method":"n"}`, wantRequest: true},
		{name: "method exponent id", frame: `{"jsonrpc":"2.0","id":1e2,"method":"n"}`, wantRequest: true},
		{name: "method boolean id", frame: `{"jsonrpc":"2.0","id":true,"method":"x"}`, wantInvalid: true},
		{name: "method object id", frame: `{"jsonrpc":"2.0","id":{},"method":"x"}`, wantInvalid: true},
		{name: "method array id", frame: `{"jsonrpc":"2.0","id":[],"method":"x"}`, wantInvalid: true},
		{name: "method false id", frame: `{"jsonrpc":"2.0","id":false,"method":"x"}`, wantInvalid: true},
		{name: "response boolean id", frame: `{"jsonrpc":"2.0","id":true,"result":{"ok":1}}`},
		{name: "response object id", frame: `{"jsonrpc":"2.0","id":{},"result":{"ok":1}}`},
		{name: "response array id", frame: `{"jsonrpc":"2.0","id":[],"result":{"ok":1}}`},
		{name: "response null id", frame: `{"jsonrpc":"2.0","id":null,"result":{"ok":1}}`},
		{name: "wrong version method absent", frame: `{"jsonrpc":"1.0","method":"ghost"}`},
		{name: "wrong version method boolean", frame: `{"jsonrpc":"1.0","id":true,"method":"x"}`, wantInvalid: true},
		{name: "missing version method present id", frame: `{"id":5,"method":"x"}`, wantInvalid: true},
		{name: "wrong version notify", frame: `{"jsonrpc":"1.0","id":null,"method":"ghost"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedPeer(t)
			var notifyN, reqN int32
			mark := make(chan struct{})
			s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
				if method == "marker" {
					close(mark)
					return
				}
				atomic.AddInt32(&notifyN, 1)
			}
			s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
				atomic.AddInt32(&reqN, 1)
				return map[string]any{}, nil
			}
			s.writeRaw(t, tc.frame)
			if tc.wantRequest {
				deadline := time.Now().Add(2 * time.Second)
				for atomic.LoadInt32(&reqN) == 0 && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if atomic.LoadInt32(&reqN) != 1 {
					t.Fatalf("onRequest count=%d", reqN)
				}
				if atomic.LoadInt32(&notifyN) != 0 {
					t.Fatal("valid request invoked onNotify")
				}
				resp := s.readJSON(t)
				if resp["error"] != nil {
					t.Fatalf("valid request error=%v", resp["error"])
				}
				return
			}
			s.writeRaw(t, `{"jsonrpc":"2.0","method":"marker"}`)
			select {
			case <-mark:
			case <-time.After(2 * time.Second):
				t.Fatal("marker not delivered")
			}
			if atomic.LoadInt32(&notifyN) != 0 {
				t.Fatalf("onNotify invoked for %s (count=%d)", tc.name, notifyN)
			}
			if atomic.LoadInt32(&reqN) != 0 {
				t.Fatalf("onRequest invoked for %s (count=%d)", tc.name, reqN)
			}
			if tc.wantInvalid {
				resp, ok := s.tryReadJSON(t, 2*time.Second)
				if !ok {
					t.Fatal("want -32600 invalid request reply")
				}
				errObj, _ := resp["error"].(map[string]any)
				if errObj == nil || errObj["code"].(float64) != -32600 {
					t.Fatalf("want -32600, got %v", resp)
				}
				if resp["id"] != nil {
					t.Fatalf("invalid request id=%v, want null", resp["id"])
				}
			}
		})
	}
}

func TestJSONRPCIDNotificationFormDelivered(t *testing.T) {
	for _, frame := range []string{
		`{"jsonrpc":"2.0","method":"hello"}`,
		`{"jsonrpc":"2.0","id":null,"method":"hello"}`,
	} {
		s := newScriptedPeer(t)
		got := make(chan string, 1)
		s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
			got <- method
		}
		s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
			t.Errorf("onRequest invoked for %s", frame)
			return nil, nil
		}
		s.writeRaw(t, frame)
		select {
		case m := <-got:
			if m != "hello" {
				t.Fatalf("method=%s", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("notification not delivered: %s", frame)
		}
	}
}

func TestInvalidIDRequestProducesExactlyOneInvalidRequest(t *testing.T) {
	s := newScriptedPeer(t)
	var notifyN, reqN int32
	mark := make(chan struct{})
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		atomic.AddInt32(&reqN, 1)
		return map[string]any{}, nil
	}
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		if method == "marker" {
			close(mark)
			return
		}
		atomic.AddInt32(&notifyN, 1)
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":true,"method":"x"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"marker"}`)
	select {
	case <-mark:
	case <-time.After(2 * time.Second):
		t.Fatal("marker not delivered")
	}
	if atomic.LoadInt32(&notifyN) != 0 {
		t.Fatal("invalid-id request invoked onNotify")
	}
	if atomic.LoadInt32(&reqN) != 0 {
		t.Fatal("invalid-id request invoked onRequest")
	}
	resp := s.readJSON(t)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32600 {
		t.Fatalf("want exactly one -32600, got %v", resp)
	}
	if resp["id"] != nil {
		t.Fatalf("id=%v, want null", resp["id"])
	}
}

func TestInvalidIDResponseLeavesPendingCallIntact(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	resc := make(chan json.RawMessage, 1)
	go func() {
		res, err := s.peer.Call(context.Background(), "ping", nil)
		resc <- res
		errc <- err
	}()
	req := s.readJSON(t)
	id := rawID(t, req)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":true,"result":{"stolen":true}}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":{},"result":{"stolen":true}}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":[],"error":{"code":-1,"message":"x"}}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{"ok":1}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(<-resc, []byte(`"ok"`)) {
		t.Fatal("valid result lost to invalid-id response")
	}
}

func TestJSONRPCVersionDoesNotReachCallbacks(t *testing.T) {
	s := newScriptedPeer(t)
	var notifyN, reqN int32
	s.peer.onNotify = func(method string, params json.RawMessage, captured string, skip bool) {
		atomic.AddInt32(&notifyN, 1)
	}
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		atomic.AddInt32(&reqN, 1)
		return map[string]any{}, nil
	}
	s.writeRaw(t, `{"jsonrpc":"1.0","method":"ghost"}`)
	s.writeRaw(t, `{"method":"ghost2"}`)
	s.writeRaw(t, `{"jsonrpc":"2.0","method":"ok"}`)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&notifyN) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&notifyN) != 1 {
		t.Fatalf("notify=%d, want only jsonrpc 2.0", notifyN)
	}
	if atomic.LoadInt32(&reqN) != 0 {
		t.Fatal("wrong version invoked onRequest")
	}
}

func TestClassifyRPCIDTable(t *testing.T) {
	cases := []struct {
		raw  string
		omit bool
		want idClass
	}{
		{omit: true, want: idAbsent},
		{raw: `null`, want: idNull},
		{raw: `"abc"`, want: idValidString},
		{raw: `"null"`, want: idValidString},
		{raw: `"1"`, want: idValidString},
		{raw: `"01"`, want: idValidString},
		{raw: `1`, want: idValidNumber},
		{raw: `-7`, want: idValidNumber},
		{raw: `1.5`, want: idValidNumber},
		{raw: `1e2`, want: idValidNumber},
		{raw: `true`, want: idInvalid},
		{raw: `false`, want: idInvalid},
		{raw: `{}`, want: idInvalid},
		{raw: `[]`, want: idInvalid},
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if !tc.omit {
			raw = json.RawMessage(tc.raw)
		}
		if got := classifyRPCID(raw); got != tc.want {
			t.Errorf("classifyRPCID(%s)=%v want %v", tc.raw, got, tc.want)
		}
		valid := validRPCID(raw)
		wantValid := tc.want == idValidString || tc.want == idValidNumber
		if valid != wantValid {
			t.Errorf("validRPCID(%s)=%v want %v", tc.raw, valid, wantValid)
		}
	}
	if classifyRPCID(json.RawMessage(`"null"`)) == idNull {
		t.Fatal("string null must not classify as JSON null")
	}
	if !validRPCID(json.RawMessage(`"null"`)) {
		t.Fatal("string null is a valid id")
	}
}

func TestJSONRPC10ValidIDRequestDoesNotStealWaiter(t *testing.T) {
	s := newScriptedPeer(t)
	var reqN int32
	s.peer.onRequest = func(method string, params json.RawMessage, captured string, skip bool) (any, error) {
		atomic.AddInt32(&reqN, 1)
		return map[string]any{"stolen": true}, nil
	}
	errc := make(chan error, 1)
	resc := make(chan json.RawMessage, 1)
	go func() {
		res, err := s.peer.Call(context.Background(), "ping", nil)
		resc <- res
		errc <- err
	}()
	req := s.readJSON(t)
	id := rawID(t, req)
	s.writeRaw(t, `{"jsonrpc":"1.0","id":`+id+`,"method":"approval/request","params":{}}`)
	inv, ok := s.tryReadJSON(t, 2*time.Second)
	if !ok {
		t.Fatal("want -32600 for jsonrpc 1.0 request with a valid id")
	}
	errObj, _ := inv["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32600 {
		t.Fatalf("want -32600, got %v", inv)
	}
	if inv["id"] != nil {
		t.Fatalf("invalid-request id=%v, want null", inv["id"])
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{"ok":true}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(<-resc, []byte(`"ok"`)) {
		t.Fatal("pending call was stolen by a jsonrpc 1.0 request with the same id")
	}
	if atomic.LoadInt32(&reqN) != 0 {
		t.Fatal("jsonrpc 1.0 request invoked onRequest")
	}
}

func TestWaitAppliedZeroSeqAndCloseAndCancel(t *testing.T) {
	s := newScriptedPeer(t)
	if err := s.peer.waitApplied(context.Background(), 0); err != nil {
		t.Fatalf("seq 0: %v", err)
	}
	errc0 := make(chan error, 1)
	go func() {
		_, err := s.peer.callOrdered(context.Background(), "ping", nil)
		errc0 <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	if err := <-errc0; err != nil {
		t.Fatal(err)
	}
	if err := s.peer.waitApplied(context.Background(), 1); err != nil {
		t.Fatalf("already applied: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.peer.waitApplied(ctx, 1<<20) }()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel waitApplied: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitApplied not released by cancel")
	}
	errc2 := make(chan error, 1)
	go func() { errc2 <- s.peer.waitApplied(context.Background(), 1<<20) }()
	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc2:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("close waitApplied: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitApplied not released by Close")
	}
	if err := s.peer.waitApplied(context.Background(), 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("waitApplied after close: %v", err)
	}
	s.peer.enqueueOrdered(orderedNotify, inbound{Method: "late"})
}

func TestClassifyRPCIDMalformedAndWhitespace(t *testing.T) {
	if classifyRPCID(json.RawMessage("  ")) != idAbsent {
		t.Fatal("whitespace-only id must be absent")
	}
	if classifyRPCID(json.RawMessage(`"`)) != idInvalid {
		t.Fatal("truncated string id must be invalid")
	}
	if classifyRPCID(json.RawMessage(`01`)) != idInvalid {
		t.Fatal("non-JSON number must be invalid")
	}
}

func TestCloseReleasesCallWaitingOnOrderedBarrier(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "turn/start", nil)
		errc <- err
	}()
	_ = s.readJSON(t)
	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call waiting on barrier not released")
	}
}

func TestCallApplyRunsBeforeLaterOnProtocol(t *testing.T) {
	s := newScriptedPeer(t)
	applied := make(chan struct{})
	started := make(chan struct{}, 1)
	var startedBeforeApply atomic.Bool
	s.peer.onProtocol = func(method string, params json.RawMessage) {
		if method != "turn/started" {
			return
		}
		select {
		case <-applied:
		default:
			startedBeforeApply.Store(true)
		}
		select {
		case started <- struct{}{}:
		default:
		}
	}
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", map[string]any{"k": 1}, func(json.RawMessage) error {
			close(applied)
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	id := rawID(t, req)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+id+`,"result":{"sessionId":"sess-2"}}`+"\n"+
		`{"jsonrpc":"2.0","method":"turn/started","params":{"sessionId":"sess-2","turnId":"t2"}}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("later onProtocol never ran")
	}
	if startedBeforeApply.Load() {
		t.Fatal("later onProtocol ran before apply committed")
	}
}

func TestCallApplyErrorDoesNotSucceed(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
			return errors.New("malformed start")
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"sessionId":"x"}}`)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "malformed start") {
		t.Fatalf("err=%v", err)
	}
}

func TestCallApplyPanicBecomesError(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
			panic("apply boom")
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "apply boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestCallApplyCloseDuringBlockedApply(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
			close(entered)
			<-release
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.peer.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked behind apply")
	}
	select {
	case err := <-errc:
		t.Fatalf("waiter returned %v while claimed apply still blocked", err)
	default:
	}
	close(release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("claimed apply must win after Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callApply waiter stuck after apply release")
	}
	if n := len(s.peer.pend); n != 0 {
		t.Fatalf("pending leak %d", n)
	}
}

func TestCallApplyCancelDuringBlockedApply(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(ctx, "session/start", nil, func(json.RawMessage) error {
			close(entered)
			<-release
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply not entered")
	}
	cancel()
	select {
	case err := <-errc:
		t.Fatalf("waiter returned %v while claimed apply still blocked", err)
	default:
	}
	close(release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("claimed apply must win after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callApply waiter stuck after apply release")
	}
	if n := len(s.peer.pend); n != 0 {
		t.Fatalf("pending leak %d", n)
	}
}

func TestCallApplyCancelBeforeResponseIgnoresLateResult(t *testing.T) {
	s := newScriptedPeer(t)
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(ctx, "session/start", nil, func(json.RawMessage) error {
			n.Add(1)
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not win before response")
	}
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"sessionId":"late"}}`)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n.Load() != 0 {
			t.Fatal("apply ran after cancel won")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n.Load() != 0 {
		t.Fatal("apply ran after cancel won")
	}
	s.peer.mu.Lock()
	left := len(s.peer.pend)
	s.peer.mu.Unlock()
	if left != 0 {
		t.Fatalf("pending leak %d", left)
	}
}

func TestCallApplyCloseBeforeResponseIgnoresLateResult(t *testing.T) {
	s := newScriptedPeer(t)
	var n atomic.Int32
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
			n.Add(1)
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not win before response")
	}
	_ = req
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n.Load() != 0 {
			t.Fatal("apply ran after Close won")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.peer.mu.Lock()
	left := len(s.peer.pend)
	s.peer.mu.Unlock()
	if left != 0 {
		t.Fatalf("pending leak %d", left)
	}
}

func TestCallApplyErrorWinsOverCancelOnceClaimed(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(ctx, "session/start", nil, func(json.RawMessage) error {
			close(entered)
			<-release
			return errors.New("apply failed")
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply not entered")
	}
	cancel()
	select {
	case err := <-errc:
		t.Fatalf("cancel stole claimed apply error: %v", err)
	default:
	}
	close(release)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestCallOrderedApplyWaitAppliedSeesClose(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.call(context.Background(), "session/start", nil, true, func(json.RawMessage) error {
			close(entered)
			<-release
			return nil
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply not entered")
	}
	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Close stole claimed ordered response: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ordered call stuck")
	}
}

func TestCallApplyPanicWinsOverCloseOnceClaimed(t *testing.T) {
	s := newScriptedPeer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
			close(entered)
			<-release
			panic("apply boom")
		})
		errc <- err
	}()
	req := s.readJSON(t)
	s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{}}`)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("apply not entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.peer.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked behind apply")
	}
	select {
	case err := <-errc:
		t.Fatalf("Close stole claimed apply panic: %v", err)
	default:
	}
	close(release)
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "apply boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveDoesNotClaimAfterClosed(t *testing.T) {
	s := newScriptedPeer(t)
	var applied atomic.Bool
	pd := &pendingCall{
		ch:     make(chan json.RawMessage, 1),
		er:     make(chan error, 1),
		method: "session/start",
		apply: func(json.RawMessage) error {
			applied.Store(true)
			return nil
		},
	}
	s.peer.mu.Lock()
	s.peer.pend["n:1"] = pd
	s.peer.closed = true
	s.peer.mu.Unlock()

	s.peer.resolve(inbound{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)})
	if applied.Load() {
		t.Fatal("resolve applied a pending call after the peer was closed")
	}
	s.peer.mu.Lock()
	still := s.peer.pend["n:1"] == pd
	s.peer.mu.Unlock()
	if !still {
		t.Fatal("resolve claimed a call Close already owned")
	}
	select {
	case <-pd.ch:
		t.Fatal("resolve completed a closed pending call")
	case <-pd.er:
		t.Fatal("resolve failed a closed pending call")
	default:
	}
}

func TestClosedPeerHasNoUnclaimedPending(t *testing.T) {
	s := newScriptedPeer(t)
	errc := make(chan error, 1)
	go func() {
		_, err := s.peer.Call(context.Background(), "x", nil)
		errc <- err
	}()
	_ = s.readJSON(t)

	s.peer.mu.Lock()
	if s.peer.closed {
		t.Fatal("closed before Close")
	}
	if len(s.peer.pend) != 1 {
		s.peer.mu.Unlock()
		t.Fatalf("pending=%d", len(s.peer.pend))
	}
	s.peer.mu.Unlock()

	if err := s.peer.Close(); err != nil {
		t.Fatal(err)
	}
	s.peer.mu.Lock()
	closed := s.peer.closed
	n := len(s.peer.pend)
	s.peer.mu.Unlock()
	if !closed {
		t.Fatal("Close did not set closed")
	}
	if n != 0 {
		t.Fatalf("unclaimed pending survived Close: %d", n)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(err.Error(), "x") {
			t.Fatalf("wrapped ErrClosed missing method: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter leaked")
	}
}

func TestCloseResolveOwnershipOrders(t *testing.T) {
	t.Run("response-first", func(t *testing.T) {
		s := newScriptedPeer(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
		var n atomic.Int32
		errc := make(chan error, 1)
		go func() {
			_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
				n.Add(1)
				close(entered)
				<-release
				return nil
			})
			errc <- err
		}()
		req := s.readJSON(t)
		s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"ok":1}}`)
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("apply not entered")
		}
		closed := make(chan error, 1)
		go func() { closed <- s.peer.Close() }()
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Close waited for claimed apply")
		}
		close(release)
		if err := <-errc; err != nil {
			t.Fatalf("response claim must win: %v", err)
		}
		if n.Load() != 1 {
			t.Fatalf("apply count=%d", n.Load())
		}
	})
	t.Run("close-first", func(t *testing.T) {
		s := newScriptedPeer(t)
		var n atomic.Int32
		errc := make(chan error, 1)
		go func() {
			_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
				n.Add(1)
				return nil
			})
			errc <- err
		}()
		req := s.readJSON(t)
		if err := s.peer.Close(); err != nil {
			t.Fatal(err)
		}
		err := <-errc
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err=%v", err)
		}
		s.peer.resolve(inbound{ID: json.RawMessage(rawID(t, req)), Result: json.RawMessage(`{"ok":1}`)})
		if n.Load() != 0 {
			t.Fatal("late response applied after Close claimed")
		}
	})
	t.Run("cancel-first", func(t *testing.T) {
		s := newScriptedPeer(t)
		var n atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := s.peer.callApply(ctx, "session/start", nil, func(json.RawMessage) error {
				n.Add(1)
				return nil
			})
			errc <- err
		}()
		req := s.readJSON(t)
		cancel()
		err := <-errc
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
		s.writeRaw(t, `{"jsonrpc":"2.0","id":`+rawID(t, req)+`,"result":{"ok":1}}`)
		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			if n.Load() != 0 {
				t.Fatal("late response applied after cancel claimed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func TestCloseResolveOwnershipStress(t *testing.T) {
	const rounds = 80
	for i := 0; i < rounds; i++ {
		s := newScriptedPeer(t)
		var applied atomic.Int32
		errc := make(chan error, 1)
		go func() {
			_, err := s.peer.callApply(context.Background(), "session/start", nil, func(json.RawMessage) error {
				applied.Add(1)
				return nil
			})
			errc <- err
		}()
		req := s.readJSON(t)
		id := rawID(t, req)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = io.WriteString(s.toPeer, `{"jsonrpc":"2.0","id":`+id+`,"result":{}}`+"\n")
		}()
		go func() {
			defer wg.Done()
			_ = s.peer.Close()
		}()
		err := <-errc
		wg.Wait()
		n := applied.Load()
		if n > 1 {
			t.Fatalf("round %d apply ran %d times", i, n)
		}
		if n == 1 && err != nil {
			t.Fatalf("round %d claimed apply lost: %v", i, err)
		}
		if n == 0 && !errors.Is(err, ErrClosed) {
			t.Fatalf("round %d no apply err=%v", i, err)
		}
		s.peer.mu.Lock()
		left := len(s.peer.pend)
		s.peer.mu.Unlock()
		if left != 0 {
			t.Fatalf("round %d pending leak %d", i, left)
		}
	}
}
