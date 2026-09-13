package backend

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ProtocolMajor changes only when an old muxer and a new web child cannot
	// safely communicate. Minor changes are additive and unknown capabilities
	// are ignored.
	ProtocolMajor = 1
	ProtocolMinor = 0

	headerToken = "X-Scimux-Backend-Token"
	headerMajor = "X-Scimux-Backend-Major"
	headerMinor = "X-Scimux-Backend-Minor"
)

var capabilities = []string{"http-proxy-v1", "status-v1"}

// Link is the complete authority needed to call one muxer. It is passed to a
// child explicitly; children never scan temporary directories or process
// tables to discover a muxer.
type Link struct {
	Socket string `json:"socket"`
	Token  string `json:"token"`
}

// Hello is the compatibility description returned by a muxer.
type Hello struct {
	Major        int      `json:"major"`
	Minor        int      `json:"minor"`
	Capabilities []string `json:"capabilities"`
}

// Status is published by the active web generation. Current senders always
// provide Remote, including an empty value for local-only service. A nil
// pointer remains valid for older senders and means "retain" to the muxer.
type Status struct {
	Generation uint64  `json:"generation"`
	Version    string  `json:"version"`
	Remote     *string `json:"remote,omitempty"`
}

// Server owns an owner-only runtime directory and one Unix listener in it.
type Server struct {
	link Link
	dir  string
	ln   net.Listener
	http *http.Server
	done chan struct{}
	once sync.Once
}

// Listen starts a muxer API on a filesystem Unix socket. parent may be empty,
// in which case the operating system's short temporary root is used. The
// directory name is random and mode 0700, avoiding both socket-path limits
// and exposure through an arbitrarily located data directory.
func Listen(parent string, handler http.Handler) (*Server, error) {
	if handler == nil {
		return nil, errors.New("backend: nil handler")
	}
	dir, err := os.MkdirTemp(parent, "scimux-runtime-")
	if err != nil {
		return nil, fmt.Errorf("backend: create runtime directory: %w", err)
	}
	cleanupDir := func() { _ = os.Remove(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanupDir()
		return nil, fmt.Errorf("backend: protect runtime directory: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		cleanupDir()
		return nil, err
	}
	path := filepath.Join(dir, "muxer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		cleanupDir()
		return nil, fmt.Errorf("backend: listen: %w", err)
	}

	s := &Server{
		link: Link{Socket: path, Token: token},
		dir:  dir,
		ln:   ln,
		done: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_scimux/hello", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Hello{
			Major: ProtocolMajor, Minor: ProtocolMinor,
			Capabilities: append([]string(nil), capabilities...),
		})
	})
	mux.Handle("/", handler)
	s.http = &http.Server{Handler: authenticate(s.link.Token, mux)}
	go func() {
		defer close(s.done)
		_ = s.http.Serve(ln)
	}()
	return s, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("backend: generate capability: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func authenticate(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(headerToken)), []byte(token)) != 1 {
			http.Error(w, "backend capability rejected", http.StatusForbidden)
			return
		}
		major, err := strconv.Atoi(r.Header.Get(headerMajor))
		if err != nil || major != ProtocolMajor {
			w.Header().Set("X-Scimux-Backend-Major", strconv.Itoa(ProtocolMajor))
			http.Error(w, "backend protocol major mismatch", http.StatusUpgradeRequired)
			return
		}
		minor, err := strconv.Atoi(r.Header.Get(headerMinor))
		if err != nil || minor < 0 {
			http.Error(w, "invalid backend protocol minor", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Link returns a copy of the server capability.
func (s *Server) Link() Link {
	if s == nil {
		return Link{}
	}
	return s.link
}

// Close drains the private HTTP server and removes exactly the socket and
// directory this Server created. It never recursively removes a caller-owned
// temporary directory.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var closeErr error
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.http.Shutdown(ctx); err != nil {
			closeErr = err
			_ = s.ln.Close()
		}
		<-s.done
		if err := os.Remove(s.link.Socket); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
			closeErr = err
		}
		if err := os.Remove(s.dir); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

// Client is a versioned caller and HTTP proxy for one Link.
type Client struct {
	link      Link
	transport *http.Transport
	http      *http.Client
}

// NewClient validates link shape without dialing it.
func NewClient(link Link) (*Client, error) {
	if link.Socket == "" {
		return nil, errors.New("backend: empty socket path")
	}
	if link.Token == "" {
		return nil, errors.New("backend: empty capability")
	}
	tr := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", link.Socket)
		},
	}
	return &Client{
		link:      link,
		transport: tr,
		http: &http.Client{
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if c == nil || c.http == nil {
		return nil, errors.New("backend: nil client")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://scimux-muxer"+path, body)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	return req, nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set(headerToken, c.link.Token)
	req.Header.Set(headerMajor, strconv.Itoa(ProtocolMajor))
	req.Header.Set(headerMinor, strconv.Itoa(ProtocolMinor))
}

// Hello proves that the peer is a compatible muxer before a web generation
// announces readiness.
func (c *Client) Hello(ctx context.Context) (Hello, error) {
	req, err := c.request(ctx, http.MethodGet, "/_scimux/hello", nil)
	if err != nil {
		return Hello{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Hello{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Hello{}, fmt.Errorf("backend: hello: %s", resp.Status)
	}
	var hello Hello
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&hello); err != nil {
		return Hello{}, fmt.Errorf("backend: hello: %w", err)
	}
	if hello.Major != ProtocolMajor {
		return Hello{}, fmt.Errorf("backend: hello major %d, want %d", hello.Major, ProtocolMajor)
	}
	if !hasCapability(hello.Capabilities, "http-proxy-v1") {
		return Hello{}, errors.New("backend: muxer lacks http-proxy-v1")
	}
	return hello, nil
}

// PublishStatus updates muxer-side projections after activation. Older web
// generations may race one final report while draining; the receiving muxer
// is responsible for rejecting a generation regression.
func (c *Client) PublishStatus(ctx context.Context, status Status) error {
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, "/_scimux/status", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("backend: publish status: %s", resp.Status)
	}
	return nil
}

// RequestStop asks the muxer to enter the same graceful shutdown path used by
// SIGTERM. Accepted means shutdown was scheduled; the caller observes the
// registration disappearing to know that teardown completed.
func (c *Client) RequestStop(ctx context.Context) error {
	req, err := c.request(ctx, http.MethodPost, "/_scimux/stop", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("backend: request stop: %s", resp.Status)
	}
	return nil
}

func hasCapability(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// Proxy returns the web-generation side of the muxer API link.
func (c *Client) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, incoming *http.Request) {
		if c == nil || c.http == nil {
			http.Error(w, "muxer unavailable", http.StatusBadGateway)
			return
		}
		req := incoming.Clone(incoming.Context())
		req.URL.Scheme = "http"
		req.URL.Host = "scimux-muxer"
		req.RequestURI = ""
		req.Header = incoming.Header.Clone()
		removeHopHeaders(req.Header)
		c.authorize(req)
		resp, err := c.http.Do(req)
		if err != nil {
			http.Error(w, "muxer unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		removeHopHeaders(resp.Header)
		for name, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(h http.Header) {
	for _, token := range strings.Split(h.Get("Connection"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			h.Del(token)
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// Close releases idle Unix-socket connections held by the proxy.
func (c *Client) Close() error {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	return nil
}
