package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client is the laptop remote-access client.
type Client struct {
	cfg Config

	mu           sync.Mutex
	st           PersistedState
	loaded       bool
	devices      []DeviceRecord
	channels     map[string]Channel
	waiters      []Waiter
	pending      map[string]PendingWork
	revoked      map[string]struct{}
	devEpoch     map[string]uint64
	globalGen    uint64
	pub          ed25519.PublicKey
	priv         ed25519.PrivateKey
	lockFile     *os.File
	faultSkip    int
	rvStopped    bool
	enrollPosted bool
	inviteFD     int
	inviteDev    uint64
	inviteIno    uint64
	invitePath   string
	loopCancel   context.CancelFunc
	loopWG       sync.WaitGroup
	backoff      *Backoff
	waitChals    map[string][]byte
	hosted       string
	lockMu       sync.Mutex
	lockRefs     int
	devWake      chan struct{}

	// pairing is this client's FR-11/FR-12 pairing state (pairing.go). It
	// hangs off the client rather than a package-level registry so it dies
	// with the client it belongs to; pairingOnce covers a zero-value Client
	// that did not come from NewClient.
	pairingOnce sync.Once
	pairing     *pairingRuntime
}

var errNoDeviceWait = fmt.Errorf("remote: no registered device to wait")

// NewClient constructs a client. It does not load state, generate keys,
// or contact the rendezvous.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:       cfg,
		channels:  map[string]Channel{},
		pending:   map[string]PendingWork{},
		revoked:   map[string]struct{}{},
		devEpoch:  map[string]uint64{},
		waitChals: map[string][]byte{},
		devWake:   make(chan struct{}, 1),
		inviteFD:  -1,
		pairing:   newPairingRuntime(),
	}
}

// PrivateDir is <data>/remote (0700). Path join only; no filesystem work.
func (c *Client) PrivateDir() string {
	if c == nil {
		return ""
	}
	return filepath.Join(c.cfg.DataDir, PrivateDirName)
}

// StatePath is <data>/remote/state (0600). Path join only.
func (c *Client) StatePath() string {
	return filepath.Join(c.PrivateDir(), StateFileName)
}

func (c *Client) hook() Hooks {
	if c == nil {
		return Hooks{}
	}
	return c.cfg.Hooks
}

func (c *Client) rand() io.Reader {
	if c.cfg.Rand != nil {
		return c.cfg.Rand
	}
	return rand.Reader
}

func (c *Client) httpClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       15 * time.Second,
		},
	}
}

func (c *Client) requestV() int {
	if c.cfg.Version != nil {
		return *c.cfg.Version
	}
	return ProtocolVersion
}

func (c *Client) minV() int {
	if c.cfg.MinVersion != nil {
		return *c.cfg.MinVersion
	}
	return MinRequestV
}

// Start enrolls or reconnects according to FR-30.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.hook().OnRemoteInit != nil {
		c.hook().OnRemoteInit()
	}

	if c.requestV() < c.minV() {
		return classError(ClassDowngrade, "start", "protocol version is below the minimum and will not be rewritten")
	}

	if err := c.acquireLock(ctx.Done()); err != nil {
		return err
	}
	err := c.startOwned(ctx)
	c.closeInviteFD()
	if err != nil {
		c.releaseLock()
		return err
	}
	c.startRVLoop()
	return nil
}

func (c *Client) startOwned(ctx context.Context) error {
	if err := c.loadState(); err != nil {
		return err
	}
	if c.hook().OnStateLoad != nil {
		c.hook().OnStateLoad()
	}

	if rec, ok := c.recoverEnrolledTemp(); ok && (c.cfg.ExplicitRetry || c.st.Status != StateEnrolled) {
		if c.cfg.ExplicitRetry && rec.PublicKey == c.st.PublicKey || c.st.PublicKey == "" || rec.PublicKey == c.st.PublicKey {
			if c.cfg.ExplicitRetry || isCompleteEnrolled(rec) && c.st.Handle == "" {
				if c.cfg.ExplicitRetry {
					c.applyState(rec)
					if err := c.persist(c.snapshotState()); err != nil {
						return err
					}
					return nil
				}
			}
		}
	}

	switch c.classify() {
	case StateDisabled:
		c.hosted = "disabled"
		return classError(ClassDisabled, "start", "remote access is disabled; reenable explicitly")
	case StateRevoked:
		c.hosted = "revoked"
		return classError(ClassRevoked, "start", "this installation has been revoked")
	case StateKeyMissing:
		return classError(ClassKeyMissing, "start", "the installation private key is missing")
	case StateCorrupt:
		return classError(ClassCorruptIdentity, "start", "the identity file is corrupt")
	case StatePartial:
		return classError(ClassPartialIdentity, "start", "a partial identity write was left behind")
	case StateAmbiguous:
		if c.cfg.ExplicitRetry {
			if rec, ok := c.recoverEnrolledTemp(); ok && rec.PublicKey == c.st.PublicKey {
				c.applyState(rec)
				if err := c.persist(c.snapshotState()); err != nil {
					return err
				}
				return nil
			}
			invite, src, err := c.readInvite()
			if err != nil {
				if classOfErr(err) == ClassNeedInvite {
					return classError(ClassAmbiguousEnrollment, "start", "enrollment is ambiguous; recover with an explicit retry")
				}
				return err
			}
			err = c.enrollWithExistingKey(ctx, invite)
			return c.consumePostedInvite(src, invite, err)
		}
		return classError(ClassAmbiguousEnrollment, "start", "enrollment is ambiguous; recover with an explicit retry")
	case StateEnrolled:
		if invite, src, err := c.readInviteOptional(); err != nil {
			return err
		} else if invite != "" && !c.cfg.Rotate {
			return classError(ClassInviteConflict, "start", "a different invite cannot rotate an enrolled identity without an explicit rotate")
		} else if invite != "" && c.cfg.Rotate {
			err := c.enrollNew(ctx, invite)
			return c.consumePostedInvite(src, invite, err)
		}
		if err := c.authenticate(ctx); err != nil {
			if classOfErr(err) == ClassRevoked {
				c.hosted = "revoked"
				c.st.Status = StateRevoked
				_ = c.persist(c.snapshotState())
				return err
			}
			c.hosted = "unavailable"
			return nil
		}
		c.hosted = "enrolled"
		return nil
	default:
		invite, src, err := c.readInvite()
		if err != nil {
			return err
		}
		err = c.enrollNew(ctx, invite)
		if err := c.consumePostedInvite(src, invite, err); err != nil {
			return err
		}
		if err := c.authenticate(ctx); err != nil {
			if classOfErr(err) == ClassRevoked {
				c.hosted = "revoked"
				c.st.Status = StateRevoked
				_ = c.persist(c.snapshotState())
			} else {
				c.hosted = "unavailable"
			}
			return err
		}
		c.hosted = "enrolled"
		return nil
	}
}

