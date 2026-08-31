package codec

import (
	"context"
	"errors"
	"io"
)

const chunkBufSize = bodyChunkSize

// RoundTrip sends req and returns the matching response. ctx cancellation
// must propagate to the peer handler.
func (c *Conn) RoundTrip(ctx context.Context, req *Request) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hdrs, err := validateRequest(req)
	if err != nil {
		return nil, err
	}
	// §7 step 0. Nothing goes on the wire before negotiation succeeds:
	// a request sent to a peer at another major is a request that peer
	// will misread, and the FR-24 state has to be the version, not
	// whatever the misreading produced.
	if err := c.Handshake(ctx); err != nil {
		return nil, err
	}
	id := req.ID
	reqCap := requestBodyLimit(req.Method, req.Path)

	cl := &call{
		respCh:  make(chan respOrErr, 1),
		respCap: responseBodyLimit(req.Method, req.Path),
	}
	c.mu.Lock()
	if _, exists := c.pending[id]; exists {
		c.mu.Unlock()
		return nil, reject(ClassMalformed, "")
	}
	c.pending[id] = cl
	c.mu.Unlock()

	c.ensureClientLoop()

	payload, err := encodeRequestPayload(id, req.Method, req.Path, req.Query, hdrs)
	if err != nil {
		c.forgetCall(id)
		return nil, err
	}
	if err := c.writeFrame(typeRequest, payload); err != nil {
		c.forgetCall(id)
		return nil, err
	}

	bodyDone := make(chan error, 1)
	go func() {
		bodyDone <- c.writeOutgoingBody(ctx, id, req.Body, reqCap)
	}()

	fail := func(err error) (*Response, error) {
		_ = c.writeCancel(id)
		c.failCall(id, err)
		c.forgetCall(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case err := <-bodyDone:
		if err != nil {
			return fail(err)
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case res := <-cl.respCh:
			if res.err != nil {
				c.forgetCall(id)
				return nil, res.err
			}
			return res.resp, nil
		}
	case res := <-cl.respCh:
		if res.err != nil {
			c.forgetCall(id)
			return nil, res.err
		}
		go func() { <-bodyDone }()
		return res.resp, nil
	}
}

func (c *Conn) writeOutgoingBody(ctx context.Context, id string, body io.Reader, capN int64) error {
	if body == nil {
		return c.writeFrame(typeBodyEnd, mustID(id))
	}
	defer func() {
		if rc, ok := body.(io.Closer); ok {
			_ = rc.Close()
		}
	}()
	// The peer's advertised max_recv_frame constrains our encoder, so the
	// read buffer is sized from it rather than from bodyChunkSize alone.
	buf := make([]byte, c.maxSendChunk(id))
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nr, err := body.Read(buf)
		if nr > 0 {
			if capN-n < int64(nr) {
				return reject(ClassBodyTooLarge, "")
			}
			chunk, encErr := encodeBodyPayload(id, buf[:nr])
			if encErr != nil {
				return encErr
			}
			if werr := c.writeFrame(typeBody, chunk); werr != nil {
				return werr
			}
			n += int64(nr)
		}
		if err == io.EOF {
			return c.writeFrame(typeBodyEnd, mustID(id))
		}
		if err != nil {
			return err
		}
	}
}

func mustID(id string) []byte {
	b, err := encodeID(id)
	if err != nil {
		return []byte{0, 0}
	}
	return b
}

// Serve reads incoming requests until the stream or ctx ends.
func (c *Conn) Serve(ctx context.Context, h Handler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil {
		return reject(ClassMalformed, "")
	}
	if err := c.Handshake(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
	err := c.readLoop(ctx, true)
	c.mu.Lock()
	for id, fn := range c.cancels {
		fn()
		delete(c.cancels, id)
	}
	c.mu.Unlock()
	c.serveWG.Wait()
	return err
}

// Close marks the session closed and unblocks pending calls. If r or w
// implements io.Closer it is closed so the client read loop is not stuck
// in decodeFrame. If r is not a Closer, the transport owner must close
// the reader to release that loop.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.closeCh)
		c.failAll(io.ErrClosedPipe)
		if rc, ok := c.r.(io.Closer); ok {
			_ = rc.Close()
		}
		if wc, ok := c.w.(io.Closer); ok {
			_ = wc.Close()
		}
	})
	return nil
}

func (c *Conn) forgetCall(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cl := range c.pending {
		cl.settled = true
		select {
		case cl.respCh <- respOrErr{err: err}:
		default:
		}
		if cl.bodyW != nil {
			_ = cl.bodyW.CloseWithError(err)
		}
		delete(c.pending, id)
	}
	for id, inf := range c.inBody {
		_ = inf.w.CloseWithError(err)
		delete(c.inBody, id)
	}
}

func (c *Conn) ensureClientLoop() {
	c.loopOnce.Do(func() {
		go func() { _ = c.readLoop(context.Background(), false) }()
	})
}

