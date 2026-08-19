package codec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func wantReject(t *testing.T, err error, class Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want RejectError class %s", class)
	}
	var rej *RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v (%T); want RejectError class %s", err, err, class)
	}
	if rej.Class != class {
		t.Fatalf("reject class = %s, want %s (err=%v)", rej.Class, class, err)
	}
}

func wantRejectField(t *testing.T, err error, class Class, field string) {
	t.Helper()
	wantReject(t, err, class)
	var rej *RejectError
	errors.As(err, &rej)
	if rej.Field != field {
		t.Fatalf("reject field = %q, want %q (err=%v)", rej.Field, field, err)
	}
}

func bodyOf(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

func limitBody(n int64, b byte) io.ReadCloser {
	return io.NopCloser(io.LimitReader(foreverByte(b), n))
}

type foreverByte byte

func (b foreverByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func countBytes(r io.Reader) (int64, error) {
	return io.Copy(io.Discard, r)
}

// startPair crosses two io.Pipe pairs into a client Conn served by h.
func startPair(t *testing.T, h Handler) *Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	client := NewConn(cr, cw)
	server := NewConn(sr, sw)
	errc := make(chan error, 1)
	go func() {
		errc <- server.Serve(ctx, h)
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = server.Close()
		_ = cr.Close()
		_ = cw.Close()
		_ = sr.Close()
		_ = sw.Close()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return client
}

func headerAllowed(allow map[string]struct{}, name string) bool {
	_, ok := allow[http.CanonicalHeaderKey(name)]
	return ok
}

func snapshotGoroutines() map[int]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := make(map[int]string)
	blocks := strings.Split(string(buf), "\n\n")
	for _, block := range blocks {
		line, _, _ := strings.Cut(block, "\n")
		// "goroutine 123 [running]:"
		if !strings.HasPrefix(line, "goroutine ") {
			continue
		}
		rest := strings.TrimPrefix(line, "goroutine ")
		idStr, _, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		out[id] = block
	}
	return out
}

func assertNoGoroutineLeak(t *testing.T, before map[int]string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var extra []string
	for {
		now := snapshotGoroutines()
		extra = extra[:0]
		for id, stack := range now {
			if _, ok := before[id]; ok {
				continue
			}
			extra = append(extra, stack)
		}
		if len(extra) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leaked %d goroutine(s):\n%s", len(extra), strings.Join(extra, "\n---\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stallReader yields first, then blocks until unblock is closed, then EOF.
type stallReader struct {
	first   []byte
	sent    int
	unblock <-chan struct{}
}

func (s *stallReader) Read(p []byte) (int, error) {
	if s.sent < len(s.first) {
		n := copy(p, s.first[s.sent:])
		s.sent += n
		return n, nil
	}
	<-s.unblock
	return 0, io.EOF
}

func (s *stallReader) Close() error { return nil }

type heapWatch struct {
	peak     atomic.Uint64
	baseline uint64
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func startHeapWatch(t *testing.T) *heapWatch {
	t.Helper()
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	w := &heapWatch{
		baseline: ms.HeapAlloc,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	w.peak.Store(ms.HeapAlloc)
	go func() {
		defer close(w.done)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-tick.C:
				var now runtime.MemStats
				runtime.ReadMemStats(&now)
				for {
					old := w.peak.Load()
					if now.HeapAlloc <= old || w.peak.CompareAndSwap(old, now.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	t.Cleanup(func() { w.stopWatch() })
	return w
}

func (w *heapWatch) stopWatch() {
	w.once.Do(func() { close(w.stop) })
	<-w.done
}

func (w *heapWatch) Delta() int64 {
	w.stopWatch()
	return int64(w.peak.Load()) - int64(w.baseline)
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	return h.Clone()
}

// collectRequest is a handler that stores the inbound request metadata
// (headers, method, path, query, id) and a prefix of the body, then
// returns a caller-supplied response.
type collectedReq struct {
	ID      string
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte
}

func collectingHandler(out chan<- collectedReq, resp *Response) HandlerFunc {
	return func(ctx context.Context, req *Request) (*Response, error) {
		got := collectedReq{
			ID:      req.ID,
			Method:  req.Method,
			Path:    req.Path,
			Query:   req.Query,
			Headers: cloneHeader(req.Headers),
		}
		if req.Body != nil {
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, req.Body)
			got.Body = buf.Bytes()
			_ = req.Body.Close()
		}
		out <- got
		outResp := *resp
		outResp.ID = req.ID
		if outResp.Body == nil {
			outResp.Body = io.NopCloser(bytes.NewReader(nil))
		}
		return &outResp, nil
	}
}

func mustRoundTrip(t *testing.T, client *Conn, req *Request) *Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	resp, err := client.RoundTrip(ctx, req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp == nil {
		t.Fatal("RoundTrip: nil response")
	}
	return resp
}