func classOfErr(err error) Class {
	var e *Error
	if err != nil && asClass(err, &e) {
		return e.Class
	}
	return ""
}

func asClass(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) classify() EnrollmentState {
	if !c.loaded {
		if tmpIncomplete(c.tempPath()) {
			return StatePartial
		}
		if _, err := os.Stat(c.tempPath()); err == nil {
			if _, _, uerr := readJSONFile(c.tempPath()); uerr != nil {
				return StatePartial
			}
			return StatePartial
		}
		return StateAbsent
	}
	if c.st.Status == StateDisabled {
		return StateDisabled
	}
	if c.st.Status == StateRevoked {
		return StateRevoked
	}
	if c.st.Status == StateCorrupt {
		return StateCorrupt
	}
	if c.st.Status == StateKeyMissing {
		return StateKeyMissing
	}
	if c.st.Status == StatePartial {
		return StatePartial
	}
	if c.st.Status == StateAmbiguous {
		return StateAmbiguous
	}
	if c.st.Status == StateEnrolled {
		if c.st.PrivateKey == "" || c.priv == nil {
			return StateKeyMissing
		}
		return StateEnrolled
	}
	if c.st.PublicKey != "" && c.st.PrivateKey != "" && c.st.Handle == "" {
		return StateAmbiguous
	}
	return StateAbsent
}

func (c *Client) loadState() error {
	path := c.StatePath()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if _, terr := os.Stat(c.tempPath()); terr == nil {
			_, _, uerr := readJSONFile(c.tempPath())
			if uerr != nil {
				c.loaded = false
				c.st = PersistedState{Status: StatePartial}
				return classError(ClassPartialIdentity, "start", "a partial identity write was left behind")
			}
			c.loaded = false
			return nil
		}
		c.loaded = false
		c.st = PersistedState{}
		return nil
	}
	if err != nil {
		return err
	}
	var st PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		c.loaded = true
		c.st = PersistedState{Status: StateCorrupt}
		return classError(ClassCorruptIdentity, "start", "the identity file is corrupt")
	}
	if cl := validatePersistedSemantics(st); cl != "" {
		c.loaded = true
		if cl == ClassKeyMissing {
			c.applyState(st)
			return classError(ClassKeyMissing, "start", "the installation private key is missing")
		}
		c.st = PersistedState{Status: StateCorrupt}
		return classError(ClassCorruptIdentity, "start", "the identity file is semantically invalid")
	}
	if err := c.applyState(st); err != nil {
		return err
	}
	return nil
}

func (c *Client) recoverEnrolledTemp() (PersistedState, bool) {
	st, _, err := readJSONFile(c.tempPath())
	if err != nil {
		return PersistedState{}, false
	}
	if !isCompleteEnrolled(st) {
		return PersistedState{}, false
	}
	return st, true
}

func (c *Client) consumePostedInvite(src inviteSrc, invite string, err error) error {
	if !c.enrollPosted {
		return err
	}
	eerr := c.eraseInvite(src, invite)
	if eerr == nil {
		return err
	}
	if err == nil {
		return eerr
	}
	return classErrorf(ClassInviteFile, "invite-file", "the invite file could not be removed after a redeeming enroll request; delete leftover invite files in that directory yourself. Enrollment remains ambiguous; recover with an explicit retry and do not assume the invite is unused", err)
}

