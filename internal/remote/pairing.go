package remote

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
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
	// SignPub is the device's ed25519 identity. DevicePub is the P-256 Y
	// used for SAS and envelope seal; the two cannot share a field.
	SignPub []byte
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
	code       string
	rid        string
	expiresAt  time.Time
	consumed   bool
	contested  bool
	waiterOn   bool
	offer      *PairingOffer
	state      PairingUIState
	sas        string
	sasErr     error
	replyNonce []byte
	transcript []byte
}

// PairingStatus is the laptop view of one pairing code: FR-38 state plus
// the SAS derived from this session's own keys and transcript (protocol §11).
type PairingStatus struct {
	State      PairingUIState
	Code       string
	RID        string
	SAS        string
	ExpiresAt  time.Time
	LaptopPub  []byte
	ReplyNonce []byte
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
	cp.SignPub = append([]byte(nil), o.SignPub...)
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
	if _, err := r.ensureX(); err != nil {
		r.mu.Unlock()
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
	r.mu.Unlock()
	c.persistPairingX()
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
		s.sasErr = c.derivePairingSASLocked(r, s)
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
	s.sasErr = c.derivePairingSASLocked(r, s)
	return nil
}

// derivePairingSASLocked is protocol §11 on the live session: ECDH(X, Y)
// with this installation's X and the offer's Y, HKDF over this session's
// transcript. Caller holds r.mu. Failures are returned rather than
// leaving s.sas empty as something to confirm.
func (c *Client) derivePairingSASLocked(r *pairingRuntime, s *pairSession) error {
	if r == nil || s == nil || s.offer == nil {
		return classError(ClassUnauthorized, "sas", "pairing SAS was not derived")
	}
	if len(r.xPriv) == 0 {
		return classError(ClassUnauthorized, "sas", "pairing SAS was not derived: laptop pairing key is missing")
	}
	if len(s.offer.DevicePub) == 0 {
		return classError(ClassUnauthorized, "sas", "pairing SAS was not derived: device pairing public key is missing")
	}
	if len(s.replyNonce) == 0 {
		s.replyNonce = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, s.replyNonce); err != nil {
			return classErrorf(ClassUnauthorized, "sas", "pairing SAS was not derived", err)
		}
	}
	var install []byte
	if len(c.pub) > 0 {
		install = append([]byte(nil), c.pub...)
	}
	tr, err := BuildPairingTranscript(c.origin(), s.code, s.rid, r.xPub, s.offer.DevicePub, install, s.offer.OfferNonce, s.replyNonce)
	if err != nil {
		return classErrorf(ClassUnauthorized, "sas", "pairing SAS was not derived", err)
	}
	sas, err := DerivePairingSAS(r.xPriv, s.offer.DevicePub, tr)
	if err != nil {
		return classErrorf(ClassUnauthorized, "sas", "pairing SAS was not derived", err)
	}
	s.sas = sas
	s.transcript = tr
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
	out, live, err := c.completePairingSession(ctx, code, laptopConfirm, deviceConfirm)
	if err != nil {
		return PairedDevice{}, err
	}
	if err := c.adoptPairedDevice(live); err != nil {
		return PairedDevice{}, err
	}
	return out, nil
}

func (c *Client) completePairingSession(ctx context.Context, code string, laptopConfirm, deviceConfirm bool) (PairedDevice, DeviceRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return PairedDevice{}, DeviceRecord{}, err
		}
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	s, _, err := lookupSession(r, code, now)
	if err != nil {
		return PairedDevice{}, DeviceRecord{}, err
	}
	if s == nil || s.expired(now) {
		return PairedDevice{}, DeviceRecord{}, classError(ClassPairExpired, "complete", "pairing code has expired")
	}
	if s.consumed {
		return PairedDevice{}, DeviceRecord{}, classError(ClassPairConsumed, "complete", "pairing code has already been used")
	}
	if !laptopConfirm || !deviceConfirm {
		return PairedDevice{}, DeviceRecord{}, classError(ClassPairUnconfirmed, "complete", "both sides must confirm; SAS is never a password")
	}
	if s.contested || s.offer == nil {
		s.state = PairStateFailed
		return PairedDevice{}, DeviceRecord{}, fmt.Errorf("remote: pairing did not resolve to a single offer")
	}
	if s.sas == "" {
		s.state = PairStateFailed
		if s.sasErr != nil {
			return PairedDevice{}, DeviceRecord{}, s.sasErr
		}
		return PairedDevice{}, DeviceRecord{}, classError(ClassUnauthorized, "complete", "pairing SAS was not derived")
	}
	if len(s.offer.SignPub) != ed25519.PublicKeySize {
		return PairedDevice{}, DeviceRecord{}, classError(ClassUnauthorized, "complete", "device identity public key is missing")
	}
	id := s.offer.DeviceID
	if id == "" {
		id = s.rid
	}
	dev := PairedDevice{
		ID:       id,
		RID:      s.rid,
		Label:    s.offer.Label,
		PairedAt: now,
		PubKey:   append([]byte(nil), s.offer.DevicePub...),
	}
	live := DeviceRecord{
		ID:      id,
		RID:     s.rid,
		PubKey:  append([]byte(nil), s.offer.SignPub...),
		ECDHPub: append([]byte(nil), s.offer.DevicePub...),
	}
	s.consumed = true
	s.waiterOn = false
	s.state = PairStateSucceeded
	replaced := false
	for i, d := range r.devices {
		if d.ID == id {
			r.devices[i] = copyDevice(dev)
			replaced = true
			break
		}
	}
	if !replaced {
		r.devices = append(r.devices, copyDevice(dev))
	}
	return copyDevice(dev), live, nil
}

