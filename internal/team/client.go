package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const maxResponseBody = 64 * 1024

// Server is the identity a team server reports for itself.
type Server struct {
	ID   string
	Name string
}

// Sentinel errors from Hello and Register. None of them quote a join code,
// a token, or a response body.
var (
	ErrUnreachable       = errors.New("team: server unreachable")
	ErrProtocol          = errors.New("team: protocol mismatch")
	ErrJoinRefused       = errors.New("team: join refused")
	ErrAlreadyRegistered = errors.New("team: already registered")
	ErrServerFull        = errors.New("team: server full")
	ErrRejected          = errors.New("team: registration rejected")
)

// Client calls a team server. Transport, when set, replaces the direct
// transport. The timeout is the total time for one call.
type Client struct {
	Transport http.RoundTripper
	timeout   time.Duration
}

// NewClient returns a client that dials directly, follows no redirect, and
// gives each call 10 seconds.
func NewClient() *Client {
	return &Client{timeout: 10 * time.Second}
}

type wireServer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type helloResponse struct {
	Protocol int        `json:"protocol"`
	Server   wireServer `json:"server"`
}

type registerRequest struct {
	JoinCode       string `json:"join_code"`
	InstallationID string `json:"installation_id"`
	Name           string `json:"name"`
}

type registerResponse struct {
	Token  string     `json:"token"`
	Server wireServer `json:"server"`
}

// Hello is GET /v1/hello.
func (c *Client) Hello(ctx context.Context, baseURL string) (Server, error) {
	var out helloResponse
	if err := c.exchange(ctx, http.MethodGet, baseURL, "/v1/hello", nil, http.StatusOK, &out); err != nil {
		return Server{}, err
	}
	if out.Protocol != 1 || !validServer(out.Server) {
		return Server{}, ErrProtocol
	}
	return Server{ID: out.Server.ID, Name: out.Server.Name}, nil
}

// Register is POST /v1/register.
func (c *Client) Register(ctx context.Context, baseURL, code, installationID, name string) (string, Server, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// A struct of strings encodes. The error cannot occur for this value.
	_ = enc.Encode(registerRequest{JoinCode: code, InstallationID: installationID, Name: name})
	body := bytes.TrimRight(buf.Bytes(), "\n")
	var out registerResponse
	if err := c.exchange(ctx, http.MethodPost, baseURL, "/v1/register", body, http.StatusCreated, &out); err != nil {
		return "", Server{}, err
	}
	if !validHex(out.Token, 64) || !validServer(out.Server) {
		return "", Server{}, ErrProtocol
	}
	return out.Token, Server{ID: out.Server.ID, Name: out.Server.Name}, nil
}

func (c *Client) exchange(ctx context.Context, method, baseURL, path string, body []byte, okStatus int, dest any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := endpointURL(baseURL, path)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// method and endpoint are built here, so the request constructor succeeds.
	req, _ := http.NewRequestWithContext(ctx, method, endpoint, reader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ErrUnreachable
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return ErrUnreachable
	}
	if len(raw) > maxResponseBody {
		return ErrProtocol
	}
	if resp.StatusCode != okStatus {
		return mapStatusError(raw)
	}
	if err := decodeJSON(raw, dest); err != nil {
		return ErrProtocol
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	transport := c.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil}
	}
	return &http.Client{
		Timeout:       c.timeout,
		Transport:     transport,
		CheckRedirect: refuseRedirect,
	}
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func endpointURL(baseURL, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", ErrProtocol
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func mapStatusError(raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	if err := decodeJSON(raw, &body); err != nil {
		return ErrProtocol
	}
	switch body.Error {
	case "join_refused":
		return ErrJoinRefused
	case "already_registered":
		return ErrAlreadyRegistered
	case "quota":
		return ErrServerFull
	case "bad_request":
		return ErrRejected
	default:
		return ErrProtocol
	}
}

func decodeJSON(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("team: trailing json")
	}
	return nil
}

func validServer(s wireServer) bool {
	return validHex(s.ID, 32) && validName(s.Name)
}

func validHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("0123456789abcdef", s[i]) < 0 {
			return false
		}
	}
	return true
}

func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