func (c *Client) enrollNew(ctx context.Context, invite string) error {
	pub, priv, err := ed25519.GenerateKey(c.rand())
	if err != nil {
		return err
	}
	c.pub = pub
	c.priv = priv
	c.st = PersistedState{
		V:          1,
		Status:     StateAmbiguous,
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		Origin:     c.origin(),
		Pending:    &PendingEnrollment{PublicKey: hex.EncodeToString(pub), BoundAt: time.Now().UTC().Format(time.RFC3339)},
	}
	c.loaded = true
	if c.hook().OnIdentityGenerated != nil {
		c.hook().OnIdentityGenerated(append(ed25519.PublicKey(nil), pub...))
	}
	if err := c.persist(c.snapshotState()); err != nil {
		return err
	}
	return c.enrollWithExistingKey(ctx, invite)
}

func (c *Client) enrollWithExistingKey(ctx context.Context, invite string) error {
	if c.hook().OnEnrollAttempt != nil {
		c.hook().OnEnrollAttempt()
	}
	pubHex := c.st.PublicKey
	body, _ := json.Marshal(struct {
		V      int    `json:"v"`
		Code   string `json:"code"`
		PubKey string `json:"pubkey"`
	}{V: c.requestV(), Code: invite, PubKey: pubHex})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.rvBase(), "/")+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	c.enrollPosted = true
	if c.cfg.BeforeEnrollRequest != nil {
		c.cfg.BeforeEnrollRequest()
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.st.Status = StateAmbiguous
		if c.st.Pending == nil {
			c.st.Pending = &PendingEnrollment{PublicKey: pubHex}
		}
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: lost response: %w", err)
	}
	raw, err := readBounded(resp.Body, httpBodyMax)
	resp.Body.Close()
	if err != nil {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return classError(ClassAmbiguousEnrollment, "enroll", "the invite was not accepted; enrollment remains ambiguous")
	}
	ct := resp.Header.Get("Content-Type")
	if !acceptMediaType(ct, "application/json") {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: unexpected content type")
	}
	var er struct {
		Handle string      `json:"handle"`
		V      json.Number `json:"v"`
	}
	if err := json.Unmarshal(raw, &er); err != nil {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: malformed response")
	}
	if v, err := er.V.Int64(); err != nil || int(v) != ProtocolVersion {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: unexpected protocol version")
	}
	if !validHandle(er.Handle) {
		c.st.Status = StateAmbiguous
		_ = c.persist(c.snapshotState())
		return fmt.Errorf("enroll: malformed handle")
	}
	if c.hook().OnHandleReceived != nil {
		c.hook().OnHandleReceived(er.Handle)
	}
	c.st.Handle = er.Handle
	c.st.Status = StateEnrolled
	c.st.Pending = nil
	if err := c.persist(c.snapshotState()); err != nil {
		c.st.Status = StateAmbiguous
		c.st.Handle = ""
		return err
	}
	return nil
}

type inviteSrc struct {
	file string
}

func (c *Client) readInviteOptional() (string, inviteSrc, error) {
	if c.cfg.InviteFile == "" && !c.cfg.InviteStdin && c.cfg.InviteString == "" && c.cfg.Terminal == nil {
		return "", inviteSrc{}, nil
	}
	return c.readInvite()
}

func (c *Client) readInvite() (string, inviteSrc, error) {
	src := inviteSrc{}
	var raw string
	switch {
	case c.cfg.InviteFile != "":
		src.file = c.cfg.InviteFile
		s, err := c.readInviteFile(c.cfg.InviteFile)
		if err != nil {
			return "", src, err
		}
		raw = s
	case c.cfg.InviteStdin:
		in := c.cfg.Stdin
		if in == nil {
			in = os.Stdin
		}
		b, err := io.ReadAll(io.LimitReader(in, int64(inviteInputMax)+1))
		if err != nil {
			return "", src, err
		}
		if len(b) > inviteInputMax {
			return "", src, classError(ClassInviteFormat, "invite", "invite input exceeds the allowed size")
		}
		raw = string(b)
	case c.cfg.InviteString != "":
		raw = c.cfg.InviteString
	case c.cfg.Terminal != nil:
		s, err := c.readInviteTTY()
		if err != nil {
			return "", src, err
		}
		raw = s
	default:
		if c.cfg.NewTerminal != nil {
			term, err := c.cfg.NewTerminal()
			if err != nil {
				return "", src, TerminalInviteError(err)
			}
			c.cfg.Terminal = term
			s, err := c.readInviteTTY()
			if err != nil {
				return "", src, err
			}
			raw = s
			break
		}
		return "", src, needInviteErr()
	}
	raw = strings.TrimSpace(raw)
	if _, err := parseInvite(raw); err != nil {
		return "", src, classError(ClassInviteFormat, "invite", "the invite is not a valid code")
	}
	return raw, src, nil
}

func (c *Client) readInviteTTY() (string, error) {
	t := c.cfg.Terminal
	if err := t.DisableEcho(); err != nil {
		rerr := t.RestoreEcho()
		return "", TerminalInviteError(errors.Join(err, rerr))
	}
	perr := t.WritePrompt([]byte("invite: "))
	line, rerr := t.ReadLine()
	restErr := t.RestoreEcho()
	if perr != nil || rerr != nil || restErr != nil {
		return "", TerminalInviteError(errors.Join(perr, rerr, restErr))
	}
	return line, nil
}

// Close stops the rendezvous client and releases process ownership.
func (c *Client) Close() error {
	err := c.StopRendezvous()
	c.closeInviteFD()
	c.releaseLock()
	return err
}

