package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
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
	Key  string          `json:"key"`
	Body json.RawMessage `json:"body"`
}

// Sentinel errors from the channel calls. None of them quote a token, a
// title, an entry body, or a response body.
var (
	ErrNoChannel       = errors.New("team: no such channel")
	ErrUnknownMember   = errors.New("team: unknown member")
	ErrQuota           = errors.New("team: quota reached")
	ErrTooLarge        = errors.New("team: too large")
	ErrStaleGeneration = errors.New("team: stale generation")
)

const maxSeatsBody = 1 << 20

const (
	maxMirrorEntries = 256
	protocolTime     = "2006-01-02T15:04:05.000Z"
	// maxChannelsBody is the largest channel list that is still a protocol
	// answer. maxMirrorRead is the largest mirror page: 16 MiB of bodies
	// plus the line around them. maxMirrorBody is one entry body.
	maxChannelsBody = 1 << 20
	maxMirrorRead   = (16 << 20) + (64 << 10)
	maxMirrorBody   = 4 << 20
)

// marshalChannelBody encodes a JSON request body. Tests replace it.
var marshalChannelBody = encodeChannelBody

func encodeChannelBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Structs of strings, slices, and raw JSON encode. The error cannot occur.
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Seats is GET /v1/seats. The answer may be up to 1 MiB.
func (c *Client) Seats(ctx context.Context, baseURL, token string) ([]Seat, error) {
	status, raw, err := c.doAuthed(ctx, http.MethodGet, baseURL, "/v1/seats", token, nil, maxSeatsBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, mapChannelStatus(status)
	}
	seats, err := parseSeats(raw)
	if err != nil {
		return nil, ErrProtocol
	}
	return seats, nil
}

// PutChannel is PUT /v1/channels/{owner}/{chat}.
// created is true for 201 and false for 200.
func (c *Client) PutChannel(ctx context.Context, baseURL, token, owner, chat, title string) (string, bool, error) {
	path, err := channelPath(owner, chat, "")
	if err != nil {
		return "", false, err
	}
	body, err := marshalChannelBody(struct {
		Title string `json:"title"`
	}{Title: title})
	if err != nil {
		return "", false, ErrProtocol
	}
	status, raw, err := c.doAuthed(ctx, http.MethodPut, baseURL, path, token, body, maxResponseBody)
	if err != nil {
		return "", false, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", false, mapChannelStatus(status)
	}
	ch, err := parseChannel(raw)
	if err != nil {
		return "", false, ErrProtocol
	}
	return ch.Generation, status == http.StatusCreated, nil
}

// DeleteChannel is DELETE /v1/channels/{owner}/{chat}.
func (c *Client) DeleteChannel(ctx context.Context, baseURL, token, owner, chat string) error {
	path, err := channelPath(owner, chat, "")
	if err != nil {
		return err
	}
	status, raw, err := c.doAuthed(ctx, http.MethodDelete, baseURL, path, token, nil, maxResponseBody)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return mapChannelStatus(status)
	}
	if len(raw) != 0 {
		return ErrProtocol
	}
	return nil
}

// SetMembers is PUT /v1/channels/{owner}/{chat}/members.
func (c *Client) SetMembers(ctx context.Context, baseURL, token, owner, chat string, members []Member) error {
	path, err := channelPath(owner, chat, "/members")
	if err != nil {
		return err
	}
	if members == nil {
		members = []Member{}
	}
	body, err := marshalChannelBody(struct {
		Members []Member `json:"members"`
	}{Members: members})
	if err != nil {
		return ErrProtocol
	}
	status, raw, err := c.doAuthed(ctx, http.MethodPut, baseURL, path, token, body, maxResponseBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return mapChannelStatus(status)
	}
	if _, err := parseChannel(raw); err != nil {
		return ErrProtocol
	}
	return nil
}

