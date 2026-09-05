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

// PairingTTL is FR-11: 120-second lifetime of a pairing code and its
// temporary waiter, measured from first registration. It matches rv's
// PairingWaitTTL and must keep matching it: the window is sized for a
// person unlocking a phone and comparing six digits across two
// screens, not for the protocol. See §10.2 of rendezvous-v1.md for why
// widening it is cheap.
const (
	PairingTTL              = 120 * time.Second
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

// PairingOffer is one device-side pair/offer. The computer decides
// legitimacy from key material and envelope contents, never from a
// caller-supplied hostility flag.
type PairingOffer struct {
	Code       string
	DeviceID   string
	Label      string
	DevicePub  []byte
	OfferNonce []byte
	Envelope   []byte
	// SignPub is an optional legacy Ed25519 extension. The published browser
	// offer uses DevicePub, the P-256 Y used for SAS and envelope sealing.
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
	// completeMu serializes whole CompletePairing calls. The single-use
	// check reads s.consumed in prepare and sets it in commit, with an
	// adopt in between that must not hold mu (it persists), so without
	// this two callers both pass the check before either sets the flag.
	//
	// It is a lock, not a lifecycle flag: S7f removed pairSession.reserved
	// precisely because a caller that died between set and clear left the
	// session unusable forever. A mutex released by defer cannot do that.
	// Lock order is completeMu -> mu -> state lock, never the reverse;
	// nothing else acquires completeMu.
	completeMu sync.Mutex

	mu       sync.Mutex
	xPriv    []byte
	xPub     []byte
	xOnDisk  bool // xPriv/xPub have been written to the identity file
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

// PairingStatus is the computer view of one pairing code: FR-38 state plus
// the SAS derived from this session's own keys and transcript (protocol §11).
type PairingStatus struct {
	State       PairingUIState
	Code        string
	RID         string
	SAS         string
	ExpiresAt   time.Time
	ComputerPub []byte
	ReplyNonce  []byte
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
	if err := c.ensureDurableX(); err != nil {
		return PairingCode{}, err
	}
	exp := now.Add(PairingTTL)
	r := pairingOf(c)
	r.mu.Lock()
	r.sessions[code] = &pairSession{
		code:      code,
		rid:       rid,
		expiresAt: exp,
		waiterOn:  true,
		state:     PairStatePending,
	}
	r.mu.Unlock()
	c.kickWaiters()
	return PairingCode{Code: code, RID: rid, ExpiresAt: exp}, nil
}

// AcceptPairingOffer is the computer receiving a pair/offer. An offer does
// not consume the code (protocol §10.6).
func (c *Client) AcceptPairingOffer(ctx context.Context, offer PairingOffer) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	// The device names itself, so the name is bounded and stripped here,
	// at the door, rather than wherever it is later displayed. Everything
	// downstream reads s.offer.Label.
	offer.Label = sanitizeDeviceLabel(offer.Label)
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
	if len(offer.SignPub) != 0 && len(offer.SignPub) != ed25519.PublicKeySize {
		return classError(ClassHandshake, "offer", "device_sign_pub has the wrong length")
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
		return classError(ClassUnauthorized, "sas", "pairing SAS was not derived: computer pairing key is missing")
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

// FR-12's two-sided confirmation is carried by CompletePairing's computerConfirm
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
func (c *Client) CompletePairing(ctx context.Context, code string, computerConfirm, deviceConfirm bool) (PairedDevice, error) {
	// Prepare checks single-use and commit records it; hold the pairing
	// runtime's completion lock across both so the two cannot interleave
	// (AT-FR-39-c under concurrency).
	r := pairingOf(c)
	r.completeMu.Lock()
	defer r.completeMu.Unlock()

	out, live, err := c.preparePairingComplete(ctx, code, computerConfirm, deviceConfirm)
	if err != nil {
		return PairedDevice{}, err
	}
	if err := c.adoptPairedDevice(live); err != nil {
		return PairedDevice{}, err
	}
	if err := c.commitPairingComplete(code, out); err != nil {
		_ = c.unadoptPairedDevice(live)
		return PairedDevice{}, err
	}
	c.kickWaiters()
	return out, nil
}

func (c *Client) preparePairingComplete(ctx context.Context, code string, computerConfirm, deviceConfirm bool) (PairedDevice, DeviceRecord, error) {
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
	if !computerConfirm || !deviceConfirm {
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
		ID:       id,
		RID:      s.rid,
		PubKey:   append([]byte(nil), s.offer.SignPub...),
		ECDHPub:  append([]byte(nil), s.offer.DevicePub...),
		Label:    dev.Label,
		PairedAt: dev.PairedAt,
	}
	return copyDevice(dev), live, nil
}

// commitPairingComplete is the pairing-session half of CompletePairing, run
// only after the live device record has been persisted. Validation belongs
// in prepare; commit is pure mutation and does not re-check expiry or
// consumption. That is what holds the persist-race line: an expiry that
// lands during adopt cannot fail this half, because commit does not look
// at the clock. A persist failure of adopt leaves the code unconsumed so
// the operator can retry.
func (c *Client) commitPairingComplete(code string, dev PairedDevice) error {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	norm, err := normalizePairingCode(code)
	if err != nil {
		norm = code
	}
	if s := r.sessions[norm]; s != nil {
		s.consumed = true
		s.waiterOn = false
		s.state = PairStateSucceeded
	}
	replaced := false
	for i, d := range r.devices {
		if d.ID == dev.ID {
			r.devices[i] = copyDevice(dev)
			replaced = true
			break
		}
	}
	if !replaced {
		r.devices = append(r.devices, copyDevice(dev))
	}
	return nil
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
			ID:       rec.ID,
			RID:      rec.RID,
			PubKey:   append([]byte(nil), rec.PubKey...),
			ECDHPub:  append([]byte(nil), rec.ECDHPub...),
			Label:    rec.Label,
			PairedAt: rec.PairedAt,
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

// unadoptPairedDevice drops a just-adopted live record so a failed commit
// cannot leave a device the FR-38 list never recorded.
//
// Deliberate undo path: commitPairingComplete currently always returns
// nil, so CompletePairing cannot reach this. It is kept for a commit that
// may later be able to fail.
func (c *Client) unadoptPairedDevice(rec DeviceRecord) error {
	err := c.withStateLock("pair", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		kept := c.devices[:0]
		for _, d := range c.devices {
			if d.ID == rec.ID && d.RID == rec.RID {
				if rec.RID != "" {
					delete(c.revoked, rec.RID)
				}
				continue
			}
			kept = append(kept, d)
		}
		c.devices = kept
		if c.st.Status == "" {
			return nil
		}
		return c.persist(c.snapshotState())
	})
	if err == nil {
		c.kickWaiters()
	}
	return err
}

// CancelPairing tears down the temporary waiter without consuming the
// code (FR-38, protocol §10.4).
func (c *Client) CancelPairing(ctx context.Context, code string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	defer c.kickWaiters()
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
		State:       s.state,
		Code:        s.code,
		RID:         s.rid,
		SAS:         s.sas,
		ExpiresAt:   s.expiresAt,
		ComputerPub: append([]byte(nil), r.xPub...),
		ReplyNonce:  append([]byte(nil), s.replyNonce...),
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

// maxDeviceLabel bounds every label this computer stores, whoever supplied
// it. The device's own name for itself arrives in the pair-offer and is a
// claim, not a fact: nothing stops a phone from sending a kilobyte, or a
// newline, or the name of the laptop next to it. The operator's own typing
// is bounded for the duller reason that a row has to stay a row.
const maxDeviceLabel = 64

// sanitizeDeviceLabel makes a label safe to put in a list a human reads to
// decide what to revoke. Control characters go (a label is one line, and a
// line the device chose must not be able to move the cursor), surrounding
// space goes, and what is left is cut to maxDeviceLabel runes — runes, so
// a cut never lands inside a character.
func sanitizeDeviceLabel(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		if n == maxDeviceLabel {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// RenamePairedDevice sets the operator's own name for a paired device.
//
// The label is the only thing it touches. The ID, the RID, the keys and
// the paired-at are what signalling and sealing read, and a rename that
// disturbed any of them would end the pairing while claiming to have
// renamed it. An empty name is a clear rather than an error: the row then
// falls back to the device's identity, which is the only name that was
// ever a fact.
func (c *Client) RenamePairedDevice(ctx context.Context, id, label string) (PairedDevice, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return PairedDevice{}, err
		}
	}
	name := sanitizeDeviceLabel(label)
	r := pairingOf(c)
	r.mu.Lock()
	idx := -1
	for i, d := range r.devices {
		if d.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		return PairedDevice{}, classError(ClassNotFound, "rename", "paired device not found")
	}
	prev := r.devices[idx].Label
	r.devices[idx].Label = name
	out := copyDevice(r.devices[idx])
	r.mu.Unlock()
	// The durable half decides. A rename the identity file rejected is a
	// rename that would vanish at the next restart, so the list is put back
	// rather than left showing a name nothing on disk agrees with.
	if err := c.relabelDevice(ctx, id, name); err != nil {
		r.mu.Lock()
		for i := range r.devices {
			if r.devices[i].ID == id {
				r.devices[i].Label = prev
				break
			}
		}
		r.mu.Unlock()
		return PairedDevice{}, err
	}
	return out, nil
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

// setPairedDevices replaces the FR-38 list wholesale. It is the load path's
// half of the list: applyState rebuilds it from the identity file so a
// restart shows the pairings the wait loop is already polling. Callers hold
// c.mu; the lock order c.mu -> pairingRuntime.mu is the one applyState
// already takes through applyPairingXFromDisk.
func (c *Client) setPairedDevices(list []PairedDevice) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices = make([]PairedDevice, 0, len(list))
	for _, d := range list {
		r.devices = append(r.devices, copyDevice(d))
	}
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

// PairingECDHPublic is the computer static P-256 public key X used in
// the pairing transcript (protocol §11).
func (c *Client) PairingECDHPublic() ([]byte, error) {
	if err := c.ensureDurableX(); err != nil {
		return nil, err
	}
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.xPub...), nil
}

const (
	pairOfferType = "pair-offer"
	pairReplyType = "pair-reply"
)

type pairingInner struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	DevicePub  string `json:"device_pub,omitempty"`
	SignPub    string `json:"device_sign_pub,omitempty"`
	OfferNonce string `json:"offer_nonce,omitempty"`
	ReplyNonce string `json:"reply_nonce,omitempty"`
	InstallPub string `json:"install_pub,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	Label      string `json:"label,omitempty"`
}

func (c *Client) pairingWaiter(rid string) (string, bool) {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	for _, s := range r.sessions {
		s.applyExpiry(now)
		if s.rid == rid && s.waiterOn && !s.expired(now) {
			return s.code, true
		}
	}
	return "", false
}

func (c *Client) pairingLiveRIDs() map[string]bool {
	r := pairingOf(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := c.pairingNow()
	out := map[string]bool{}
	for _, s := range r.sessions {
		s.applyExpiry(now)
		if s.waiterOn && !s.expired(now) && validRID(s.rid) {
			out[s.rid] = true
		}
	}
	return out
}

// answerPairingEnvelope opens a sealed pair/offer, feeds AcceptPairingOffer,
// and seals a pair-reply to the device's Y. An unopenable or rejected offer
// returns an error and no blob so the wait loop sends nothing and does not
// consume the code (§10.6).
func (c *Client) answerPairingEnvelope(ctx context.Context, rid string, sealed []byte) ([]byte, error) {
	priv, err := c.sessionKeyPriv()
	if err != nil {
		return nil, err
	}
	origin := c.origin()
	plain, err := openEnvelopeBytes(sealed, priv, origin, rid)
	if err != nil {
		return nil, err
	}
	var inner pairingInner
	if err := json.Unmarshal(plain, &inner); err != nil {
		return nil, classErrorf(ClassHandshake, "pair", "pairing offer is not JSON", err)
	}
	if inner.Type != pairOfferType {
		return nil, classError(ClassHandshake, "pair", "envelope is not a pair-offer")
	}
	code, ok := c.pairingWaiter(rid)
	if !ok {
		return nil, classError(ClassPairExpired, "pair", "pairing waiter is not registered")
	}
	y, err := hex.DecodeString(inner.DevicePub)
	if err != nil || len(y) == 0 {
		return nil, classError(ClassHandshake, "pair", "pair-offer device_pub is missing")
	}
	var sign []byte
	if inner.SignPub != "" {
		sign, err = hex.DecodeString(inner.SignPub)
		if err != nil {
			return nil, classError(ClassHandshake, "pair", "pair-offer device_sign_pub is not hex")
		}
	}
	var offerN []byte
	if inner.OfferNonce != "" {
		offerN, err = hex.DecodeString(inner.OfferNonce)
		if err != nil {
			return nil, classError(ClassHandshake, "pair", "pair-offer offer_nonce is not hex")
		}
	}
	if err := c.AcceptPairingOffer(ctx, PairingOffer{
		Code:       code,
		DeviceID:   inner.DeviceID,
		Label:      inner.Label,
		DevicePub:  y,
		SignPub:    sign,
		OfferNonce: offerN,
		Envelope:   append([]byte(nil), plain...),
	}); err != nil {
		return nil, err
	}
	st, err := c.PairingSession(code)
	if err != nil {
		return nil, err
	}
	if st.SAS == "" {
		return nil, classError(ClassUnauthorized, "pair", "pairing SAS was not derived")
	}
	var install string
	c.mu.Lock()
	if len(c.pub) > 0 {
		install = hex.EncodeToString(c.pub)
	}
	c.mu.Unlock()
	reply, err := json.Marshal(pairingInner{
		V:          ProtocolVersion,
		Type:       pairReplyType,
		InstallPub: install,
		ReplyNonce: hex.EncodeToString(st.ReplyNonce),
	})
	if err != nil {
		return nil, err
	}
	return sealEnvelopeBytes(reply, y, origin, rid)
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
