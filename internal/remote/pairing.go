package remote

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
)

// PairingTTL is FR-11: 60-second lifetime of a pairing code and its
// temporary waiter, measured from first registration.
const (
	PairingTTL              = 60 * time.Second
	PairingCodeCrockfordLen = 8
	pairingCodeEntropyBytes = 5
)

const (
	ClassPairExpired     Class = "pair-expired"
	ClassPairConsumed    Class = "pair-consumed"
	ClassPairUnconfirmed Class = "pair-unconfirmed"
	ClassPairRole        Class = "pair-role"
	ClassPairReflection  Class = "pair-reflection"
)

const pairingTranscriptLabel = "scimux-rv/pair/v1"
const pairingSASSalt = "scimux-rv/sas/v1"

// PairingUIState is one FR-38 state. Tests match on the constant.
type PairingUIState string

const (
	PairStateWarning   PairingUIState = "authority-warning"
	PairStatePending   PairingUIState = "pending"
	PairStateExpired   PairingUIState = "expired"
	PairStateCancelled PairingUIState = "cancelled"
	PairStateFailed    PairingUIState = "failed"
	PairStateSucceeded PairingUIState = "succeeded"
)

// PairingCode is one minted short code plus its temporary rendezvous ID.
type PairingCode struct {
	Code      string
	RID       string
	ExpiresAt time.Time
}

// PairingOffer is one device-side pair/offer. The laptop decides
// legitimacy from key material and envelope contents, never from a
// caller-supplied hostility flag.
type PairingOffer struct {
	Code       string
	DeviceID   string
	Label      string
	DevicePub  []byte
	OfferNonce []byte
	Envelope   []byte
}

// PairedDevice is a completed pairing record: stable label and paired-at.
type PairedDevice struct {
	ID       string
	RID      string
	Label    string
	PairedAt time.Time
	PubKey   []byte
}

type pairingRuntime struct {
	mu       sync.Mutex
	xPriv    []byte
	xPub     []byte
	sessions map[string]*pairSession
	devices  []PairedDevice
}

type pairSession struct {
	code      string
	rid       string
	expiresAt time.Time
	consumed  bool
	contested bool
	waiterOn  bool
	offer     *PairingOffer
	state     PairingUIState
}

// pairingOf returns the client's own pairing runtime. State belongs to the
// client, not to a package-level registry keyed by client pointer: such a
// registry never drops an entry, so it would pin every *Client ever created —
// and its sessions and paired devices — for the life of the process.
//
// NewClient allocates the runtime; the sync.Once covers a zero-value Client.
// A nil client has nowhere to keep pairing state, so it panics here exactly as
// it already would at c.rand() or c.cfg — pairing is not nil-tolerant.
func pairingOf(c *Client) *pairingRuntime {
	c.pairingOnce.Do(func() {
		if c.pairing == nil {
			c.pairing = newPairingRuntime()
		}
	})
	return c.pairing
}

func newPairingRuntime() *pairingRuntime {
	return &pairingRuntime{sessions: map[string]*pairSession{}}
}

func (c *Client) pairingNow() time.Time {
	if c != nil && c.cfg.Clock != nil {
		return c.cfg.Clock.Now()
	}
	return time.Now()
}

func (r *pairingRuntime) ensureX() ([]byte, error) {
	if len(r.xPub) > 0 {
		return append([]byte(nil), r.xPub...), nil
	}
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	r.xPriv = k.Bytes()
	r.xPub = k.PublicKey().Bytes()
	return append([]byte(nil), r.xPub...), nil
}

func (s *pairSession) expired(now time.Time) bool {
	if s == nil {
		return false
	}
	return !now.Before(s.expiresAt)
}

func (s *pairSession) applyExpiry(now time.Time) {
	if s == nil || !s.expired(now) {
		return
	}
	s.waiterOn = false
	if s.state != PairStateSucceeded {
		s.state = PairStateExpired
	}
}

func lookupSession(r *pairingRuntime, code string, now time.Time) (*pairSession, string, error) {
	norm, err := normalizePairingCode(code)
	if err != nil {
		return nil, "", classError(ClassPairExpired, "pair", "pairing code is not valid")
	}
	s := r.sessions[norm]
	if s != nil {
		s.applyExpiry(now)
	}
	return s, norm, nil
}

func copyOffer(o PairingOffer) *PairingOffer {
	cp := o
	cp.DevicePub = append([]byte(nil), o.DevicePub...)
	cp.OfferNonce = append([]byte(nil), o.OfferNonce...)
	cp.Envelope = append([]byte(nil), o.Envelope...)
	return &cp
}

func pubsEqual(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && bytes.Equal(a, b)
}

func copyDevice(d PairedDevice) PairedDevice {
	d.PubKey = append([]byte(nil), d.PubKey...)
	return d
}