func (c *Conn) readLoop(ctx context.Context, server bool) error {
	br := c.br
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if c.closed.Load() {
			return nil
		}
		fr, err := decodeFrame(br)
		if err != nil {
			if c.closed.Load() {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !server {
				c.failAll(err)
			}
			return err
		}
		if fr.typ == typeHello {
			// §2.2.2: exactly one hello per side per channel, and the
			// handshake already consumed it. A second one would be a way
			// to renegotiate mid-stream, which §2.2 does not have — a new
			// version needs a new channel.
			err := reject(ClassMalformed, "")
			if !server {
				c.failAll(err)
			}
			return err
		}
		if !isKnownType(fr.typ) {
			// tunnel-v2 §2.1. The frame was read whole, so the
			// stream stays aligned; dropping it is what makes a
			// MINOR addition invisible to an older computer rather
			// than a torn-down session.
			continue
		}
		if server {
			c.handleServerFrame(ctx, fr)
		} else {
			c.handleClientFrame(fr)
		}
	}
}

func (c *Conn) handleServerFrame(ctx context.Context, fr *frame) {
	switch fr.typ {
	case typeRequest:
		c.acceptRequest(ctx, fr.payload)
	case typeBody:
		c.acceptBody(fr.payload)
	case typeBodyEnd:
		c.acceptBodyEnd(fr.payload)
	case typeCancel:
		c.acceptCancel(fr.payload)
	}
}

type incoming struct {
	w   *io.PipeWriter
	n   int64
	cap int64
}

func (c *Conn) acceptRequest(ctx context.Context, payload []byte) {
	id, method, path, query, hdrs, err := decodeRequestPayload(payload)
	if err != nil {
		return
	}
	tmp := &Request{ID: id, Method: method, Path: path, Query: query, Headers: hdrs}
	hdrs, err = validateRequest(tmp)
	if err != nil {
		_ = c.writeReject(id, err)
		return
	}
	pr, pw := io.Pipe()
	c.mu.Lock()
	c.inBody[id] = &incoming{w: pw, cap: requestBodyLimit(method, path)}
	reqCtx, cancel := context.WithCancel(ctx)
	c.cancels[id] = cancel
	c.reqMeta[id] = reqMeta{method: method, path: path}
	h := c.handler
	c.mu.Unlock()

	req := &Request{
		ID:      id,
		Method:  method,
		Path:    path,
		Query:   query,
		Headers: hdrs,
		Body:    pr,
	}
	c.serveWG.Add(1)
	go func() {
		defer c.serveWG.Done()
		defer cancel()
		defer func() {
			c.mu.Lock()
			delete(c.cancels, id)
			delete(c.reqMeta, id)
			if inf, ok := c.inBody[id]; ok {
				_ = inf.w.Close()
				delete(c.inBody, id)
			}
			c.mu.Unlock()
		}()
		resp, herr := h.ServeRemote(reqCtx, req)
		_ = pr.Close()
		if herr != nil || resp == nil {
			return
		}
		resp.ID = id
		c.writeOutgoingResponse(reqCtx, id, method, path, resp)
	}()
}

