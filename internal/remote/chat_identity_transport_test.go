package remote

import (
	"context"
	"io"
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
