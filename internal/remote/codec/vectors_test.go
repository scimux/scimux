package codec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestCodecVectors dumps testdata/vectors.json from the production
// encoders (encodeRequestPayload, encodeResponsePayload, encodeRejectPayload,
// encodeID, encodeHeaders via those, writeFrame, writeOutgoingBody) and
// checks the committed file matches a fresh dump. The browser tests treat
// this file as the oracle; a vector derived by reading the format is not one.
func TestCodecVectors(t *testing.T) {
	got, err := json.MarshalIndent(buildVectorFile(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "vectors.json")
	want, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if merr := os.MkdirAll(filepath.Dir(path), 0o755); merr != nil {
				t.Fatalf("mkdir testdata: %v", merr)
			}
			if werr := os.WriteFile(path, got, 0o644); werr != nil {
				t.Fatalf("write %s: %v", path, werr)
			}
			t.Fatalf("wrote %s from production encoders; rerun", path)
		}
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s does not match a dump from the production encoders", path)
	}
}

type vectorFile struct {
	MaxFramePayload int      `json:"max_frame_payload"`
	BodyChunkSize   int      `json:"body_chunk_size"`
	FrameHeaderSize int      `json:"frame_header_size"`
	Counts          counts   `json:"counts"`
	Vectors         []vector `json:"vectors"`
}

type counts struct {
	Vectors  int `json:"vectors"`
	Request  int `json:"request"`
	Response int `json:"response"`
	Body     int `json:"body"`
	End      int `json:"end"`
	Reject   int `json:"reject"`
}

type vector struct {
	Name    string         `json:"name"`
	Request *vectorRequest `json:"request,omitempty"`
	Bytes   string         `json:"bytes"`
	Events  []vectorEvent  `json:"events"`
}

type vectorRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
	BodyB64 string            `json:"body_b64,omitempty"`
}

type vectorEvent struct {
	Type     string            `json:"type"`
	ID       string            `json:"id"`
	Status   int               `json:"status,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	ChunkB64 string            `json:"chunk_b64,omitempty"`
	Class    string            `json:"class,omitempty"`
	Message  string            `json:"message,omitempty"`
}

func buildVectorFile(t *testing.T) vectorFile {
	t.Helper()
	chunked := bytes.Repeat([]byte{'a'}, bodyChunkSize+1)
	vectors := []vector{
		requestVector(t, "get-empty", vectorRequest{
			ID: "1", Method: http.MethodGet, Path: "/api/state", Query: "",
			Headers: map[string]string{},
		}, nil),
		requestVector(t, "get-query", vectorRequest{
			ID: "q", Method: http.MethodGet, Path: "/api/state", Query: "q=hello",
			Headers: map[string]string{"Content-Type": "application/json"},
		}, nil),
		requestVector(t, "post-small", vectorRequest{
			ID: "p", Method: http.MethodPost, Path: "/api/nodes/n1/send", Query: "",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"text":"hi"}`,
		}, []byte(`{"text":"hi"}`)),
		requestVector(t, "post-chunked", vectorRequest{
			ID: "c", Method: http.MethodPost, Path: "/api/ui", Query: "",
			Headers: map[string]string{"Content-Type": "application/json"},
			BodyB64: base64.StdEncoding.EncodeToString(chunked),
		}, chunked),
		framedVector(t, "response-200", func(c *Conn) {
			writeResponse(t, c, "r200", http.StatusOK, http.Header{
				"Content-Type": {"application/json"},
			}, []byte(`{"ok":true}`))
		}),
		framedVector(t, "response-204", func(c *Conn) {
			writeResponse(t, c, "r204", http.StatusNoContent, nil, nil)
		}),
		framedVector(t, "response-206", func(c *Conn) {
			writeResponse(t, c, "r206", http.StatusPartialContent, http.Header{
				"Content-Range": {"bytes 0-3/8"},
			}, []byte("abcd"))
		}),
		framedVector(t, "reject-field", func(c *Conn) {
			writeRejectFrame(t, c, "rj1", ClassMalformed, "path")
		}),
		framedVector(t, "reject-class", func(c *Conn) {
			writeRejectFrame(t, c, "rj2", ClassTruncated, "")
		}),
		framedVector(t, "body-only", func(c *Conn) {
			writeBodyFrame(t, c, "b", []byte("xyz"))
		}),
		framedVector(t, "end-only", func(c *Conn) {
			writeEndFrame(t, c, "b")
		}),
	}
	if len(vectors) == 0 {
		t.Fatal("anti-vacuity: no vectors produced")
	}
	var ct counts
	ct.Vectors = len(vectors)
	nReq := 0
	for _, v := range vectors {
		if v.Request != nil {
			nReq++
		}
		for _, e := range v.Events {
			switch e.Type {
			case "response":
				ct.Response++
			case "body":
				ct.Body++
			case "end":
				ct.End++
			case "reject":
				ct.Reject++
			}
		}
	}
	if nReq == 0 || ct.Response == 0 || ct.Body == 0 || ct.End == 0 || ct.Reject == 0 {
		t.Fatalf("anti-vacuity: encode=%d events response=%d body=%d end=%d reject=%d",
			nReq, ct.Response, ct.Body, ct.End, ct.Reject)
	}
	ct.Request = nReq
	return vectorFile{
		MaxFramePayload: maxFramePayload,
		BodyChunkSize:   bodyChunkSize,
		FrameHeaderSize: frameHeaderSize,
		Counts:          ct,
		Vectors:         vectors,
	}
}