// State reports the FR-30 enrollment state.
func (c *Client) State() (EnrollmentState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.loadState(); err != nil {
			if e, ok := err.(*Error); ok {
				return eClassState(e.Class), err
			}
			return "", err
		}
	}
	if c.st.Status != "" {
		if c.st.Status == StateEnrolled && c.st.PrivateKey == "" {
			return StateKeyMissing, nil
		}
		return c.st.Status, nil
	}
	return StateAbsent, nil
}

func eClassState(cl Class) EnrollmentState {
	switch cl {
	case ClassCorruptIdentity:
		return StateCorrupt
	case ClassPartialIdentity:
		return StatePartial
	case ClassKeyMissing:
		return StateKeyMissing
	case ClassAmbiguousEnrollment:
		return StateAmbiguous
	case ClassRevoked:
		return StateRevoked
	case ClassDisabled:
		return StateDisabled
	default:
		return ""
	}
}

// PublicKey returns the installation public key.
func (c *Client) PublicKey() (ed25519.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.loadState(); err != nil {
			return nil, err
		}
	}
	if len(c.pub) == 0 {
		return nil, fmt.Errorf("remote: no public key")
	}
	return append(ed25519.PublicKey(nil), c.pub...), nil
}

// Handle returns the non-secret installation handle.
func (c *Client) Handle() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.loadState(); err != nil {
			return "", err
		}
	}
	if c.st.Handle == "" {
		return "", fmt.Errorf("remote: no handle")
	}
	return c.st.Handle, nil
}

// RegisterDevice records one paired device and mints a 256-bit RID.
func (c *Client) RegisterDevice(ctx context.Context, rec DeviceRecord) (DeviceRecord, error) {
	_ = ctx
	var out DeviceRecord
	err := c.withStateLock("register", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.loaded {
			if err := c.loadState(); err != nil {
				return err
			}
		}
		if c.st.Status == StateDisabled {
			return classError(ClassDisabled, "register", "remote access is disabled")
		}
		if c.st.Status == StateRevoked {
			return classError(ClassRevoked, "register", "this installation has been revoked")
		}
		if rec.ID == "" {
			return classError(ClassUnauthorized, "register", "device id is empty")
		}
		for _, d := range c.devices {
			if d.ID == rec.ID {
				return classError(ClassUnauthorized, "register", "device id is already registered")
			}
		}
		if len(rec.PubKey) != ed25519.PublicKeySize {
			return classError(ClassUnauthorized, "register", "device public key is invalid")
		}
		rid, err := mintRID(c.rand(), c.devices)
		if err != nil {
			return err
		}
		out = DeviceRecord{
			ID:      rec.ID,
			RID:     rid,
			PubKey:  append([]byte(nil), rec.PubKey...),
			ECDHPub: append([]byte(nil), rec.ECDHPub...),
		}
		c.devices = append(c.devices, out)
		if err := c.persist(c.snapshotState()); err != nil {
			c.devices = c.devices[:len(c.devices)-1]
			return err
		}
		return nil
	})
	if err != nil {
		return DeviceRecord{}, err
	}
	c.kickWaiters()
	return DeviceRecord{ID: out.ID, RID: out.RID, PubKey: append([]byte(nil), out.PubKey...)}, nil
}

func mintRID(r io.Reader, existing []DeviceRecord) (string, error) {
	seen := map[string]struct{}{}
	for _, d := range existing {
		seen[d.RID] = struct{}{}
	}
	buf := make([]byte, RendezvousIDBits/8)
	for i := 0; i < 16; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		rid := hex.EncodeToString(buf)
		if _, ok := seen[rid]; !ok {
			return rid, nil
		}
	}
	return "", fmt.Errorf("remote: rid collision")
}

// Devices lists persisted device records.
func (c *Client) Devices() ([]DeviceRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.loadState(); err != nil {
			return nil, err
		}
	}
	out := make([]DeviceRecord, len(c.devices))
	for i, d := range c.devices {
		out[i] = DeviceRecord{ID: d.ID, RID: d.RID, PubKey: append([]byte(nil), d.PubKey...), ECDHPub: append([]byte(nil), d.ECDHPub...)}
	}
	return out, nil
}

// RevokeDevice is FR-13/FR-29: durable local revoke, no rendezvous required.
func (c *Client) RevokeDevice(ctx context.Context, id string) error {
	_ = ctx
	err := c.withStateLock("revoke", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.loaded {
			if err := c.loadState(); err != nil {
				return err
			}
		}
		if ch := c.channels[id]; ch != nil {
			_ = ch.Close()
			delete(c.channels, id)
		}
		if _, ok := c.pending[id]; ok {
			delete(c.pending, id)
			if c.hook().OnPendingErase != nil {
				c.hook().OnPendingErase(id)
			}
		}
		c.revoked[id] = struct{}{}
		c.devEpoch[id]++
		kept := c.devices[:0]
		for _, d := range c.devices {
			if d.ID == id {
				if d.RID != "" {
					c.revoked[d.RID] = struct{}{}
				}
				continue
			}
			kept = append(kept, d)
		}
		c.devices = kept
		if err := c.persist(c.snapshotState()); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		c.kickWaiters()
	}
	return err
}

// AttachChannel registers a live fake channel for a device.
func (c *Client) AttachChannel(id string, ch Channel) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.authDevice(id); err != nil {
		return err
	}
	c.channels[id] = ch
	return nil
}

