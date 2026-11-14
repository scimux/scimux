package team

import (
	"context"
	"encoding/json"
	"errors"
)

// Seat is one registered seat on a team server.
type Seat struct {
	ID   string
	Name string
}

// Member is one seat allowed to read a channel or to write comments.
type Member struct {
	Seat   string `json:"seat"`
	Access string `json:"access"` // "read" or "write"
}

// MirrorEntry is one record appended to a channel mirror.
type MirrorEntry struct {
	Key  string
	Body json.RawMessage
}

// Sentinel errors from the channel calls. None of them quote a token, a
// title, an entry body, or a response body.
var (
	ErrNoChannel     = errors.New("team: no such channel")
	ErrUnknownMember = errors.New("team: unknown member")
	ErrQuota         = errors.New("team: quota reached")
	ErrTooLarge      = errors.New("team: too large")
)

// errChannelStub is the red-state answer. The implementation replaces it.
var errChannelStub = errors.New("team: channel stub")

// marshalChannelBody encodes a JSON request body. Tests replace it.
var marshalChannelBody = func(v any) ([]byte, error) {
	return nil, errChannelStub
}

// Seats is GET /v1/seats.
func (c *Client) Seats(context.Context, string, string) ([]Seat, error) {
	return nil, errChannelStub
}

// PutChannel is PUT /v1/channels/{owner}/{chat}.
// created is true for 201 and false for 200.
func (c *Client) PutChannel(context.Context, string, string, string, string, string) (string, bool, error) {
	return "", false, errChannelStub
}

// DeleteChannel is DELETE /v1/channels/{owner}/{chat}.
func (c *Client) DeleteChannel(context.Context, string, string, string, string) error {
	return errChannelStub
}

// SetMembers is PUT /v1/channels/{owner}/{chat}/members.
func (c *Client) SetMembers(context.Context, string, string, string, string, []Member) error {
	return errChannelStub
}

// AppendMirror is POST /v1/channels/{owner}/{chat}/mirror.
func (c *Client) AppendMirror(context.Context, string, string, string, string, []MirrorEntry) (string, int64, error) {
	return "", 0, errChannelStub
}
