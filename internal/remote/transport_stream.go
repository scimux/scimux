package remote

// S6 — the two adapters between the data channel and the FR-27 codec.
//
// dcStream turns a message-oriented WebRTC data channel into the byte
// stream the codec reads and writes. httpTunnelHandler turns a codec
// request back into an *http.Request for the S3 tunnel boundary. Neither
// invents policy: the codec owns admission, the boundary owns the response.

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/scimux/scimux/internal/remote/codec"
)

// Data-channel write sizing. dcMaxMessage is well under the 64 KiB an SCTP
// peer is required to accept, and comfortably under the codec's own
// maxFramePayload, so one codec frame may become several messages — which
// is fine, because the reader below reassembles a stream and never reads
// message boundaries.
const (
	dcMaxMessage   = 16 << 10
	dcHighWater    = 1 << 20
	dcLowWater     = 256 << 10
	dcWriteTimeout = 30 * time.Second
)

// dcStream is one end of a data channel presented as an io.ReadWriteCloser.
//
// Reads come from an io.Pipe fed by OnMessage. The pipe blocks the pion
// read loop when the codec is slower than the peer, which is the natural
// backpressure and is why nothing here buffers without bound. Writes chunk
// and wait on the channel's buffered-amount-low signal for the same reason
// in the other direction.
type dcStream struct {
	dc *webrtc.DataChannel

	pr *io.PipeReader
	pw *io.PipeWriter

	openCh   chan struct{}
	openOnce onceCloser

	closeCh   chan struct{}
	closeOnce onceCloser

	lowCh chan struct{}

	// errOnce keeps the first transport error, so a later Close cannot
	// overwrite the reason the stream actually ended.
	errOnce sync.Once
}

func newDCStream(dc *webrtc.DataChannel) *dcStream {
	pr, pw := io.Pipe()
	s := &dcStream{
		dc:      dc,
		pr:      pr,
		pw:      pw,
		openCh:  make(chan struct{}),
		closeCh: make(chan struct{}),
		lowCh:   make(chan struct{}, 1),
	}
	dc.SetBufferedAmountLowThreshold(dcLowWater)
	dc.OnBufferedAmountLow(func() {
		select {
		case s.lowCh <- struct{}{}:
		default:
		}
	})
	dc.OnOpen(func() { s.openOnce.close(s.openCh) })
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if len(msg.Data) == 0 {
			return
		}
		_, _ = s.pw.Write(msg.Data)
	})
	dc.OnClose(func() { s.fail(io.EOF) })
	dc.OnError(func(err error) { s.fail(err) })
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		s.openOnce.close(s.openCh)
	}
	return s
}

// waitOpen blocks until the channel is open, the stream fails, or ctx ends.
func (s *dcStream) waitOpen(ctx context.Context) error {
	select {
	case <-s.openCh:
		return nil
	case <-s.closeCh:
		return classError(ClassHandshake, "channel", "the data channel closed before it opened")
	case <-ctx.Done():
		return classErrorf(ClassHandshake, "channel", "the data channel never opened", ctx.Err())
	}
}

func (s *dcStream) fail(err error) {
	s.errOnce.Do(func() {
		if err == nil {
			err = io.EOF
		}
		_ = s.pw.CloseWithError(err)
	})
	s.closeOnce.close(s.closeCh)
}