// Channel returns the live channel for a device, if any.
func (c *Client) Channel(id string) (Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.channels[id]
	if !ok {
		return nil, classError(ClassNotFound, "channel", "no live channel")
	}
	return ch, nil
}

// AddWaiter registers a fake rendezvous wait/ID.
func (c *Client) AddWaiter(w Waiter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w == nil {
		return classError(ClassNotFound, "wait", "unknown rendezvous id")
	}
	if !c.loaded {
		if err := c.loadState(); err != nil {
			return err
		}
	}
	if c.st.Status == StateDisabled {
		return classError(ClassDisabled, "wait", "remote access is disabled")
	}
	if c.st.Status == StateRevoked {
		return classError(ClassRevoked, "wait", "this installation has been revoked")
	}
	rid := w.ID()
	if !validRID(rid) {
		return classError(ClassNotFound, "wait", "unknown rendezvous id")
	}
	if _, ok := c.revoked[rid]; ok {
		return classError(ClassRevoked, "wait", "rendezvous id is revoked")
	}
	found := false
	for _, d := range c.devices {
		if d.RID == rid {
			found = true
			break
		}
	}
	if !found {
		return classError(ClassNotFound, "wait", "unknown rendezvous id")
	}
	c.waiters = append(c.waiters, w)
	return nil
}

// Waiters lists current wait registrations.
func (c *Client) Waiters() ([]Waiter, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Waiter, len(c.waiters))
	copy(out, c.waiters)
	return out, nil
}

func (c *Client) authDevice(id string) error {
	if c.st.Status == StateDisabled {
		return classError(ClassDisabled, "auth", "remote access is disabled")
	}
	if _, ok := c.revoked[id]; ok {
		return classError(ClassRevoked, "auth", "device is revoked")
	}
	for _, d := range c.devices {
		if d.ID == id {
			return nil
		}
	}
	return classError(ClassNotFound, "auth", "unknown device")
}

func (c *Client) snapshotAuth(id string) (disabled bool, epoch uint64, gen uint64) {
	return c.st.Status == StateDisabled, c.devEpoch[id], c.globalGen
}

func waitGate(ctx context.Context, g *Gate) error {
	if g == nil {
		return nil
	}
	if g.Arrived != nil {
		select {
		case <-g.Arrived:
		default:
			close(g.Arrived)
		}
	}
	if g.Release == nil {
		return nil
	}
	select {
	case <-g.Release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func signalPublished(g *Gate) {
	if g == nil || g.Published == nil {
		return
	}
	select {
	case <-g.Published:
	default:
		close(g.Published)
	}
}

func (c *Client) gated(ctx context.Context, deviceID string, gate *Gate, succeed func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if err := c.authDevice(deviceID); err != nil {
		c.mu.Unlock()
		return err
	}
	_, epoch, gen := c.snapshotAuth(deviceID)
	c.mu.Unlock()

	if err := waitGate(ctx, gate); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.authDevice(deviceID); err != nil {
		return err
	}
	_, epoch2, gen2 := c.snapshotAuth(deviceID)
	if epoch2 != epoch || gen2 != gen {
		return c.authDevice(deviceID)
	}
	if succeed != nil {
		return succeed()
	}
	return nil
}

// Handshake starts a newly opening handshake that races revoke/disable.
func (c *Client) Handshake(ctx context.Context, deviceID string, gate *Gate) error {
	return c.gated(ctx, deviceID, gate, nil)
}

// PublishChannel publishes a candidate live channel (linearizability barrier).
func (c *Client) PublishChannel(ctx context.Context, deviceID string, ch Channel, gate *Gate) error {
	return c.gated(ctx, deviceID, gate, func() error {
		c.channels[deviceID] = ch
		signalPublished(gate)
		return nil
	})
}

// BeginRequest is a request already at the authorization boundary.
func (c *Client) BeginRequest(ctx context.Context, deviceID string, gate *Gate) error {
	return c.gated(ctx, deviceID, gate, func() error {
		if c.hook().OnAdmission != nil {
			c.hook().OnAdmission(deviceID)
		}
		return nil
	})
}

// Deliver sends one peer request. Absent peer → ClassPeerAbsent, nothing queued (FR-24).
func (c *Client) Deliver(ctx context.Context, deviceID string, msg []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if err := c.authDevice(deviceID); err != nil {
		c.mu.Unlock()
		if classOfErr(err) == ClassNotFound || classOfErr(err) == ClassRevoked {
			return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
		}
		return err
	}
	_, epoch, gen := c.snapshotAuth(deviceID)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	if err := c.authDevice(deviceID); err != nil {
		c.mu.Unlock()
		if classOfErr(err) == ClassNotFound || classOfErr(err) == ClassRevoked {
			return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
		}
		return err
	}
	_, epoch2, gen2 := c.snapshotAuth(deviceID)
	if epoch2 != epoch || gen2 != gen {
		c.mu.Unlock()
		return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
	}
	ch := c.channels[deviceID]
	c.mu.Unlock()
	if ch == nil {
		return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
	}
	err := ch.Send(msg)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.revoked[deviceID]; ok || c.st.Status == StateDisabled {
		return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
	}
	_, epoch3, gen3 := c.snapshotAuth(deviceID)
	if epoch3 != epoch || gen3 != gen {
		return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
	}
	if err != nil {
		if ch.Closed() {
			return classError(ClassPeerAbsent, "deliver", "the peer is not connected")
		}
		return err
	}
	return nil
}

// DisableAll is FR-32.
func (c *Client) DisableAll(ctx context.Context) error {
	_ = ctx
	c.stopRVLoop()
	return c.withStateLock("disable", func() error {
		return c.disableAllLocked()
	})
}

func (c *Client) disableAllLocked() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if err := c.loadState(); err != nil {
			return err
		}
	}
	switch c.classify() {
	case StateCorrupt:
		return classError(ClassCorruptIdentity, "disable", "the identity file is corrupt")
	case StatePartial:
		return classError(ClassPartialIdentity, "disable", "a partial identity write was left behind")
	case StateKeyMissing:
		return classError(ClassKeyMissing, "disable", "the installation private key is missing")
	case StateAmbiguous:
		return classError(ClassAmbiguousEnrollment, "disable", "enrollment is ambiguous")
	}
	c.st.Status = StateDisabled
	c.hosted = "disabled"
	c.globalGen++
	for id, ch := range c.channels {
		if ch != nil {
			_ = ch.Close()
		}
		delete(c.channels, id)
	}
	for _, w := range c.waiters {
		if w != nil {
			w.Cancel()
		}
	}
	c.waiters = nil
	for id := range c.pending {
		delete(c.pending, id)
		if c.hook().OnPendingErase != nil {
			c.hook().OnPendingErase(id)
		}
	}
	if c.loaded || c.st.PublicKey != "" || c.st.Handle != "" {
		c.loaded = true
		if c.st.V == 0 {
			c.st.V = 1
		}
		if err := c.persist(c.snapshotState()); err != nil {
			return err
		}
	}
	c.rvStopped = true
	if c.hook().OnRendezvousStop != nil {
		c.hook().OnRendezvousStop()
	}
	return nil
}

