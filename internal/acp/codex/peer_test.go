package codex

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopWriteCloser adapts an io.Writer to io.WriteCloser for peer tests.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// bufWriter is a concurrency-safe in-memory writer capturing outbound frames.
type bufWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *bufWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *bufWriter) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimRight(b.buf.String(), "\n"), "\n")
}

func TestPeerCallResponse(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	// Server side: read the request id then answer it.
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()

	var got json.RawMessage
	var callErr error
	done := make(chan struct{})
	go func() {
		got, callErr = p.call("initialize", map[string]any{"clientInfo": "x"})
		close(done)
	}()

	// The peer wrote the request to out; reply on reqW with id 1.
	waitFor(t, func() bool { return len(out.lines()) >= 1 && out.lines()[0] != "" })
	reqW.Write([]byte(`{"id":1,"result":{"ok":true}}` + "\n"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("call did not return")
	}
	if callErr != nil {
		t.Fatalf("call error: %v", callErr)
	}
	if !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("unexpected result: %s", got)
	}
}

func TestPeerCallError(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()

	res := make(chan error, 1)
	go func() { _, err := p.call("thread/start", nil); res <- err }()
	waitFor(t, func() bool { return len(out.lines()) >= 1 && out.lines()[0] != "" })
	reqW.Write([]byte(`{"id":1,"error":{"code":-32600,"message":"bad thread"}}` + "\n"))

	err := <-res
	if err == nil || !strings.Contains(err.Error(), "bad thread") {
		t.Fatalf("want rpc error, got %v", err)
	}
	var re *rpcError
	if !as(err, &re) || re.Code != -32600 {
		t.Fatalf("error should be *rpcError with code -32600, got %v", err)
	}
}

func TestPeerNotificationDispatch(t *testing.T) {
	p := newPeer(nopWriteCloser{io.Discard}, nil)
	var mu sync.Mutex
	seen := map[string]json.RawMessage{}
	p.onNotify = func(method string, params json.RawMessage) {
		mu.Lock()
		seen[method] = params
		mu.Unlock()
	}
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte(`{"method":"turn/started","params":{"x":1}}` + "\n"))
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return seen["turn/started"] != nil })
}

func TestPeerServerRequestAnswered(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	p.onRequest = func(method string, params json.RawMessage) (any, *rpcError) {
		if method == "currentTime/read" {
			return map[string]string{"currentTime": "now"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "no"}
	}
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte(`{"id":0,"method":"currentTime/read","params":{}}` + "\n"))

	waitFor(t, func() bool {
		for _, l := range out.lines() {
			if strings.Contains(l, `"currentTime":"now"`) && strings.Contains(l, `"id":0`) {
				return true
			}
		}
		return false
	})
}

func TestPeerServerRequestRejected(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	p.onRequest = func(method string, params json.RawMessage) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: "denied"}
	}
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte(`{"id":7,"method":"item/permissions/requestApproval","params":{}}` + "\n"))
	waitFor(t, func() bool {
		for _, l := range out.lines() {
			if strings.Contains(l, `"error"`) && strings.Contains(l, "denied") {
				return true
			}
		}
		return false
	})
}

func TestPeerConcurrentCalls(t *testing.T) {
	out := &bufWriter{}
	p := newPeer(out, nil)
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()

	const n = 5
	results := make([]json.RawMessage, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.call("m", map[string]int{"i": i})
		}(i)
	}
	// Answer each id out of order.
	waitFor(t, func() bool { return len(out.lines()) >= n && out.lines()[n-1] != "" })
	for _, id := range []int{3, 1, 5, 2, 4} {
		reqW.Write([]byte(`{"id":` + itoaTest(id) + `,"result":{"id":` + itoaTest(id) + `}}` + "\n"))
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
	}
}

func TestPeerCloseReleasesPending(t *testing.T) {
	p := newPeer(nopWriteCloser{io.Discard}, nil)
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	res := make(chan error, 1)
	go func() { _, err := p.call("m", nil); res <- err }()
	// Close the server side; the pending call must be released, not hang.
	time.Sleep(20 * time.Millisecond)
	reqW.Close()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("expected error after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call was not released on close")
	}
	// A subsequent call must fail fast.
	if _, err := p.call("m", nil); err == nil {
		t.Fatal("call after close should fail")
	}
}

func TestPeerIgnoresGarbageLine(t *testing.T) {
	var traced []string
	var mu sync.Mutex
	p := newPeer(nopWriteCloser{io.Discard}, func(dir, method string, raw []byte) {
		mu.Lock()
		traced = append(traced, dir+":"+method)
		mu.Unlock()
	})
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte("not json\n"))
	reqW.Write([]byte(`{"method":"ok","params":{}}` + "\n"))
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		var sawErr, sawNote bool
		for _, e := range traced {
			if e == "err:decode" {
				sawErr = true
			}
			if e == "in-note:ok" {
				sawNote = true
			}
		}
		return sawErr && sawNote
	})
}

func TestPeerStringID(t *testing.T) {
	// A string id with no pending call must be silently dropped (nothing is
	// registered under "abc") and the reader must keep going to deliver the
	// following notification.
	p := newPeer(nopWriteCloser{io.Discard}, nil)
	got := make(chan struct{}, 1)
	p.onNotify = func(string, json.RawMessage) {
		select {
		case got <- struct{}{}:
		default:
		}
	}
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()
	reqW.Write([]byte(`{"id":"abc","result":{}}` + "\n"))
	reqW.Write([]byte(`{"method":"n","params":{}}` + "\n"))
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("string-id frame wedged the reader")
	}
}

func TestPeerStringIDDelivered(t *testing.T) {
	// A server that echoes a numeric id as a quoted string ("1" instead of 1)
	// must still resolve the pending call, not drop the response and hang.
	out := &bufWriter{}
	p := newPeer(out, nil)
	reqR, reqW := io.Pipe()
	go func() { _ = p.readLoop(reqR) }()

	var got json.RawMessage
	var callErr error
	done := make(chan struct{})
	go func() {
		got, callErr = p.call("initialize", map[string]any{"clientInfo": "x"})
		close(done)
	}()

	// Wait for the outbound request, then reply with the id as a quoted string.
	waitFor(t, func() bool { return len(out.lines()) >= 1 && out.lines()[0] != "" })
	reqW.Write([]byte(`{"id":"1","result":{"stringIdOk":true}}` + "\n"))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("string-id response did not resolve the pending call")
	}
	if callErr != nil {
		t.Fatalf("call error: %v", callErr)
	}
	if !strings.Contains(string(got), `"stringIdOk":true`) {
		t.Fatalf("unexpected result: %s", got)
	}
}

// --- helpers ---

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// as is a tiny errors.As shim to avoid importing errors in every test.
func as(err error, target **rpcError) bool {
	if re, ok := err.(*rpcError); ok {
		*target = re
		return true
	}
	return false
}