// adoptPairedDevice publishes a completed pairing onto the client's device
// registry so the wait loop will poll that RID. pairingRuntime.devices is
// the FR-38 list; c.devices is what session signalling consults.
func (c *Client) adoptPairedDevice(rec DeviceRecord) error {
	err := c.withStateLock("pair", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.loaded {
			if err := c.loadState(); err != nil {
				return err
			}
		}
		newRec := DeviceRecord{
			ID:      rec.ID,
			RID:     rec.RID,
			PubKey:  append([]byte(nil), rec.PubKey...),
			ECDHPub: append([]byte(nil), rec.ECDHPub...),
		}
		prevIdx := -1
		var previous DeviceRecord
		for i, d := range c.devices {
			if d.ID == rec.ID {
				previous = d
				prevIdx = i
				break
			}
		}
		if prevIdx >= 0 {
			if ch := c.channels[rec.ID]; ch != nil {
				_ = ch.Close()
				delete(c.channels, rec.ID)
			}
			if _, ok := c.pending[rec.ID]; ok {
				delete(c.pending, rec.ID)
				if c.hook().OnPendingErase != nil {
					c.hook().OnPendingErase(rec.ID)
				}
			}
			if previous.RID != "" {
				c.revoked[previous.RID] = struct{}{}
			}
			c.devEpoch[rec.ID]++
			c.devices[prevIdx] = newRec
		} else {
			c.devices = append(c.devices, newRec)
		}
		if c.st.Status == "" {
			return nil
		}
		if err := c.persist(c.snapshotState()); err != nil {
			if prevIdx >= 0 {
				if previous.RID != "" {
					delete(c.revoked, previous.RID)
				}
				c.devices[prevIdx] = previous
			} else {
				c.devices = c.devices[:len(c.devices)-1]
			}
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.kickWaiters()
	return nil
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
	s.sas = ""
	s.sasErr = nil
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

// PairingSession is the current FR-38 state plus SAS material for a code.
func (c *Client) PairingSession(code string) (PairingStatus, error) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	s, _, err := lookupSession(r, code, c.pairingNow())
	if err != nil {
		return PairingStatus{}, err
	}
	if s == nil {
		return PairingStatus{}, classError(ClassNotFound, "pair", "unknown pairing code")
	}
	return PairingStatus{
		State:      s.state,
		Code:       s.code,
		RID:        s.rid,
		SAS:        s.sas,
		ExpiresAt:  s.expiresAt,
		LaptopPub:  append([]byte(nil), r.xPub...),
		ReplyNonce: append([]byte(nil), s.replyNonce...),
	}, nil
}

// RevokePairedDevice removes a completed pairing from the device list.
func (c *Client) RevokePairedDevice(ctx context.Context, id string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	r := pairingOf(c)
	r.mu.Lock()
	prev := make([]PairedDevice, len(r.devices))
	for i, d := range r.devices {
		prev[i] = copyDevice(d)
	}
	kept := r.devices[:0]
	found := false
	for _, d := range r.devices {
		if d.ID == id {
			found = true
			continue
		}
		kept = append(kept, copyDevice(d))
	}
	if !found {
		r.mu.Unlock()
		return classError(ClassNotFound, "revoke", "paired device not found")
	}
	r.devices = kept
	r.mu.Unlock()
	if err := c.RevokeDevice(ctx, id); err != nil {
		r.mu.Lock()
		r.devices = prev
		r.mu.Unlock()
		return err
	}
	return nil
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
	created := len(r.xPub) == 0
	pub, err := r.ensureX()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if created {
		c.persistPairingX()
	}
	return pub, nil
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