// MintPairingCode mints an 8-character pairing code with PairingTTL and a
// client-minted temporary rendezvous ID (FR-11).
func (c *Client) MintPairingCode(ctx context.Context) (PairingCode, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return PairingCode{}, err
		}
	}
	ent := make([]byte, pairingCodeEntropyBytes)
	if _, err := io.ReadFull(c.rand(), ent); err != nil {
		return PairingCode{}, err
	}
	code := encodeCrockford(ent)
	if len(code) != PairingCodeCrockfordLen {
		return PairingCode{}, fmt.Errorf("remote: pairing code length %d", len(code))
	}
	rid, err := mintRID(c.rand(), nil)
	if err != nil {
		return PairingCode{}, err
	}
	now := c.pairingNow()
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.ensureX(); err != nil {
		return PairingCode{}, err
	}
	exp := now.Add(PairingTTL)
	r.sessions[code] = &pairSession{
		code:      code,
		rid:       rid,
		expiresAt: exp,
		waiterOn:  true,
		state:     PairStatePending,
	}
	return PairingCode{Code: code, RID: rid, ExpiresAt: exp}, nil
}

// AcceptPairingOffer is the laptop receiving a pair/offer. An offer does
// not consume the code (protocol §10.6).
func (c *Client) AcceptPairingOffer(ctx context.Context, offer PairingOffer) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	s, _, err := lookupSession(r, offer.Code, now)
	if err != nil {
		return err
	}
	if s == nil || s.expired(now) {
		return classError(ClassPairExpired, "offer", "pairing code has expired")
	}
	if s.consumed {
		return classError(ClassPairConsumed, "offer", "pairing code has already been used")
	}

	x, err := r.ensureX()
	if err != nil {
		return err
	}
	if class := offerAttackClass(offer, x); class != "" {
		s.state = PairStateFailed
		return classError(class, "offer", "pairing offer rejected")
	}

	if s.offer != nil && pubsEqual(s.offer.DevicePub, offer.DevicePub) {
		s.waiterOn = true
		s.state = PairStatePending
		return nil
	}
	if s.offer != nil && (len(s.offer.DevicePub) > 0 || len(offer.DevicePub) > 0) && !bytes.Equal(s.offer.DevicePub, offer.DevicePub) {
		s.contested = true
		s.state = PairStateFailed
		return fmt.Errorf("remote: pairing offer conflict")
	}
	s.offer = copyOffer(offer)
	s.waiterOn = true
	s.state = PairStatePending
	return nil
}

func offerAttackClass(offer PairingOffer, x []byte) Class {
	if len(offer.Envelope) > 0 {
		var env struct {
			Type       string `json:"type"`
			InstallPub string `json:"install_pub"`
		}
		if json.Unmarshal(offer.Envelope, &env) == nil {
			if env.Type == "pair-reply" {
				return ClassPairRole
			}
			if env.InstallPub != "" && len(x) > 0 && strings.EqualFold(env.InstallPub, hex.EncodeToString(x)) {
				return ClassPairRole
			}
		}
	}
	if pubsEqual(offer.DevicePub, x) {
		return ClassPairReflection
	}
	return ""
}

// FR-12's two-sided confirmation is carried by CompletePairing's laptopConfirm
// and deviceConfirm arguments — the caller (the pairing UI) owns the two human
// acts, and CompletePairing fails closed with ClassPairUnconfirmed unless it
// has both. There is deliberately no ConfirmPairing(code, side) method: one
// that recorded a side without CompletePairing depending on it would be
// decorative, and one that CompletePairing *required* would contradict
// AT-FR-12-c, which completes a pairing having called nothing but
// CompletePairing(ctx, code, true, true). Do not reintroduce it as a
// convenience wrapper; it reads like the confirmation channel and is not one.

// CompletePairing finishes pairing only when both sides confirm.
// Withholding either confirmation fails closed with ClassPairUnconfirmed
// and does not persist a device.
func (c *Client) CompletePairing(ctx context.Context, code string, laptopConfirm, deviceConfirm bool) (PairedDevice, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return PairedDevice{}, err
		}
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	s, _, err := lookupSession(r, code, now)
	if err != nil {
		return PairedDevice{}, err
	}
	if s == nil || s.expired(now) {
		return PairedDevice{}, classError(ClassPairExpired, "complete", "pairing code has expired")
	}
	if s.consumed {
		return PairedDevice{}, classError(ClassPairConsumed, "complete", "pairing code has already been used")
	}
	if !laptopConfirm || !deviceConfirm {
		return PairedDevice{}, classError(ClassPairUnconfirmed, "complete", "both sides must confirm; SAS is never a password")
	}
	if s.contested || s.offer == nil {
		s.state = PairStateFailed
		return PairedDevice{}, fmt.Errorf("remote: pairing did not resolve to a single offer")
	}
	id := s.offer.DeviceID
	if id == "" {
		id = s.rid
	}
	for _, d := range r.devices {
		if d.ID == id {
			id = s.rid
			break
		}
	}
	dev := PairedDevice{
		ID:       id,
		RID:      s.rid,
		Label:    s.offer.Label,
		PairedAt: now,
		PubKey:   append([]byte(nil), s.offer.DevicePub...),
	}
	s.consumed = true
	s.waiterOn = false
	s.state = PairStateSucceeded
	r.devices = append(r.devices, copyDevice(dev))
	return copyDevice(dev), nil
}

