package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

// mockTransport is an in-process Transport backed by two pipes. The client
// writes requests to Stdin (which the mock server reads) and reads streamed
// frames from Stdout (which the mock server writes). No process is spawned, so
// the whole client read/turn/approval path is exercised without the codex CLI.
type mockTransport struct {
	clientW *io.PipeWriter // client's Stdin  (server reads reqR)
	reqR    *io.PipeReader
	respR   *io.PipeReader // client's Stdout (server writes respW)
	serverW *io.PipeWriter
	once    sync.Once
}

func newMockTransport() (*mockTransport, *mockServer) {
	reqR, clientW := io.Pipe()
	respR, serverW := io.Pipe()
	mt := &mockTransport{clientW: clientW, reqR: reqR, respR: respR, serverW: serverW}
	ms := &mockServer{
		in:   bufio.NewScanner(reqR),
		out:  serverW,
		reqs: make(chan clientFrame, 16),
		resp: make(chan clientFrame, 16),
	}
	ms.in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	go ms.readLoop()
	return mt, ms
}

func (m *mockTransport) Stdin() io.WriteCloser { return m.clientW }
func (m *mockTransport) Stdout() io.Reader     { return m.respR }
func (m *mockTransport) Close() error {
	m.once.Do(func() {
		_ = m.clientW.Close()
		_ = m.serverW.Close()
		_ = m.reqR.Close()
		_ = m.respR.Close()
	})
	return nil
}

// clientFrame is a decoded client->server frame.
type clientFrame struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  json.RawMessage  `json:"error"`
}

// mockServer speaks the server side over the pipes. It classifies incoming
// client frames into requests (method+id) and responses (id, no method — e.g.
// an approval decision) so scenario code can await each independently.
type mockServer struct {
	in   *bufio.Scanner
	out  io.Writer
	mu   sync.Mutex
	reqs chan clientFrame
	resp chan clientFrame
}

func (s *mockServer) readLoop() {
	for s.in.Scan() {
		line := s.in.Bytes()
		if len(line) == 0 {
			continue
		}
		var f clientFrame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.Method != "" {
			s.reqs <- f
		} else {
			s.resp <- f
		}
	}
	close(s.reqs)
	close(s.resp)
}

// nextReq blocks for the next client request (initialize/thread/start/turn/...).
func (s *mockServer) nextReq(t *testing.T) clientFrame {
	t.Helper()
	select {
	case f, ok := <-s.reqs:
		if !ok {
			t.Fatal("mock: client request stream closed unexpectedly")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("mock: timed out waiting for client request")
		return clientFrame{}
	}
}

// nextResp blocks for the next client response (e.g. an approval decision).
func (s *mockServer) nextResp(t *testing.T) clientFrame {
	t.Helper()
	select {
	case f, ok := <-s.resp:
		if !ok {
			t.Fatal("mock: client response stream closed unexpectedly")
		}
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("mock: timed out waiting for client response")
		return clientFrame{}
	}
}

// writeRaw sends one raw frame (already valid JSON) to the client.
func (s *mockServer) writeRaw(t *testing.T, raw string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := io.WriteString(s.out, raw+"\n"); err != nil {
		t.Fatalf("mock: write: %v", err)
	}
}

// reply sends a success response echoing the request's id.
func (s *mockServer) reply(t *testing.T, id *json.RawMessage, resultJSON string) {
	t.Helper()
	idb, _ := json.Marshal(id)
	s.writeRaw(t, `{"id":`+string(idb)+`,"result":`+resultJSON+`}`)
}

// note sends a notification.
func (s *mockServer) note(t *testing.T, method, paramsJSON string) {
	t.Helper()
	s.writeRaw(t, `{"method":"`+method+`","params":`+paramsJSON+`}`)
}

// serverRequest sends a server->client request with the given id.
func (s *mockServer) serverRequest(t *testing.T, id int, method, paramsJSON string) {
	t.Helper()
	s.writeRaw(t, `{"id":`+itoaTest(id)+`,"method":"`+method+`","params":`+paramsJSON+`}`)
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
