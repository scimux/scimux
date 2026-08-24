// Package codec implements the FR-27 tunnel request/response frame codec.
//
// Packet S4. Standard library only: no pion, no network, no internal/app.
// Bodies travel as chunked frames with an explicit end marker; per-route
// caps are enforced as the stream arrives. Header fidelity is not a property:
// names outside the allowlist are dropped.
package codec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// Per-route body caps. These are the codec's own copies of the handler
// limits; the codec cannot inherit a boundary-level cap and must not be
// tighter than the handler or it silently breaks uploads.
//
// Values match the shipped handlers (jsonBodyMax / uiStateMax / attachUploadMax
// / agentAssetMaxBytes). jsonBodyMax and uiStateMax are both 1 MiB — the
// comment in security.go that calls /api/ui "larger" is stale.
const (
	JSONBodyMax     int64 = 1 << 20
	UIStateMax      int64 = 1 << 20
	AttachUploadMax int64 = 25 << 20
	AgentAssetMax   int64 = 50 << 20
)

// ErrUnimplemented is returned by R1 stubs. It is not a RejectError.
var ErrUnimplemented = errors.New("codec: unimplemented")

// Class names the FR-27 rejection classes. Tests match on Class, not on
// strings, so a plausible wrong error cannot satisfy a row.
type Class string

const (
	ClassAbsoluteURI    Class = "absolute-uri"
	ClassAuthority      Class = "authority"
	ClassHopByHop       Class = "hop-by-hop"
	ClassUpgrade        Class = "upgrade"
	ClassTrailer        Class = "trailer"
	ClassLengthConflict Class = "length-conflict"
	ClassCRLF           Class = "crlf"
	ClassMethod         Class = "method"
	ClassBodyTooLarge   Class = "body-too-large"
	ClassMalformed      Class = "malformed"
	ClassTruncated      Class = "truncated"
)

// RejectError is a request or frame the codec refused. None of the named
// classes may be sanitised into acceptance.
type RejectError struct {
	Class Class
	Field string
}

func (e *RejectError) Error() string {
	if e == nil {
		return "codec: rejected"
	}
	if e.Field != "" {
		return fmt.Sprintf("codec: rejected %s in %s", e.Class, e.Field)
	}
	return fmt.Sprintf("codec: rejected %s", e.Class)
}

// Request is one FR-27 request: id, allowed method, origin-relative path
// and query, allowlisted headers, streamed body.
type Request struct {
	ID      string
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    io.ReadCloser
}

// Response is one FR-27 response. Status 206 and the Range-related headers
// must survive intact.
type Response struct {
	ID      string
	Status  int
	Headers http.Header
	Body    io.ReadCloser
}

// Handler serves one remote request. ctx is cancelled when the peer cancels.
type Handler interface {
	ServeRemote(ctx context.Context, req *Request) (*Response, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, req *Request) (*Response, error)

// ServeRemote calls f.
func (f HandlerFunc) ServeRemote(ctx context.Context, req *Request) (*Response, error) {
	return f(ctx, req)
}

// Conn is one end of a codec session over a bidirectional byte stream.
type Conn struct {
	r io.Reader
	w io.Writer

	// role is this side's tunnel-v2 §5.1 role, sent in our hello.
	role uint8
	// Impl is free diagnostic text sent in our hello. The peer must never
	// parse it for behaviour (§5.1).
	Impl string

	// br is owned from construction, not created inside the read loop, so
	// the handshake and the loop read from the same buffer. Two readers
	// over one stream would lose whatever the first one buffered past its
	// own last byte.
	br *bufio.Reader

	handshakeOnce sync.Once
	handshakeErr  error
	peerHello     Hello
	havePeerHello atomic.Bool

	writeMu sync.Mutex

	closeOnce sync.Once
	closed    atomic.Bool
	closeCh   chan struct{}

	loopOnce sync.Once

	mu      sync.Mutex
	pending map[string]*call
	inBody  map[string]*incoming
	cancels map[string]context.CancelFunc
	reqMeta map[string]reqMeta
	handler Handler
	serveWG sync.WaitGroup
}

type reqMeta struct {
	method string
	path   string
}

type call struct {
	respCh  chan respOrErr
	bodyW   *io.PipeWriter
	respCap int64
	n       int64
	settled bool
}

type respOrErr struct {
	resp *Response
	err  error
}

// NewConn binds one side of a pair. r and w are typically the two halves
// of a crossed io.Pipe pair; the codec does not open a network.
//
// role is RoleInitiator or RoleResponder (tunnel-v2 §5.1). It is fixed at
// construction because it is a property of which end of the tunnel this
// is, not of any one request, and the peer is told it in the hello.
func NewConn(r io.Reader, w io.Writer, role uint8) *Conn {
	return &Conn{
		r:       r,
		w:       w,
		role:    role,
		br:      bufio.NewReaderSize(r, chunkBufSize),
		closeCh: make(chan struct{}),
		pending: make(map[string]*call),
		inBody:  make(map[string]*incoming),
		cancels: make(map[string]context.CancelFunc),
		reqMeta: make(map[string]reqMeta),
	}
}

// allowedMethods is the closed method set. Admission is a map lookup;
// anything not present is ClassMethod. Tests generate the complement
// from MethodAllowlist rather than listing rejected methods.
var allowedMethods = map[string]struct{}{
	http.MethodGet:    {},
	http.MethodHead:   {},
	http.MethodPost:   {},
	http.MethodPut:    {},
	http.MethodPatch:  {},
	http.MethodDelete: {},
}

// Canonical MIME names. X-Scimux-CSRF, Accept-Encoding, Content-Encoding,
// Host, and every hop-by-hop name are deliberately absent.
var allowedRequestHeaders = map[string]struct{}{
	"Content-Length":    {},
	"Content-Type":      {},
	"If-Match":          {},
	"If-Modified-Since": {},
	"If-None-Match":     {},
	"Range":             {},
}

var allowedResponseHeaders = map[string]struct{}{
	"Accept-Ranges":          {},
	"Cache-Control":          {},
	"Content-Disposition":    {},
	"Content-Length":         {},
	"Content-Range":          {},
	"Content-Type":           {},
	"Etag":                   {},
	"Last-Modified":          {},
	"X-Content-Type-Options": {},
}

// MethodAllowlist returns a copy of the closed method set.
func MethodAllowlist() map[string]struct{} {
	return copySet(allowedMethods)
}

// RequestHeaderAllowlist returns a copy of the closed request-header set.
func RequestHeaderAllowlist() map[string]struct{} {
	return copySet(allowedRequestHeaders)
}

// ResponseHeaderAllowlist returns a copy of the closed response-header set.
func ResponseHeaderAllowlist() map[string]struct{} {
	return copySet(allowedResponseHeaders)
}

func copySet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func reject(class Class, field string) error {
	return &RejectError{Class: class, Field: field}
}