// Reenable is the distinct, explicit action that reverses DisableAll.
func (c *Client) Reenable(ctx context.Context) error {
	_ = ctx
	return c.withStateLock("reenable", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.loaded {
			if err := c.loadState(); err != nil {
				return err
			}
		}
		if c.st.Status != StateDisabled {
			return nil
		}
		if c.st.Handle == "" || c.st.PublicKey == "" || c.st.PrivateKey == "" || !validHandle(c.st.Handle) {
			return classError(ClassCorruptIdentity, "reenable", "disabled state has no complete enrolled identity")
		}
		c.st.Status = StateEnrolled
		c.hosted = "enrolled"
		return c.persist(c.snapshotState())
	})
}

// SetPending installs pending handshake/envelope/reply evidence for a device.
func (c *Client) SetPending(id string, p PendingWork) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.authDevice(id); err != nil {
		return err
	}
	if c.pending == nil {
		c.pending = map[string]PendingWork{}
	}
	c.pending[id] = PendingWork{
		Handshake: append([]byte(nil), p.Handshake...),
		Envelope:  append([]byte(nil), p.Envelope...),
		Reply:     append([]byte(nil), p.Reply...),
	}
	return nil
}

// DevicePending reports pending handshake/envelope/reply state for a device.
func (c *Client) DevicePending(id string) (PendingWork, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.Status == StateDisabled {
		return PendingWork{}, classError(ClassDisabled, "pending", "remote access is disabled")
	}
	if _, ok := c.revoked[id]; ok {
		return PendingWork{}, classError(ClassNotFound, "pending", "device is revoked")
	}
	p, ok := c.pending[id]
	if !ok {
		found := false
		for _, d := range c.devices {
			if d.ID == id {
				found = true
				break
			}
		}
		if !found {
			return PendingWork{}, classError(ClassNotFound, "pending", "unknown device")
		}
		return PendingWork{}, nil
	}
	return PendingWork{
		Handshake: append([]byte(nil), p.Handshake...),
		Envelope:  append([]byte(nil), p.Envelope...),
		Reply:     append([]byte(nil), p.Reply...),
	}, nil
}

// StopRendezvous stops the rendezvous client without disabling enrollment.
func (c *Client) StopRendezvous() error {
	c.stopRVLoop()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rvStopped = true
	if c.hook().OnRendezvousStop != nil {
		c.hook().OnRendezvousStop()
	}
	return nil
}

func (c *Client) startRVLoop() {
	if c.cfg.Backoff.Initial == 0 && c.cfg.Scheduler == nil {
		return
	}
	c.backoff = NewBackoff(c.cfg.Backoff, c.cfg.Clock, c.cfg.RNG)
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.loopCancel = cancel
	c.mu.Unlock()
	c.loopWG.Add(1)
	go func() {
		defer c.loopWG.Done()
		c.rvLoop(ctx)
	}()
}

func (c *Client) stopRVLoop() {
	c.mu.Lock()
	cancel := c.loopCancel
	c.loopCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.kickWaiters()
	c.loopWG.Wait()
}

func (c *Client) kickWaiters() {
	if c.devWake == nil {
		return
	}
	select {
	case c.devWake <- struct{}{}:
	default:
	}
}