func (c *Conn) acceptBody(payload []byte) {
	id, data, err := decodeIDPayload(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	inf := c.inBody[id]
	c.mu.Unlock()
	if inf == nil {
		return
	}
	if inf.cap-inf.n < int64(len(data)) {
		_ = inf.w.CloseWithError(reject(ClassBodyTooLarge, ""))
		_ = c.writeReject(id, reject(ClassBodyTooLarge, ""))
		return
	}
	inf.n += int64(len(data))
	_, _ = inf.w.Write(data)
}

func (c *Conn) acceptBodyEnd(payload []byte) {
	id, _, err := decodeIDPayload(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	inf, ok := c.inBody[id]
	if ok {
		delete(c.inBody, id)
	}
	c.mu.Unlock()
	if ok {
		_ = inf.w.Close()
	}
}

func (c *Conn) acceptCancel(payload []byte) {
	id, _, err := decodeIDPayload(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	fn := c.cancels[id]
	inf := c.inBody[id]
	if inf != nil {
		delete(c.inBody, id)
	}
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
	if inf != nil {
		_ = inf.w.CloseWithError(context.Canceled)
	}
}

func (c *Conn) handleClientFrame(fr *frame) {
	switch fr.typ {
	case typeResponse:
		c.acceptResponse(fr.payload)
	case typeBody:
		c.acceptClientBody(fr.payload)
	case typeBodyEnd:
		c.acceptClientBodyEnd(fr.payload)
	case typeReject:
		c.acceptReject(fr.payload)
	}
}

func (c *Conn) acceptResponse(payload []byte) {
	id, status, hdrs, err := decodeResponsePayload(payload)
	if err != nil {
		c.failCall(id, err)
		return
	}
	hdrs, err = validateResponseHeaders(hdrs)
	if err != nil {
		c.failCall(id, err)
		return
	}
	pr, pw := io.Pipe()
	c.mu.Lock()
	cl := c.pending[id]
	if cl == nil || cl.settled {
		c.mu.Unlock()
		_ = pw.Close()
		return
	}
	cl.bodyW = pw
	c.mu.Unlock()
	select {
	case cl.respCh <- respOrErr{resp: &Response{
		ID:      id,
		Status:  status,
		Headers: hdrs,
		Body:    pr,
	}}:
	default:
		_ = pw.Close()
	}
}

func (c *Conn) acceptClientBody(payload []byte) {
	id, data, err := decodeIDPayload(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	cl := c.pending[id]
	if cl == nil {
		c.mu.Unlock()
		return
	}
	bodyW := cl.bodyW
	if bodyW == nil {
		c.mu.Unlock()
		return
	}
	if cl.respCap-cl.n < int64(len(data)) {
		cl.settled = true
		c.mu.Unlock()
		_ = bodyW.CloseWithError(reject(ClassBodyTooLarge, ""))
		return
	}
	cl.n += int64(len(data))
	c.mu.Unlock()
	_, _ = bodyW.Write(data)
}

func (c *Conn) acceptClientBodyEnd(payload []byte) {
	id, _, err := decodeIDPayload(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	cl := c.pending[id]
	var bodyW *io.PipeWriter
	if cl != nil {
		bodyW = cl.bodyW
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if bodyW != nil {
		_ = bodyW.Close()
	}
}

func (c *Conn) acceptReject(payload []byte) {
	id, class, field, err := decodeRejectPayload(payload)
	if err != nil {
		return
	}
	if !knownRejectClass(class) {
		// tunnel-v2 §2.1 and §5: an unknown class is not an error. It
		// is an opaque refusal reason a later MINOR may have added,
		// and relabelling it "malformed" told the operator the frame
		// was corrupt when the peer had simply refused for a reason
		// this build predates — which would force every new class to
		// be a MAJOR bump.
		//
		// It is still peer-supplied text on its way into an error
		// message, so it is surfaced only in the shape a class may
		// take. Anything else is genuinely unparseable.
		if !plausibleRejectClass(class) {
			class = ClassMalformed
		}
		// The field's meaning is class-specific, so it means nothing
		// alongside a class this build does not know.
		field = ""
	}
	c.failCall(id, reject(class, field))
}

func (c *Conn) failCall(id string, err error) {
	c.mu.Lock()
	cl := c.pending[id]
	if cl == nil || cl.settled {
		c.mu.Unlock()
		return
	}
	cl.settled = true
	bodyW := cl.bodyW
	c.mu.Unlock()
	if bodyW != nil {
		_ = bodyW.CloseWithError(err)
		return
	}
	select {
	case cl.respCh <- respOrErr{err: err}:
	default:
	}
}

func (c *Conn) writeCancel(id string) error {
	return c.writeFrame(typeCancel, mustID(id))
}

func (c *Conn) writeReject(id string, err error) error {
	class := ClassMalformed
	field := ""
	var rej *RejectError
	if errors.As(err, &rej) {
		class = rej.Class
		field = rej.Field
	}
	p, encErr := encodeRejectPayload(id, class, field)
	if encErr != nil {
		return encErr
	}
	return c.writeFrame(typeReject, p)
}

func (c *Conn) writeOutgoingResponse(ctx context.Context, id, method, path string, resp *Response) {
	hdrs, err := validateResponseHeaders(resp.Headers)
	if err != nil {
		_ = c.writeReject(id, err)
		return
	}
	payload, err := encodeResponsePayload(id, resp.Status, hdrs)
	if err != nil {
		_ = c.writeReject(id, err)
		return
	}
	if err := c.writeFrame(typeResponse, payload); err != nil {
		return
	}
	capN := responseBodyLimit(method, path)
	body := resp.Body
	if body == nil {
		_ = c.writeFrame(typeBodyEnd, mustID(id))
		return
	}
	defer body.Close()
	buf := make([]byte, c.maxSendChunk(id))
	var n int64
	for {
		if ctx.Err() != nil {
			return
		}
		nr, rerr := body.Read(buf)
		if nr > 0 {
			if capN-n < int64(nr) {
				_ = c.writeReject(id, reject(ClassBodyTooLarge, ""))
				return
			}
			chunk, encErr := encodeBodyPayload(id, buf[:nr])
			if encErr != nil {
				return
			}
			if werr := c.writeFrame(typeBody, chunk); werr != nil {
				return
			}
			n += int64(nr)
		}
		if rerr == io.EOF {
			_ = c.writeFrame(typeBodyEnd, mustID(id))
			return
		}
		if rerr != nil {
			_ = c.writeReject(id, rerr)
			return
		}
	}
}
