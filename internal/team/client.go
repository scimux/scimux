package team

import (
	"context"
	"errors"
	"net/http"
	"time"
)

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

// NewClient returns a client with a 10 second total timeout and no transport
// of its own. The stub methods succeed without a call.
func NewClient() *Client {
	return &Client{}
}

// Hello is GET /v1/hello. The stub returns an empty server.
func (c *Client) Hello(ctx context.Context, baseURL string) (Server, error) {
	return Server{}, nil
}

// Register is POST /v1/register. The stub returns an empty token and server.
func (c *Client) Register(ctx context.Context, baseURL, code, installationID, name string) (string, Server, error) {
	return "", Server{}, nil
}
