package remote

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/remote/codec"
)

// The production HTTP tunnel adapter must receive the query after the real
// codec has removed unlisted headers. The label is a precondition, not auth.
func TestChatIdentityQueryThroughProductionTunnel(t *testing.T) {
	left, right := net.Pipe()
	client := codec.NewConn(left, left, codec.RoleInitiator)
	server := codec.NewConn(right, right, codec.RoleResponder)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	handler := &httpTunnelHandler{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scimux-Chat-Instance") != "" {
			t.Error("unlisted header crossed codec")
		}
		if r.URL.Query().Get("chat_instance") != "synthetic-old" {
			t.Errorf("query=%q", r.URL.RawQuery)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"text":"synthetic"}` {
			t.Errorf("body=%q err=%v", body, err)
		}
		http.Error(w, "chat identity changed", http.StatusConflict)
	})}
	go func() { done <- server.Serve(ctx, handler) }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		left.Close()
		right.Close()
		<-done
	})
	resp, err := client.RoundTrip(ctx, &codec.Request{ID: "synthetic-instance", Method: "POST", Path: "/api/nodes/n1/send", Query: "chat_instance=synthetic-old", Headers: http.Header{"Content-Type": []string{"application/json"}, "X-Scimux-Chat-Instance": []string{"synthetic-old"}}, Body: io.NopCloser(strings.NewReader(`{"text":"synthetic"}`))})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.Status != http.StatusConflict {
		t.Fatalf("status=%d", resp.Status)
	}
}

// Multipart uploads and associated downloads use the production codec and
// HTTP adapter, including their closed header allowlists and query transport.
func TestPeopleFilesThroughProductionCodecAndTunnel(t *testing.T) {
	left, right := net.Pipe()
	client := codec.NewConn(left, left, codec.RoleInitiator)
	server := codec.NewConn(right, right, codec.RoleResponder)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	base := "/api/team/subscriptions/" + strings.Repeat("a", 32) + "/" + strings.Repeat("b", 32) + "/" + strings.Repeat("c", 16) + "/" + strings.Repeat("d", 16) + "/attachments"
	handler := &httpTunnelHandler{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scimux-Chat-Instance") != "" {
			t.Error("unlisted header crossed codec")
		}
		if r.Method == http.MethodPost {
			if r.URL.RawQuery != "stage_id=0123456789abcdef&chat_instance=synthetic" {
				t.Errorf("query=%q", r.URL.RawQuery)
			}
			file, _, err := r.FormFile("files")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			body, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || string(body) != "synthetic bytes" {
				t.Errorf("multipart mismatch: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"attachments":[{"id":"0123456789abcdef"}]}`)
			return
		}
		if r.URL.Path != base+"/1/0123456789abcdef" {
			t.Error("association path changed")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="synthetic.txt"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.WriteString(w, "synthetic bytes")
	})}
	go func() { done <- server.Serve(ctx, handler) }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		left.Close()
		right.Close()
		<-done
	})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("files", "synthetic.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, "synthetic bytes")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	requests := []*codec.Request{
		{ID: "synthetic-upload", Method: http.MethodPost, Path: base, Query: "stage_id=0123456789abcdef&chat_instance=synthetic", Headers: http.Header{"Content-Type": []string{form.FormDataContentType()}, "X-Scimux-Chat-Instance": []string{"synthetic"}}, Body: io.NopCloser(bytes.NewReader(body.Bytes()))},
		{ID: "synthetic-download", Method: http.MethodGet, Path: base + "/1/0123456789abcdef"},
	}
	for i, request := range requests {
		response, err := client.RoundTrip(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.Status != http.StatusOK {
			t.Fatalf("response %d: %v", response.Status, err)
		}
		if i == 1 && (string(data) != "synthetic bytes" || response.Headers.Get("X-Content-Type-Options") != "nosniff" || response.Headers.Get("Content-Disposition") == "") {
			t.Fatal("download body or safety headers changed")
		}
	}
}