// CancelPairing tears down the temporary waiter without consuming the
// code (FR-38, protocol §10.4).
func (c *Client) CancelPairing(ctx context.Context, code string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	s, _, err := lookupSession(r, code, now)
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	if s.expired(now) {
		return classError(ClassPairExpired, "cancel", "pairing code has expired")
	}
	s.waiterOn = false
	s.offer = nil
	s.contested = false
	if !s.consumed {
		s.state = PairStateCancelled
	}
	return nil
}

// PairingConsumed reports whether the code has passed the single-use
// transition (reply or TTL — not offer, not cancel).
func (c *Client) PairingConsumed(code string) (bool, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	s, _, err := lookupSession(r, code, c.pairingNow())
	if err != nil {
		return false, err
	}
	if s == nil {
		return false, nil
	}
	return s.consumed || s.expired(c.pairingNow()), nil
}

// PairingWaiterRegistered reports whether the temporary RID still has a
// waiter.
func (c *Client) PairingWaiterRegistered(rid string) (bool, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	for _, s := range r.sessions {
		s.applyExpiry(now)
		if s.rid == rid {
			return s.waiterOn && !s.expired(now), nil
		}
	}
	return false, nil
}

// PairingState is the current FR-38 UI state for a code.
func (c *Client) PairingState(code string) (PairingUIState, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	s, _, err := lookupSession(r, code, c.pairingNow())
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", classError(ClassNotFound, "pair", "unknown pairing code")
	}
	return s.state, nil
}

// PairedDevices lists completed pairings with stable labels and paired-at.
func (c *Client) PairedDevices() ([]PairedDevice, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PairedDevice, len(r.devices))
	for i, d := range r.devices {
		out[i] = copyDevice(d)
	}
	return out, nil
}

// PairingECDHPublic is the laptop static P-256 public key X used in
// the pairing transcript (protocol §11).
func (c *Client) PairingECDHPublic() ([]byte, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensureX()
}

// BuildPairingTranscript is protocol §11.
func BuildPairingTranscript(origin, code, rid string, x, y, installPub, offerNonce, replyNonce []byte) ([]byte, error) {
	norm, err := normalizePairingCode(code)
	if err != nil {
		norm = code
	}
	parts := [][]byte{
		[]byte(pairingTranscriptLabel),
		[]byte(origin),
		[]byte(norm),
		[]byte(rid),
		x,
		y,
		installPub,
		offerNonce,
		replyNonce,
	}
	var b []byte
	for i, p := range parts {
		if i > 0 {
			b = append(b, 0x00)
		}
		b = append(b, p...)
	}
	return b, nil
}

// DerivePairingSAS is protocol §11. Never a password, never auto-confirmed.
func DerivePairingSAS(localPriv, peerPub, transcript []byte) (string, error) {
	curve := ecdh.P256()
	priv, err := curve.NewPrivateKey(localPriv)
	if err != nil {
		return "", err
	}
	pub, err := curve.NewPublicKey(peerPub)
	if err != nil {
		return "", err
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return "", err
	}
	key, err := hkdf.Key(sha256.New, shared, []byte(pairingSASSalt), string(transcript), 4)
	if err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(key)
	return fmt.Sprintf("%06d", n%1000000), nil
}

func normalizePairingCode(s string) (string, error) {
	var b strings.Builder
	b.Grow(PairingCodeCrockfordLen)
	for _, r := range s {
		switch r {
		case '-', ' ':
			continue
		case 'o', 'O':
			r = '0'
		case 'i', 'I', 'l', 'L':
			r = '1'
		default:
			r = unicode.ToUpper(r)
		}
		if strings.IndexRune(inviteAlphabet, r) < 0 {
			return "", fmt.Errorf("remote: pairing code has invalid character")
		}
		b.WriteRune(r)
	}
	if b.Len() != PairingCodeCrockfordLen {
		return "", fmt.Errorf("remote: pairing code length %d", b.Len())
	}
	return b.String(), nil
}

func encodeCrockford(src []byte) string {
	var acc uint64
	var bits uint
	out := make([]byte, 0, PairingCodeCrockfordLen)
	for _, b := range src {
		acc = (acc << 8) | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, inviteAlphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		out = append(out, inviteAlphabet[(acc<<(5-bits))&31])
	}
	return string(out)
}