func (s *dcStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *dcStream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		select {
		case <-s.closeCh:
			return total, io.ErrClosedPipe
		default:
		}
		if err := s.awaitDrain(); err != nil {
			return total, err
		}
		n := len(p)
		if n > dcMaxMessage {
			n = dcMaxMessage
		}
		if err := s.dc.Send(p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// awaitDrain holds the writer while the channel's send buffer is above the
// high-water mark. Without it a large response (the index, an asset) would
// queue the whole body in memory before SCTP had sent any of it.
func (s *dcStream) awaitDrain() error {
	amount := func() uint64 { return s.dc.BufferedAmount() }
	// Fast path: stay at or below the waterline without arming the write timer.
	if amount() <= dcHighWater {
		return nil
	}
	deadline := time.NewTimer(dcWriteTimeout)
	defer deadline.Stop()
	return awaitBufferedDrain(amount, dcHighWater, s.lowCh, s.closeCh, deadline.C)
}

// awaitBufferedDrain is the pure wait loop behind awaitDrain. Production
// supplies the live DataChannel amount and a dcWriteTimeout timer; tests
// inject amount, waterline, low/close/expiry signals without pion or Sleep.
func awaitBufferedDrain(
	amount func() uint64,
	highWater uint64,
	lowCh <-chan struct{},
	closeCh <-chan struct{},
	expiry <-chan time.Time,
) error {
	for {
		if amount() <= highWater {
			return nil
		}
		select {
		case <-lowCh:
			// Re-read amount on the next iteration; a coalesced low must
			// not succeed while the buffer is still above the waterline.
		case <-closeCh:
			return io.ErrClosedPipe
		case <-expiry:
			return classError(ClassLost, "channel", "the data channel stopped draining")
		}
	}
}

func (s *dcStream) Close() error {
	s.fail(io.ErrClosedPipe)
	_ = s.pr.Close()
	return s.dc.Close()
}

// httpTunnelHandler serves one FR-27 request by replaying it into the S3
// tunnel boundary. It adds no headers of its own: FR-17 forbids
// synthesising Host, Origin, Referer, Sec-Fetch-Site, or the CSRF header,
// and a tunnelled request that lacks one must be judged by the boundary on
// that basis, not helped past it.
type httpTunnelHandler struct {
	h http.Handler
}

func (t *httpTunnelHandler) ServeRemote(ctx context.Context, req *codec.Request) (*codec.Response, error) {
	if req == nil {
		return nil, classError(ClassHandshake, "tunnel", "no request")
	}
	target := req.Path
	if req.Query != "" {
		target += "?" + req.Query
	}
	body := req.Body
	var reqBody io.ReadCloser = http.NoBody
	if body != nil {
		reqBody = body
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, target, reqBody)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "tunnel", "the tunnelled request is not a request", err)
	}
	for name, vals := range req.Headers {
		for _, v := range vals {
			hr.Header.Add(name, v)
		}
	}
	// RemoteAddr is what the boundary logs and rate-limits on. The peer is
	// not on an IP path we can name, and inventing a loopback address here
	// would hand a remote request whatever loopback is trusted with.
	hr.RemoteAddr = ""

	w := newTunnelResponseWriter()
	go func() {
		defer w.finish()
		t.h.ServeHTTP(w, hr)
	}()

	select {
	case <-w.headerCh:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &codec.Response{
		ID:      req.ID,
		Status:  w.status,
		Headers: w.snapshot,
		Body:    w.pr,
	}, nil
}

// tunnelResponseWriter streams the handler's response into the codec
// instead of buffering it: /api/state is small, but an attachment or an
// agent asset is not, and the boundary's own limits are the ones that
// should decide, not this adapter's memory.
type tunnelResponseWriter struct {
	hdr      http.Header
	snapshot http.Header
	status   int

	pr *io.PipeReader
	pw *io.PipeWriter

	headerCh   chan struct{}
	headerOnce sync.Once
}

func newTunnelResponseWriter() *tunnelResponseWriter {
	pr, pw := io.Pipe()
	return &tunnelResponseWriter{
		hdr:      make(http.Header),
		pr:       pr,
		pw:       pw,
		headerCh: make(chan struct{}),
	}
}

func (w *tunnelResponseWriter) Header() http.Header { return w.hdr }

func (w *tunnelResponseWriter) WriteHeader(status int) {
	w.headerOnce.Do(func() {
		w.status = status
		w.snapshot = w.hdr.Clone()
		close(w.headerCh)
	})
}

func (w *tunnelResponseWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

// Flush exists so handlers that flush do not fall back to buffering. The
// stream is already unbuffered, so there is nothing to push.
func (w *tunnelResponseWriter) Flush() {
	w.WriteHeader(http.StatusOK)
}

func (w *tunnelResponseWriter) finish() {
	w.WriteHeader(http.StatusOK)
	_ = w.pw.Close()
}

// codecRequest converts an outbound *http.Request into an FR-27 request.
// Hop-by-hop and authority headers are dropped rather than passed for the
// codec to reject: they are artefacts of the local http.Request, not
// something the caller asked to send.
func codecRequest(req *http.Request) (*codec.Request, error) {
	id, err := randomHex(8)
	if err != nil {
		return nil, classErrorf(ClassHandshake, "round-trip", "could not mint a request id", err)
	}
	hdr := make(http.Header, len(req.Header))
	for name, vals := range req.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Upgrade", "Trailer", "Connection", "Keep-Alive",
			"Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection",
			"Te", "Transfer-Encoding":
			continue
		}
		cp := make([]string, len(vals))
		copy(cp, vals)
		hdr[http.CanonicalHeaderKey(name)] = cp
	}
	path, query := "/", ""
	if req.URL != nil {
		if req.URL.Path != "" {
			path = req.URL.Path
		}
		query = req.URL.RawQuery
	}
	var body io.ReadCloser
	if req.Body != nil && req.Body != http.NoBody {
		body = req.Body
	}
	return &codec.Request{
		ID:      id,
		Method:  req.Method,
		Path:    path,
		Query:   query,
		Headers: hdr,
		Body:    body,
	}, nil
}

// httpResponse converts an FR-27 response back into an *http.Response.
func httpResponse(req *http.Request, resp *codec.Response) *http.Response {
	hdr := resp.Headers
	if hdr == nil {
		hdr = make(http.Header)
	}
	body := resp.Body
	if body == nil {
		body = http.NoBody
	}
	return &http.Response{
		Status:     http.StatusText(resp.Status),
		StatusCode: resp.Status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     hdr,
		Body:       body,
		Request:    req,
	}
}
