package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/scimux/scimux/internal/remote/codec"
)

// TestPeopleRealAppThroughProductionTunnel is opt-in from the isolated browser
// fixture. The loopback bridge supplies the fixture's local CSRF boundary;
// application and relay authorization and storage remain the real routes.
func TestPeopleRealAppThroughProductionTunnel(t *testing.T) {
	target := os.Getenv("SCIMUX_TUNNEL_APP_TARGET")
	if target == "" {
		t.Skip("requires the isolated browser fixture")
	}
	base, err := url.Parse(target)
	if err != nil || base.Scheme != "http" || base.Hostname() != "127.0.0.1" || base.Port() == "" || base.Path != "" {
		t.Fatal("invalid private target")
	}
	key := os.Getenv("SCIMUX_TUNNEL_APP_KEY")
	if !regexp.MustCompile(`^[0-9a-f]{32}/[0-9a-f]{32}/[0-9a-f]{16}/[0-9a-f]{16}$`).MatchString(key) {
		t.Fatal("invalid synthetic subscription")
	}
	copyInstance := os.Getenv("SCIMUX_TUNNEL_APP_COPY_INSTANCE")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(copyInstance) {
		t.Fatal("invalid synthetic copy instance")
	}
	token := os.Getenv("SCIMUX_TUNNEL_APP_CSRF")
	if token == "" {
		t.Fatal("missing synthetic CSRF")
	}
	left, right := net.Pipe()
	client := codec.NewConn(left, left, codec.RoleInitiator)
	server := codec.NewConn(right, right, codec.RoleResponder)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	adapter := &httpTunnelHandler{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, e := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), r.Body)
		if e != nil {
			http.Error(w, "bridge request", 500)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Set("Origin", target)
		req.Header.Set("X-Scimux-CSRF", token)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			http.Error(w, "bridge unavailable", 502)
			return
		}
		defer resp.Body.Close()
		for name, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})}
	go func() { done <- server.Serve(ctx, adapter) }()
	t.Cleanup(func() {
		cancel()
		client.Close()
		server.Close()
		left.Close()
		right.Close()
		<-done
	})
	path := "/api/team/subscriptions/" + key
	var formBody bytes.Buffer
	form := multipart.NewWriter(&formBody)
	part, err := form.CreateFormFile("files", "synthetic-tunnel.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("synthetic tunnel bytes"))
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	round := func(id, method, path, query string, headers http.Header, body []byte) (int, http.Header, []byte) {
		t.Helper()
		response, e := client.RoundTrip(ctx, &codec.Request{ID: id, Method: method, Path: path, Query: query, Headers: headers, Body: io.NopCloser(bytes.NewReader(body))})
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		return response.Status, response.Headers, raw
	}
	query := url.Values{"copy_instance": {copyInstance}}
	uploadQuery := url.Values{"copy_instance": {copyInstance}, "stage_id": {"0123456789abcdef"}}
	status, _, raw := round("upload", "POST", path+"/attachments", uploadQuery.Encode(), http.Header{"Content-Type": {form.FormDataContentType()}, "X-Scimux-Chat-Instance": {"not-transported"}}, formBody.Bytes())
	if os.Getenv("SCIMUX_TUNNEL_APP_READONLY") == "1" {
		if status != http.StatusForbidden {
			t.Fatalf("read-only upload status %d", status)
		}
		return
	}
	if status != http.StatusOK {
		t.Fatalf("real upload status=%d body=%s", status, raw)
	}
	var staged struct {
		Attachments []struct {
			ID string `json:"id"`
		} `json:"attachments"`
	}
	if json.Unmarshal(raw, &staged) != nil || len(staged.Attachments) != 1 || staged.Attachments[0].ID != "0123456789abcdef" {
		t.Fatal("staged identity mismatch")
	}
	body := []byte(`{"text":"","anchor":0,"key":"0123456789abcdef","attachments":["0123456789abcdef"]}`)
	status, _, raw = round("publish", "POST", path+"/comments", query.Encode(), http.Header{"Content-Type": {"application/json"}}, body)
	if status != http.StatusOK {
		t.Fatalf("real publication status=%d body=%s", status, raw)
	}
	var published struct {
		Seq uint64 `json:"seq"`
	}
	if json.Unmarshal(raw, &published) != nil || published.Seq == 0 {
		t.Fatal("missing publication sequence")
	}
	status, headers, raw := round("download", "GET", path+"/attachments/"+strconv.FormatUint(published.Seq, 10)+"/0123456789abcdef", query.Encode(), nil, nil)
	if status != http.StatusOK || string(raw) != "synthetic tunnel bytes" || headers.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment") {
		t.Fatalf("real download status=%d", status)
	}
}
