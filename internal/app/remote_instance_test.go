package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote/codec"
)

// Exercise the real codec and router with the custom header deliberately lost.
func TestRemoteCodecChatIdentityFence(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1", InstanceID: "replacement"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nodes/{id}/send", a.handleSend)
	mux.HandleFunc("POST /api/nodes/{id}/send/interrupt", a.handleSendInterrupt)
	mux.HandleFunc("POST /api/nodes/{id}/attachments", a.handleUploadAttachments)
	mux.HandleFunc("PATCH /api/nodes/{id}", a.handleUpdateNode)
	mux.HandleFunc("DELETE /api/nodes/{id}", a.handleDeleteNode)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	client := codec.NewConn(cr, cw, codec.RoleInitiator)
	server := codec.NewConn(sr, sw, codec.RoleResponder)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, codec.HandlerFunc(func(ctx context.Context, req *codec.Request) (*codec.Response, error) {
			if req.Headers.Get("X-Scimux-Chat-Instance") != "" {
				t.Error("unlisted header crossed codec")
			}
			request := httptest.NewRequest(req.Method, req.Path+"?"+req.Query, req.Body).WithContext(ctx)
			request.Header = req.Headers
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			r := recorder.Result()
			return &codec.Response{ID: req.ID, Status: r.StatusCode, Headers: r.Header, Body: r.Body}, nil
		}))
	}()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		cr.Close()
		cw.Close()
		sr.Close()
		sw.Close()
		<-done
	})
	var upload bytes.Buffer
	mw := multipart.NewWriter(&upload)
	f, _ := mw.CreateFormFile("files", "synthetic.txt")
	f.Write([]byte("synthetic"))
	mw.Close()
	cases := []struct{ path, method, body, content string }{
		{"/api/nodes/n1/send", "POST", `{"text":"synthetic"}`, "application/json"},
		{"/api/nodes/n1/send/interrupt", "POST", "", ""},
		{"/api/nodes/n1/attachments", "POST", upload.String(), mw.FormDataContentType()},
		{"/api/nodes/n1", "PATCH", `{"description":"synthetic"}`, "application/json"},
		{"/api/nodes/n1", "DELETE", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			headers := http.Header{"X-Scimux-Chat-Instance": []string{"old"}}
			if tc.content != "" {
				headers.Set("Content-Type", tc.content)
			}
			resp, err := client.RoundTrip(t.Context(), &codec.Request{ID: tc.method + tc.path, Method: tc.method, Path: tc.path, Query: "chat_instance=old", Headers: headers, Body: io.NopCloser(strings.NewReader(tc.body))})
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.Status != http.StatusConflict {
				t.Fatalf("status %d, want 409", resp.Status)
			}
		})
	}
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}
	call := func(id, path, content, body string) *codec.Response {
		resp, err := client.RoundTrip(t.Context(), &codec.Request{ID: id, Method: "POST", Path: path, Query: "chat_instance=replacement", Headers: http.Header{"Content-Type": []string{content}}, Body: io.NopCloser(strings.NewReader(body))})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	up := call("valid-upload", "/api/nodes/n1/attachments", mw.FormDataContentType(), upload.String())
	var uploaded struct {
		Attachments []Attachment `json:"attachments"`
	}
	if up.Status != 200 {
		t.Fatalf("current upload status %d", up.Status)
	}
	if err := json.NewDecoder(up.Body).Decode(&uploaded); err != nil {
		t.Fatal(err)
	}
	up.Body.Close()
	body, err := json.Marshal(map[string]any{"text": "synthetic", "attachments": uploaded.Attachments})
	if err != nil {
		t.Fatal(err)
	}
	send := call("valid-send", "/api/nodes/n1/send", "application/json", string(body))
	io.Copy(io.Discard, send.Body)
	send.Body.Close()
	if send.Status != 200 {
		t.Fatalf("current send status %d", send.Status)
	}
	gate := a.chatGate("n1")
	gate.Lock()
	interrupts := make(chan *codec.Response, 1)
	go func() { interrupts <- call("valid-interrupt", "/api/nodes/n1/send/interrupt", "", "") }()
	var interrupt *codec.Response
	select {
	case interrupt = <-interrupts:
		gate.Unlock()
	case <-time.After(5 * time.Second):
		gate.Unlock()
		t.Fatal("remote interrupt blocked behind send gate")
	}
	io.Copy(io.Discard, interrupt.Body)
	interrupt.Body.Close()
	if interrupt.Status != 200 {
		t.Fatalf("current interrupt status %d", interrupt.Status)
	}
}

func TestRemoteCodecHeldUploadRechecksChatIdentity(t *testing.T) {
	a := newTestApp(t, &fakeTmux{})
	a.byID["n1"] = &Node{ID: "n1", InstanceID: "old"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/nodes/{id}/attachments", a.handleUploadAttachments)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	client := codec.NewConn(cr, cw, codec.RoleInitiator)
	server := codec.NewConn(sr, sw, codec.RoleResponder)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- server.Serve(ctx, codec.HandlerFunc(func(ctx context.Context, req *codec.Request) (*codec.Response, error) {
			request := httptest.NewRequest(req.Method, req.Path+"?"+req.Query, &signalingReadCloser{ReadCloser: req.Body, started: started}).WithContext(ctx)
			request.Header = req.Headers
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			r := recorder.Result()
			return &codec.Response{ID: req.ID, Status: r.StatusCode, Headers: r.Header, Body: r.Body}, nil
		}))
	}()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		cr.Close()
		cw.Close()
		sr.Close()
		sw.Close()
		<-done
	})
	var data bytes.Buffer
	mw := multipart.NewWriter(&data)
	f, _ := mw.CreateFormFile("files", "synthetic.txt")
	f.Write([]byte("synthetic"))
	mw.Close()
	rd, wr := io.Pipe()
	result := make(chan *codec.Response, 1)
	errors := make(chan error, 1)
	go func() {
		resp, err := client.RoundTrip(ctx, &codec.Request{ID: "held-upload", Method: "POST", Path: "/api/nodes/n1/attachments", Query: "chat_instance=old", Headers: http.Header{"Content-Type": []string{mw.FormDataContentType()}}, Body: rd})
		if err != nil {
			errors <- err
			return
		}
		result <- resp
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not reach server")
	}
	a.mu.Lock()
	a.byID["n1"] = &Node{ID: "n1", InstanceID: "replacement"}
	a.mu.Unlock()
	if _, err := wr.Write(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	wr.Close()
	select {
	case err := <-errors:
		t.Fatal(err)
	case resp := <-result:
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.Status != http.StatusConflict {
			t.Fatalf("status %d, want 409", resp.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not finish")
	}
}