func (c *Client) rvLoop(ctx context.Context) {
	var wg sync.WaitGroup
	active := map[string]context.CancelFunc{}
	defer func() {
		for rid, cancel := range active {
			cancel()
			delete(active, rid)
		}
		wg.Wait()
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		if c.loopStopped() {
			return
		}
		live := c.liveRIDs()
		for rid, cancel := range active {
			if !live[rid] {
				cancel()
				delete(active, rid)
			}
		}
		started := 0
		for rid := range live {
			if _, ok := active[rid]; ok {
				continue
			}
			rctx, cancel := context.WithCancel(ctx)
			active[rid] = cancel
			rid := rid
			wg.Add(1)
			started++
			go func() {
				defer wg.Done()
				c.waitLoopRID(rctx, rid)
			}()
		}
		if len(live) == 0 {
			delay := c.cfg.Backoff.Initial
			if delay == 0 {
				delay = 50 * time.Millisecond
			}
			if !c.schedWaitOrKick(ctx, delay) {
				return
			}
			continue
		}
		if started > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-c.devWake:
		}
	}
}

func (c *Client) loopStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rvStopped || c.st.Status == StateRevoked || c.st.Status == StateDisabled
}

func (c *Client) liveRIDs() map[string]bool {
	c.mu.Lock()
	out := map[string]bool{}
	if c.st.Status == StateRevoked || c.st.Status == StateDisabled || c.rvStopped {
		c.mu.Unlock()
		return out
	}
	for _, d := range c.devices {
		if validRID(d.RID) {
			if _, revoked := c.revoked[d.RID]; !revoked {
				out[d.RID] = true
			}
		}
	}
	c.mu.Unlock()
	for rid := range c.pairingLiveRIDs() {
		out[rid] = true
	}
	return out
}

func (c *Client) waitLoopRID(ctx context.Context, rid string) {
	// reply is the sealed answer owed to this device, carried out on the next
	// wait. It survives a failed post so a transport blip does not strand a
	// device that is holding its envelope POST open.
	var reply []byte
	for {
		if ctx.Err() != nil {
			return
		}
		if !c.liveRIDs()[rid] {
			c.kickWaiters()
			return
		}
		var env []byte
		var err error
		if pairCode, ok := c.pairingWaiter(rid); ok {
			env, err = c.postPairWaitRID(ctx, pairCode, rid, reply)
		} else {
			env, err = c.postWaitRID(ctx, rid, reply)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Delivered. A failed post may not have reached the rendezvous, so
			// the reply is held for the next attempt rather than dropped.
			reply = nil
		}
		if len(env) > 0 {
			// A device is calling. Answering is best-effort by design: an
			// envelope that does not open, or a device with no pairing key on
			// record, must leave the loop polling and send nothing back.
			var out []byte
			var aerr error
			if _, ok := c.pairingWaiter(rid); ok {
				out, aerr = c.answerPairingEnvelope(ctx, rid, env)
			} else {
				out, aerr = c.answerSessionEnvelope(ctx, rid, env)
			}
			if aerr == nil {
				reply = out
			} else {
				c.noteWaitResult(aerr)
			}
		}
		if classOfErr(err) == ClassRevoked {
			c.applyRevoked()
			return
		}
		c.noteWaitResult(err)
		var delay time.Duration
		switch {
		case errors.Is(err, errNoDeviceWait):
			return
		case err != nil:
			if c.backoff != nil {
				_ = c.backoff.NotifyDown()
				_ = c.backoff.Failure()
				delay, _ = c.backoff.Delay()
			} else {
				delay = 100 * time.Millisecond
			}
		default:
			if c.backoff != nil {
				_ = c.backoff.NotifyUp()
			}
			delay = c.cfg.Backoff.Initial
			if delay == 0 {
				delay = 100 * time.Millisecond
			}
		}
		if !c.schedWait(ctx, delay) {
			return
		}
	}
}