// AppendMirror is POST /v1/channels/{owner}/{chat}/mirror.
// generation is the channel the entries were prepared for.
func (c *Client) AppendMirror(ctx context.Context, baseURL, token, owner, chat, generation string, entries []MirrorEntry) (string, int64, error) {
	path, err := channelPath(owner, chat, "/mirror")
	if err != nil {
		return "", 0, err
	}
	if !validHex(generation, 16) {
		return "", 0, ErrProtocol
	}
	if err := validMirrorEntries(entries); err != nil {
		return "", 0, err
	}
	body, err := marshalChannelBody(struct {
		Generation string        `json:"generation"`
		Entries    []MirrorEntry `json:"entries"`
	}{Generation: generation, Entries: entries})
	if err != nil {
		return "", 0, ErrProtocol
	}
	status, raw, err := c.doAuthed(ctx, http.MethodPost, baseURL, path, token, body, maxResponseBody)
	if err != nil {
		return "", 0, err
	}
	if status != http.StatusOK {
		return "", 0, mapChannelStatus(status)
	}
	return parseAppend(raw, entries)
}

func channelPath(owner, chat, rest string) (string, error) {
	if !validHex(owner, 32) || !validChatID(chat) {
		return "", ErrProtocol
	}
	return "/v1/channels/" + url.PathEscape(owner) + "/" + url.PathEscape(chat) + rest, nil
}

func validChatID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validEntryKey(s string) bool {
	if len(s) < 1 || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return true
}

func validMirrorEntries(entries []MirrorEntry) error {
	if len(entries) < 1 || len(entries) > maxMirrorEntries {
		return ErrProtocol
	}
	for _, e := range entries {
		if !validEntryKey(e.Key) || !json.Valid(e.Body) {
			return ErrProtocol
		}
	}
	return nil
}

func mapChannelStatus(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNoChannel
	case http.StatusRequestEntityTooLarge:
		return ErrTooLarge
	case http.StatusUnprocessableEntity:
		return ErrUnknownMember
	case http.StatusInsufficientStorage:
		return ErrQuota
	case http.StatusConflict:
		return ErrStaleGeneration
	default:
		return ErrProtocol
	}
}

type seatWire struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func parseSeats(raw []byte) ([]Seat, error) {
	var doc struct {
		Seats *[]seatWire `json:"seats"`
	}
	if err := decodeJSON(raw, &doc); err != nil || doc.Seats == nil {
		return nil, ErrProtocol
	}
	out := make([]Seat, 0, len(*doc.Seats))
	for _, s := range *doc.Seats {
		if !validHex(s.ID, 32) || !validStoredName(s.Name, true) {
			return nil, ErrProtocol
		}
		out = append(out, Seat{ID: s.ID, Name: s.Name})
	}
	return out, nil
}

type channelWire struct {
	Owner        string    `json:"owner"`
	Chat         string    `json:"chat"`
	Title        string    `json:"title"`
	Generation   string    `json:"generation"`
	CreatedAt    string    `json:"created_at"`
	Access       string    `json:"access"`
	MirrorLast   uint64    `json:"mirror_last"`
	CommentsLast uint64    `json:"comments_last"`
	Members      *[]Member `json:"members"`
}

func parseChannel(raw []byte) (channelWire, error) {
	// encoding/json replaces an invalid byte inside a string, so the
	// raw body is checked before it is decoded.
	if !utf8.Valid(raw) {
		return channelWire{}, ErrProtocol
	}
	var doc channelWire
	if err := decodeJSON(raw, &doc); err != nil {
		return channelWire{}, ErrProtocol
	}
	if !validHex(doc.Owner, 32) || !validChatID(doc.Chat) || !validChannelTitle(doc.Title) || !validHex(doc.Generation, 16) || !validProtocolTime(doc.CreatedAt) {
		return channelWire{}, ErrProtocol
	}
	if doc.Access != "owner" && doc.Access != "read" && doc.Access != "write" {
		return channelWire{}, ErrProtocol
	}
	if doc.Members == nil {
		return channelWire{}, ErrProtocol
	}
	seen := make(map[string]struct{}, len(*doc.Members))
	for _, m := range *doc.Members {
		if !validHex(m.Seat, 32) || (m.Access != "read" && m.Access != "write") {
			return channelWire{}, ErrProtocol
		}
		if _, ok := seen[m.Seat]; ok {
			return channelWire{}, ErrProtocol
		}
		seen[m.Seat] = struct{}{}
	}
	return doc, nil
}

func validChannelTitle(s string) bool {
	if len(s) > 200 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validProtocolTime(s string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return false
	}
	return parsed.UTC().Format(protocolTime) == s
}

type appendResult struct {
	Key       string `json:"key"`
	Seq       uint64 `json:"seq"`
	Duplicate bool   `json:"duplicate"`
}

