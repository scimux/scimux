package codec

import (
	"context"
	"time"
)

// The hello frame (tunnel-v2 §2.2.2, §5.1).
//
// Exactly one per side per channel, immediately after the preamble and
// before anything else. The preamble says which protocol this is; the
// hello says what this particular peer can do. Everything that grows
// grows here, as records, so a MINOR bump is a new tag an older peer
// steps over.
const (
	// RoleInitiator is the side that sends requests (the browser).
	RoleInitiator uint8 = 0
	// RoleResponder is the side that serves them (the laptop).
	RoleResponder uint8 = 1
)

// handshakeDeadline bounds negotiation only. It is the one place in this
// protocol a clock appears: there is nothing yet to cancel, and a silent
// peer is otherwise indistinguishable from one that will never speak.
const handshakeDeadline = 10 * time.Second

// Hello is one peer's advertisement.
type Hello struct {
	// Role is RoleInitiator or RoleResponder. Two peers in the same role
	// is a wiring bug that would otherwise present as silence.
	Role uint8
	// MaxRecvFrame is the largest frame payload this peer will accept. It
	// constrains the *peer's* encoder, never our own: a value above
	// maxFramePayload does not raise this side's limit. Absent means
	// maxFramePayload.
	MaxRecvFrame uint32
	// Impl is free diagnostic text. It MUST NOT be parsed for behaviour —
	// capabilities are tags, not version strings.
	Impl string
}

func validRole(r uint8) bool {
	return r == RoleInitiator || r == RoleResponder
}

func encodeHelloPayload(h Hello) ([]byte, error) {
	if !validRole(h.Role) {
		return nil, reject(ClassMalformed, "")
	}
	b, err := putRecordU8(nil, tagRole, h.Role)
	if err != nil {
		return nil, err
	}
	if b, err = putRecordU32(b, tagMaxRecvFrame, h.MaxRecvFrame); err != nil {
		return nil, err
	}
	return putRecordString(b, tagImpl, h.Impl)
}

func decodeHelloPayload(p []byte) (Hello, error) {
	h := Hello{Role: 0xff, MaxRecvFrame: maxFramePayload}
	err := eachRecord(p, func(tag uint8, val []byte) error {
		switch tag {
		case tagRole:
			r, rErr := recordU8(val)
			if rErr != nil {
				return rErr
			}
			h.Role = r
		case tagMaxRecvFrame:
			n, nErr := recordU32(val)
			if nErr != nil {
				return nErr
			}
			h.MaxRecvFrame = n
		case tagImpl:
			h.Impl = string(val)
		}
		return nil
	})
	if err != nil {
		return Hello{}, err
	}
	if !validRole(h.Role) {
		return Hello{}, reject(ClassMalformed, "")
	}
	return h, nil
}

// Handshake writes this side's preamble and hello and reads the peer's.
// It is idempotent: RoundTrip and Serve both call it, and repeat calls
// return the first result without writing again.
//
// The read is bounded by handshakeDeadline as well as by ctx: a peer that
// never speaks must become a named version failure rather than a hang.
func (c *Conn) Handshake(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.handshakeOnce.Do(func() {
		c.handshakeErr = c.handshake(ctx)
	})
	return c.handshakeErr
}

func (c *Conn) handshake(ctx context.Context) error {
	payload, err := encodeHelloPayload(Hello{
		Role:         c.role,
		MaxRecvFrame: maxFramePayload,
		Impl:         c.Impl,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, handshakeDeadline)
	defer cancel()

	type result struct {
		hello Hello
		err   error
	}
	done := make(chan result, 1)
	// The reader starts before the write. Both sides send unprompted
	// (§2.2), so over an unbuffered transport — an io.Pipe, or a data
	// channel with no slack — a peer that wrote first and read second
	// would block against a peer doing exactly the same thing.
	go func() {
		h, err := c.readPeerHandshake()
		done <- result{h, err}
	}()

	// Ours next, unprompted. There is no round trip: a peer that is
	// about to refuse our major still gets our preamble to name it by.
	if err := c.writeRaw(encodePreamble()); err != nil {
		return err
	}
	if err := c.writeFrame(typeHello, payload); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		// Silence is indistinguishable from a peer that will never speak,
		// and there is nothing yet to cancel. It reads as "not a tunnel
		// peer", which is what the FR-24 state has to say.
		return &VersionError{Reason: VersionNoPreamble}
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		if r.hello.Role == c.role {
			// Two peers in the same role is a wiring bug that would
			// otherwise present as a session where nobody ever answers.
			return reject(ClassMalformed, "")
		}
		c.peerHello = r.hello
		c.havePeerHello.Store(true)
		return nil
	}
}

func (c *Conn) readPeerHandshake() (Hello, error) {
	if _, _, err := decodePreamble(c.br); err != nil {
		return Hello{}, err
	}
	fr, err := decodeFrame(c.br)
	if err != nil {
		return Hello{}, err
	}
	if fr.typ != typeHello {
		// §2.2.2: the hello is the first frame, full stop. Accepting a
		// request here would defeat the point of negotiating before one.
		return Hello{}, reject(ClassMalformed, "")
	}
	return decodeHelloPayload(fr.payload)
}

// PeerHello returns the peer's hello, once the handshake has succeeded.
func (c *Conn) PeerHello() (Hello, bool) {
	if !c.havePeerHello.Load() {
		return Hello{}, false
	}
	return c.peerHello, true
}

// maxSendFrame is the largest payload this side may put in a frame:
// min(maxFramePayload, peer's advertised MaxRecvFrame). A peer may lower
// our frame size; it may never raise it.
func (c *Conn) maxSendFrame() int {
	n := maxFramePayload
	if h, ok := c.PeerHello(); ok && h.MaxRecvFrame > 0 && int(h.MaxRecvFrame) < n {
		n = int(h.MaxRecvFrame)
	}
	return n
}

// maxSendChunk is maxSendFrame less the record overhead a body frame adds
// around the data: an id record plus the data record's own header.
func (c *Conn) maxSendChunk(id string) int {
	n := c.maxSendFrame() - 2*recordHeaderSize - len(id)
	if n > bodyChunkSize {
		n = bodyChunkSize
	}
	if n < 1 {
		n = 1
	}
	return n
}
