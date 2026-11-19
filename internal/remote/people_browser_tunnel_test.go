package remote

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scimux/scimux/internal/remote/codec"
)

// This opt-in browser integration uses synthetic records and a private HTTP
// bridge into the real codec and production HTTP tunnel adapter. It does not
// launch an agent or depend on a user profile.
func TestPeopleDelayedBrowserDownloadThroughProductionTunnel(t *testing.T) {
	playwright := os.Getenv("SCIMUX_E2E_PLAYWRIGHT")
	if playwright == "" {
		t.Skip("set SCIMUX_E2E_PLAYWRIGHT for the browser tunnel integration")
	}
	artifacts := os.Getenv("SCIMUX_TUNNEL_ARTIFACTS")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	if err := os.MkdirAll(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"desktop", "phone"} {
		t.Run(mode, func(t *testing.T) {
			left, right := net.Pipe()
			client := codec.NewConn(left, left, codec.RoleInitiator)
			server := codec.NewConn(right, right, codec.RoleResponder)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			root := "/api/team/subscriptions"
			serverID := strings.Repeat("a", 32)
			owner := strings.Repeat("b", 32)
			chat := strings.Repeat("c", 16)
			generation := strings.Repeat("d", 16)
			copyInstance := strings.Repeat("f", 32)
			id := "0123456789abcdef"
			base := root + "/" + serverID + "/" + owner + "/" + chat + "/" + generation
			filePath := base + "/attachments/1/" + id
			file := filePath + "?copy_instance=" + copyInstance
			var held atomic.Bool
			var dispatches atomic.Int32
			release := make(chan struct{})
			var releaseOnce sync.Once
			backend := &httpTunnelHandler{h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/send") || strings.Contains(r.URL.Path, "/interrupt") {
					dispatches.Add(1)
					http.Error(w, "not allowed", 403)
					return
				}
				switch r.URL.Path {
				case root:
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"people_attachments":{"version":1,"max_file_bytes":4194304,"max_files":16,"max_total_bytes":16777216},"servers":[{"id":"%s","people_files":"supported","people_file_binding":"%s"}],"subscriptions":[{"server":"%s","owner":"%s","chat":"%s","generation":"%s","people_copy_instance":"%s"}]}`, serverID, strings.Repeat("e", 64), serverID, owner, chat, generation, copyInstance)
				case base + "/comments":
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"entries":[{"seq":1,"attachments":[{"id":"%s","size":1,"mime":"text/plain"}]}]}`, id)
				case filePath:
					if r.URL.Query().Get("copy_instance") != copyInstance {
						http.Error(w, "wrong copy", http.StatusConflict)
						return
					}
					held.Store(true)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("Content-Disposition", `attachment; filename="synthetic.txt"`)
					w.Header().Set("X-Content-Type-Options", "nosniff")
					_, _ = io.WriteString(w, "x")
				default:
					http.NotFound(w, r)
				}
			})}
			go func() { done <- server.Serve(ctx, backend) }()
			var requestID atomic.Uint64
			mux := http.NewServeMux()
			mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
				target, err := url.ParseRequestURI(r.URL.Query().Get("path"))
				if err != nil || !strings.HasPrefix(target.Path, root) {
					http.Error(w, "bad target", 400)
					return
				}
				response, err := client.RoundTrip(r.Context(), &codec.Request{ID: fmt.Sprintf("synthetic-%d", requestID.Add(1)), Method: http.MethodGet, Path: target.Path, Query: target.RawQuery})
				if err != nil {
					http.Error(w, "tunnel unavailable", 502)
					return
				}
				defer response.Body.Close()
				for key, values := range response.Headers {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(response.Status)
				_, _ = io.Copy(w, response.Body)
			})
			mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
				if held.Load() {
					_, _ = io.WriteString(w, "held")
				} else {
					_, _ = io.WriteString(w, "waiting")
				}
			})
			mux.HandleFunc("/release", func(w http.ResponseWriter, _ *http.Request) {
				releaseOnce.Do(func() { close(release) })
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("/dispatch-count", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, dispatches.Load()) })
			mux.HandleFunc("/bootstrap.js", func(w http.ResponseWriter, r *http.Request) {
				http.ServeFile(w, r, filepath.Join("..", "..", "web", "js", "bootstrap.js"))
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = fmt.Fprintf(w, `<!doctype html><button id="load">Load message</button><button id="poll">Poll capabilities</button><button id="release">Release response</button><div id="state"></div><script type="module">
import {createRemoteAssetTransport} from '/bootstrap.js';
const base=%q, root=%q, file=%q;
const channel=path=>fetch('/rpc?path='+encodeURIComponent(path));
const transport=createRemoteAssetTransport({channel,assets:new Map(),createObjectURL:blob=>URL.createObjectURL(blob)});
await (await transport.fetchImpl(root)).json();document.querySelector('#state').innerHTML='<span id="ready">ready</span>';
document.querySelector('#load').onclick=async()=>{await (await transport.fetchImpl(base+'/comments?copy_instance='+%q)).json();const link=document.createElement('a');link.id='file';link.download='synthetic.txt';link.textContent='Download retained file';link.href=transport.assetURL(file);document.body.append(link)};
document.querySelector('#poll').onclick=async()=>{await (await transport.fetchImpl(root)).json();const done=document.createElement('span');done.id='polled';done.textContent='polled';document.body.append(done)};
document.querySelector('#release').onclick=()=>fetch('/release');</script>`, base, root, file, copyInstance)
			})
			web := httptest.NewServer(mux)
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				web.Close()
				cancel()
				client.Close()
				server.Close()
				left.Close()
				right.Close()
				<-done
			})
			limit, cancelRun := context.WithTimeout(ctx, 45*time.Second)
			defer cancelRun()
			command := exec.CommandContext(limit, "node", filepath.Join("..", "..", "web", "test", "testdata", "team-e2e", "remote-people-tunnel.cjs"))
			command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
			command.WaitDelay = 5 * time.Second
			command.Env = append(os.Environ(), "SCIMUX_TUNNEL_MODE="+mode, "SCIMUX_TUNNEL_URL="+web.URL, "SCIMUX_TUNNEL_ARTIFACTS="+artifacts)
			output, err := command.CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatalf("browser tunnel journey: %v", err)
			}
			if !held.Load() || dispatches.Load() != 0 {
				t.Fatal("missing held transfer or unexpected agent dispatch")
			}
		})
	}
}