func parseAppend(raw []byte, entries []MirrorEntry) (string, int64, error) {
	var doc struct {
		Generation string          `json:"generation"`
		Results    *[]appendResult `json:"results"`
		Last       *int64          `json:"last"`
	}
	if err := decodeJSON(raw, &doc); err != nil {
		return "", 0, ErrProtocol
	}
	if !validHex(doc.Generation, 16) || doc.Results == nil || doc.Last == nil || *doc.Last < 0 || len(*doc.Results) != len(entries) {
		return "", 0, ErrProtocol
	}
	for i, res := range *doc.Results {
		if !validEntryKey(res.Key) || res.Key != entries[i].Key {
			return "", 0, ErrProtocol
		}
	}
	return doc.Generation, *doc.Last, nil
}

// Channel is one channel the caller can see.
type Channel struct {
	Owner        string
	Chat         string
	Title        string
	Generation   string
	CreatedAt    string
	Access       string
	MirrorLast   uint64
	CommentsLast uint64
	Members      []Member
}

// MirrorRecord is one mirror entry. Body is the JSON value the server sent.
type MirrorRecord struct {
	Seq    uint64
	Key    string
	Author string
	At     string
	Body   json.RawMessage
}

// MirrorPage is one page of a mirror log.
type MirrorPage struct {
	Generation string
	Entries    []MirrorRecord
	Last       uint64
	More       bool
}

// Channels is GET /v1/channels.
func (c *Client) Channels(ctx context.Context, baseURL, token string, skipInvalidIDs ...bool) ([]Channel, error) {
	status, raw, err := c.doAuthed(ctx, http.MethodGet, baseURL, "/v1/channels", token, nil, maxChannelsBody)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, mapChannelStatus(status)
	}
	list, err := parseChannels(raw)
	if err != nil {
		return nil, ErrProtocol
	}
	return list, nil
}

// ReadMirror is GET /v1/channels/{owner}/{chat}/mirror?after=.
func (c *Client) ReadMirror(ctx context.Context, baseURL, token, owner, chat string, after uint64) (MirrorPage, error) {
	path, err := channelPath(owner, chat, "/mirror")
	if err != nil {
		return MirrorPage{}, err
	}
	status, raw, err := c.withAfter(after).doAuthed(ctx, http.MethodGet, baseURL, path, token, nil, maxMirrorRead)
	if err != nil {
		return MirrorPage{}, err
	}
	if status != http.StatusOK {
		return MirrorPage{}, mapChannelStatus(status)
	}
	page, err := parseMirrorPage(raw, after)
	if err != nil {
		return MirrorPage{}, ErrProtocol
	}
	return page, nil
}

// queryTrip adds the mirror page query. The inner transport is the client's.
type queryTrip struct {
	inner http.RoundTripper
	query string
}

func (q queryTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	next := r.Clone(r.Context())
	next.URL.RawQuery = q.query
	return q.inner.RoundTrip(next)
}

// withAfter returns a client that sends after= on the next call.
func (c *Client) withAfter(after uint64) *Client {
	clone := *c
	inner := c.Transport
	if inner == nil {
		inner = &http.Transport{Proxy: nil, DisableKeepAlives: true}
	}
	clone.Transport = queryTrip{inner: inner, query: "after=" + strconv.FormatUint(after, 10)}
	return &clone
}