func (c *Client) noteWaitResult(err error) {
	if classOfErr(err) == ClassRevoked {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.Status == StateRevoked || c.st.Status == StateDisabled {
		return
	}
	if err != nil && !errors.Is(err, errNoDeviceWait) {
		c.hosted = "unavailable"
		return
	}
	c.hosted = "enrolled"
}

func (c *Client) applyRevoked() {
	c.mu.Lock()
	c.hosted = "revoked"
	if c.st.Status != StateRevoked {
		c.st.Status = StateRevoked
		_ = c.persist(c.snapshotState())
	}
	c.rvStopped = true
	cancel := c.loopCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.kickWaiters()
}

// postWaitRID performs one /v1/wait for rid, optionally carrying a sealed
// reply for the device, and returns the sealed envelope the rendezvous served
// (nil on a 204).
//
// The envelope used to be read and discarded here. It is returned now because
// it is the only thing the wait exists to fetch: dropping it made the whole
// loop a no-op with correct challenge rotation.
func (c *Client) postWaitRID(ctx context.Context, rid string, reply []byte) ([]byte, error) {
	return c.postSignedWait(ctx, "/v1/wait", rid, "", reply)
}

func (c *Client) postPairWaitRID(ctx context.Context, code, rid string, reply []byte) ([]byte, error) {
	return c.postSignedWait(ctx, "/v1/pair/wait", rid, code, reply)
}

func (c *Client) postSignedWait(ctx context.Context, route, rid, code string, reply []byte) ([]byte, error) {
	c.mu.Lock()
	if c.st.Handle == "" || len(c.priv) != ed25519.PrivateKeySize || !validRID(rid) {
		c.mu.Unlock()
		return nil, errNoDeviceWait
	}
	handle := c.st.Handle
	priv := append(ed25519.PrivateKey(nil), c.priv...)
	origin := c.origin()
	chal := append([]byte(nil), c.waitChals[rid]...)
	c.mu.Unlock()

	if len(chal) != 32 {
		got, err := c.postChallenge(ctx)
		if err != nil {
			c.clearWaitChalRID(rid)
			return nil, err
		}
		chal = got
	}
	msg := buildAuthMessage(origin, ProtocolVersion, route, handle, chal)
	sig := ed25519.Sign(priv, msg)
	fields := map[string]any{
		"v":         c.requestV(),
		"handle":    handle,
		"challenge": hex.EncodeToString(chal),
		"sig":       hex.EncodeToString(sig),
		"id":        rid,
		"max_ms":    WaitDefaultMaxMS,
	}
	if code != "" {
		fields["code"] = code
	}
	// A sealed answer rides out on an ordinary wait. The signature above is
	// unchanged by its presence — the rendezvous vectors carry a
	// byte-identical sig for wait-response-envelope and envelope-reply-on-wait
	// — so reply is outside the signed message, exactly like max_ms.
	if len(reply) > 0 {
		fields["reply"] = hex.EncodeToString(reply)
	}
	body, _ := json.Marshal(fields)
	resp, err := c.postWaitJSONAt(ctx, route, body)
	if err != nil {
		c.clearWaitChalRID(rid)
		return nil, err
	}
	raw, boundErr := readBounded(resp.Body, waitBodyMax)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		c.clearWaitChalRID(rid)
		return nil, classError(ClassUnavailable, "wait", "rendezvous rejected the wait; retrying with a fresh challenge")
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		c.clearWaitChalRID(rid)
		return nil, fmt.Errorf("wait status %d", resp.StatusCode)
	}
	if boundErr != nil {
		c.clearWaitChalRID(rid)
		return nil, boundErr
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		c.clearWaitChalRID(rid)
		return nil, fmt.Errorf("wait missing nosniff")
	}
	next, err := hex.DecodeString(resp.Header.Get("X-Rv-Challenge"))
	if err != nil || len(next) != 32 {
		c.clearWaitChalRID(rid)
		return nil, fmt.Errorf("wait missing next challenge")
	}
	if resp.StatusCode == http.StatusNoContent && len(raw) != 0 {
		c.clearWaitChalRID(rid)
		return nil, fmt.Errorf("wait nonempty 204")
	}
	if resp.StatusCode == http.StatusOK {
		ct := resp.Header.Get("Content-Type")
		if !acceptMediaType(ct, "application/octet-stream") {
			c.clearWaitChalRID(rid)
			return nil, fmt.Errorf("wait unexpected content type")
		}
	}
	c.mu.Lock()
	if c.waitChals == nil {
		c.waitChals = map[string][]byte{}
	}
	c.waitChals[rid] = next
	c.mu.Unlock()
	return raw, nil
}

func (c *Client) postWaitJSON(ctx context.Context, body []byte) (*http.Response, error) {
	return c.postWaitJSONAt(ctx, "/v1/wait", body)
}

func (c *Client) postWaitJSONAt(ctx context.Context, route string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.rvBase(), "/")+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	return c.waitHTTPClient().Do(req)
}

func (c *Client) waitHTTPClient() *http.Client {
	if c.cfg.HTTPClient != nil {
		return c.cfg.HTTPClient
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: time.Duration(WaitDefaultMaxMS)*time.Millisecond + 15*time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}

func (c *Client) clearWaitChalRID(rid string) {
	c.mu.Lock()
	delete(c.waitChals, rid)
	c.mu.Unlock()
}

// HostedStatus is the remote status projected to the hosted application.
func (c *Client) HostedStatus() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosted != "" {
		return c.hosted
	}
	switch c.st.Status {
	case StateRevoked:
		return "revoked"
	case StateDisabled:
		return "disabled"
	case StateEnrolled:
		return "enrolled"
	default:
		return string(c.st.Status)
	}
}

func (c *Client) withStateLock(op string, fn func() error) error {
	if err := c.holdLock(op); err != nil {
		return err
	}
	defer c.unholdLock()
	return fn()
}

func (c *Client) schedWait(ctx context.Context, d time.Duration) bool {
	return c.schedWaitSelect(ctx, d, false)
}

func (c *Client) schedWaitOrKick(ctx context.Context, d time.Duration) bool {
	return c.schedWaitSelect(ctx, d, true)
}

func (c *Client) schedWaitSelect(ctx context.Context, d time.Duration, kick bool) bool {
	if c.cfg.Scheduler != nil {
		ch := make(chan struct{})
		cancel := c.cfg.Scheduler.After(d, func() { close(ch) })
		if kick {
			select {
			case <-ctx.Done():
				cancel()
				return false
			case <-ch:
				return true
			case <-c.devWake:
				cancel()
				return true
			}
		}
		select {
		case <-ctx.Done():
			cancel()
			return false
		case <-ch:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	if kick {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-c.devWake:
			return true
		}
	}
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