func requestVector(t *testing.T, name string, req vectorRequest, body []byte) vector {
	t.Helper()
	var buf bytes.Buffer
	c := NewConn(nil, &buf)
	hdrs := http.Header{}
	for k, v := range req.Headers {
		hdrs[k] = []string{v}
	}
	payload, err := encodeRequestPayload(req.ID, req.Method, req.Path, req.Query, hdrs)
	if err != nil {
		t.Fatalf("%s: encodeRequestPayload: %v", name, err)
	}
	if err := c.writeFrame(typeRequest, payload); err != nil {
		t.Fatalf("%s: writeFrame request: %v", name, err)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	if err := c.writeOutgoingBody(context.Background(), req.ID, r, 1<<30); err != nil {
		t.Fatalf("%s: writeOutgoingBody: %v", name, err)
	}
	raw := buf.Bytes()
	return vector{
		Name:    name,
		Request: &req,
		Bytes:   base64.StdEncoding.EncodeToString(raw),
		Events:  decodeEvents(t, name, raw),
	}
}

func framedVector(t *testing.T, name string, write func(*Conn)) vector {
	t.Helper()
	var buf bytes.Buffer
	c := NewConn(nil, &buf)
	write(c)
	raw := buf.Bytes()
	if len(raw) == 0 {
		t.Fatalf("%s: production encoder wrote nothing", name)
	}
	return vector{
		Name:   name,
		Bytes:  base64.StdEncoding.EncodeToString(raw),
		Events: decodeEvents(t, name, raw),
	}
}

func writeResponse(t *testing.T, c *Conn, id string, status int, h http.Header, body []byte) {
	t.Helper()
	payload, err := encodeResponsePayload(id, status, h)
	if err != nil {
		t.Fatalf("encodeResponsePayload: %v", err)
	}
	if err := c.writeFrame(typeResponse, payload); err != nil {
		t.Fatalf("writeFrame response: %v", err)
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	if err := c.writeOutgoingBody(context.Background(), id, r, 1<<30); err != nil {
		t.Fatalf("writeOutgoingBody: %v", err)
	}
}

func writeRejectFrame(t *testing.T, c *Conn, id string, class Class, field string) {
	t.Helper()
	payload, err := encodeRejectPayload(id, class, field)
	if err != nil {
		t.Fatalf("encodeRejectPayload: %v", err)
	}
	if err := c.writeFrame(typeReject, payload); err != nil {
		t.Fatalf("writeFrame reject: %v", err)
	}
}

func writeBodyFrame(t *testing.T, c *Conn, id string, data []byte) {
	t.Helper()
	chunk, err := encodeID(id)
	if err != nil {
		t.Fatalf("encodeID: %v", err)
	}
	chunk = append(chunk, data...)
	if err := c.writeFrame(typeBody, chunk); err != nil {
		t.Fatalf("writeFrame body: %v", err)
	}
}

func writeEndFrame(t *testing.T, c *Conn, id string) {
	t.Helper()
	idp, err := encodeID(id)
	if err != nil {
		t.Fatalf("encodeID: %v", err)
	}
	if err := c.writeFrame(typeBodyEnd, idp); err != nil {
		t.Fatalf("writeFrame body-end: %v", err)
	}
}

func decodeEvents(t *testing.T, name string, raw []byte) []vectorEvent {
	t.Helper()
	r := bytes.NewReader(raw)
	events := make([]vectorEvent, 0)
	for r.Len() > 0 {
		fr, err := decodeFrame(r)
		if err != nil {
			t.Fatalf("%s: decodeFrame: %v", name, err)
		}
		switch fr.typ {
		case typeRequest:
			continue
		case typeResponse:
			id, status, h, err := decodeResponsePayload(fr.payload)
			if err != nil {
				t.Fatalf("%s: decodeResponsePayload: %v", name, err)
			}
			events = append(events, vectorEvent{
				Type:    "response",
				ID:      id,
				Status:  status,
				Headers: headerMap(h),
			})
		case typeBody:
			id, data, err := decodeIDPayload(fr.payload)
			if err != nil {
				t.Fatalf("%s: decodeIDPayload: %v", name, err)
			}
			events = append(events, vectorEvent{
				Type:     "body",
				ID:       id,
				ChunkB64: base64.StdEncoding.EncodeToString(data),
			})
		case typeBodyEnd:
			id, _, err := decodeIDPayload(fr.payload)
			if err != nil {
				t.Fatalf("%s: decodeIDPayload: %v", name, err)
			}
			events = append(events, vectorEvent{Type: "end", ID: id})
		case typeReject:
			id, class, field, err := decodeRejectPayload(fr.payload)
			if err != nil {
				t.Fatalf("%s: decodeRejectPayload: %v", name, err)
			}
			events = append(events, vectorEvent{
				Type:    "reject",
				ID:      id,
				Class:   string(class),
				Message: (&RejectError{Class: class, Field: field}).Error(),
			})
		case typeCancel:
			continue
		default:
			t.Fatalf("%s: unexpected frame type %d", name, fr.typ)
		}
	}
	return events
}

func headerMap(h http.Header) map[string]string {
	out := map[string]string{}
	if h == nil {
		return out
	}
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}