func parseChannels(raw []byte) ([]Channel, error) {
	if !utf8.Valid(raw) {
		return nil, ErrProtocol
	}
	var doc struct {
		Channels *[]json.RawMessage `json:"channels"`
	}
	if err := decodeJSON(raw, &doc); err != nil || doc.Channels == nil {
		return nil, ErrProtocol
	}
	out := make([]Channel, 0, len(*doc.Channels))
	for _, one := range *doc.Channels {
		ch, err := parseListedChannel(one)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

func parseListedChannel(raw json.RawMessage) (Channel, error) {
	var doc struct {
		Owner        string          `json:"owner"`
		Chat         string          `json:"chat"`
		Title        string          `json:"title"`
		Generation   string          `json:"generation"`
		CreatedAt    string          `json:"created_at"`
		Access       string          `json:"access"`
		MirrorLast   uint64          `json:"mirror_last"`
		CommentsLast uint64          `json:"comments_last"`
		Members      json.RawMessage `json:"members"`
	}
	if err := decodeJSON(raw, &doc); err != nil {
		return Channel{}, ErrProtocol
	}
	if !validHex(doc.Owner, 32) || !validChatID(doc.Chat) || !validChannelTitle(doc.Title) || !validHex(doc.Generation, 16) || !validProtocolTime(doc.CreatedAt) {
		return Channel{}, ErrProtocol
	}
	if doc.Access != "owner" && doc.Access != "read" && doc.Access != "write" {
		return Channel{}, ErrProtocol
	}
	members, err := listedMembers(doc.Access, doc.Members)
	if err != nil {
		return Channel{}, err
	}
	return Channel{
		Owner: doc.Owner, Chat: doc.Chat, Title: doc.Title, Generation: doc.Generation,
		CreatedAt: doc.CreatedAt, Access: doc.Access, MirrorLast: doc.MirrorLast,
		CommentsLast: doc.CommentsLast, Members: members,
	}, nil
}

func listedMembers(access string, raw json.RawMessage) ([]Member, error) {
	if access != "owner" {
		if len(raw) != 0 {
			return nil, ErrProtocol
		}
		return nil, nil
	}
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, ErrProtocol
	}
	var members []Member
	if err := decodeJSON(raw, &members); err != nil {
		return nil, ErrProtocol
	}
	if err := checkListedMembers(members); err != nil {
		return nil, err
	}
	return members, nil
}

func checkListedMembers(members []Member) error {
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		if !validHex(m.Seat, 32) || (m.Access != "read" && m.Access != "write") {
			return ErrProtocol
		}
		if _, ok := seen[m.Seat]; ok {
			return ErrProtocol
		}
		seen[m.Seat] = struct{}{}
	}
	return nil
}

func parseMirrorPage(raw []byte, after uint64) (MirrorPage, error) {
	if !utf8.Valid(raw) {
		return MirrorPage{}, ErrProtocol
	}
	var doc struct {
		Generation string             `json:"generation"`
		Entries    *[]json.RawMessage `json:"entries"`
		Last       *uint64            `json:"last"`
		More       *bool              `json:"more"`
	}
	if err := decodeJSON(raw, &doc); err != nil || doc.Entries == nil || doc.Last == nil || doc.More == nil {
		return MirrorPage{}, ErrProtocol
	}
	if !validHex(doc.Generation, 16) {
		return MirrorPage{}, ErrProtocol
	}
	if len(*doc.Entries) > maxMirrorEntries {
		return MirrorPage{}, ErrProtocol
	}
	entries := make([]MirrorRecord, 0, len(*doc.Entries))
	var prev uint64
	for i, one := range *doc.Entries {
		rec, err := parseMirrorRecord(one)
		if err != nil {
			return MirrorPage{}, err
		}
		if rec.Seq <= after || (i > 0 && rec.Seq <= prev) {
			return MirrorPage{}, ErrProtocol
		}
		prev = rec.Seq
		entries = append(entries, rec)
	}
	if *doc.More {
		if len(entries) == 0 || *doc.Last <= entries[len(entries)-1].Seq {
			return MirrorPage{}, ErrProtocol
		}
	} else if len(entries) > 0 && *doc.Last != entries[len(entries)-1].Seq {
		return MirrorPage{}, ErrProtocol
	}
	return MirrorPage{Generation: doc.Generation, Entries: entries, Last: *doc.Last, More: *doc.More}, nil
}

func parseMirrorRecord(raw json.RawMessage) (MirrorRecord, error) {
	var doc struct {
		Seq    *uint64         `json:"seq"`
		Key    string          `json:"key"`
		Author string          `json:"author"`
		At     string          `json:"at"`
		Body   json.RawMessage `json:"body"`
	}
	if err := decodeJSON(raw, &doc); err != nil || doc.Seq == nil {
		return MirrorRecord{}, ErrProtocol
	}
	if !validEntryKey(doc.Key) || !validHex(doc.Author, 32) || !validProtocolTime(doc.At) {
		return MirrorRecord{}, ErrProtocol
	}
	if len(doc.Body) == 0 || len(doc.Body) > maxMirrorBody {
		return MirrorRecord{}, ErrProtocol
	}
	return MirrorRecord{
		Seq: *doc.Seq, Key: doc.Key, Author: doc.Author, At: doc.At,
		Body: append(json.RawMessage(nil), doc.Body...),
	}, nil
}
