package remote

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

const tempSuffix = ".tmp"

func (c *Client) tempPath() string {
	return c.StatePath() + tempSuffix
}

// persist writes identity state with a durable transaction:
//
//	write temp → fsync temp → rename → fsync parent
//
// Rename alone is not durable: without the temp and parent fsyncs a crash
// can leave the new name pointing at a zero-length or never-synced file.
// Failures before rename intentionally leave a recoverable .tmp next to
// the live state file; cleanup is not automatic because deleting a temp
// that may hold the only copy of a newly enrolled identity would destroy
// recovery evidence. Startup adopts a complete enrolled temp
// (recoverEnrolledTemp); any other present temp remains partial recovery
// evidence. tmpIncomplete distinguishes unreadable or malformed JSON from a
// parseable-but-semantically-incomplete record; semantic validation is separate.
func (c *Client) persist(st PersistedState) error {
	if st.V == 0 {
		st.V = 1
	}
	if c.cfg.BeforePersist != nil {
		c.cfg.BeforePersist()
	}
	dir := c.PrivateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)

	fault := c.cfg.FailWrite
	match := fault != nil && (fault.When == "" || fault.When == st.Status)
	if match {
		if c.faultSkip < fault.Skip {
			c.faultSkip++
			match = false
		}
	}
	fail := func(step WriteStep) bool {
		return match && fault != nil && fault.Step == step
	}
	injected := func(step WriteStep) error {
		return fmt.Errorf("remote: persist %s: injected failure", step)
	}

	tmp := c.tempPath()
	if fail(WriteTempCreate) {
		return injected(WriteTempCreate)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if fail(WriteData) {
		f.Close()
		return injected(WriteData)
	}
	raw, err := json.Marshal(c.persistEnvelope(st))
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}

	if fail(WriteSync) {
		f.Close()
		return injected(WriteSync)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}

	if fail(WriteChmod) {
		f.Close()
		return injected(WriteChmod)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)

	if fail(WriteRename) {
		return injected(WriteRename)
	}
	if err := os.Rename(tmp, c.StatePath()); err != nil {
		return err
	}
	_ = os.Chmod(c.StatePath(), 0o600)

	if fail(WriteParentSync) {
		return injected(WriteParentSync)
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

func (c *Client) snapshotState() PersistedState {
	st := c.st
	st.V = 1
	if st.Origin == "" {
		st.Origin = c.origin()
	}
	st.Devices = make([]PersistedDevice, len(c.devices))
	for i, d := range c.devices {
		st.Devices[i] = PersistedDevice{
			ID:      d.ID,
			RID:     d.RID,
			PubKey:  hex.EncodeToString(d.PubKey),
			ECDHPub: hex.EncodeToString(d.ECDHPub),
		}
	}
	return st
}

func (c *Client) applyState(st PersistedState) error {
	c.st = st
	c.loaded = true
	c.devices = c.devices[:0]
	for _, d := range st.Devices {
		rec := DeviceRecord{ID: d.ID, RID: d.RID}
		if d.PubKey != "" {
			if raw, err := hex.DecodeString(d.PubKey); err == nil {
				rec.PubKey = raw
			}
		}
		if d.ECDHPub != "" {
			if raw, err := hex.DecodeString(d.ECDHPub); err == nil {
				rec.ECDHPub = raw
			}
		}
		c.devices = append(c.devices, rec)
	}
	if st.PublicKey != "" {
		if raw, err := hex.DecodeString(st.PublicKey); err == nil {
			c.pub = raw
		}
	}
	if st.PrivateKey != "" {
		if raw, err := hex.DecodeString(st.PrivateKey); err == nil {
			c.priv = raw
		}
	}
	return c.applyPairingXFromDisk()
}

// persistedEnvelope is the identity file plus the pairing ECDH key X.
// Extra fields are ignored by PersistedState unmarshal; applyState re-reads
// them so a restart keeps the same SAS identity.
type persistedEnvelope struct {
	PersistedState
	PairingXPriv string `json:"pairing_x_priv,omitempty"`
	PairingXPub  string `json:"pairing_x_pub,omitempty"`
}

func (c *Client) persistEnvelope(st PersistedState) persistedEnvelope {
	env := persistedEnvelope{PersistedState: st}
	priv, pub := c.copyPairingX()
	if len(priv) > 0 && len(pub) > 0 {
		env.PairingXPriv = hex.EncodeToString(priv)
		env.PairingXPub = hex.EncodeToString(pub)
	}
	return env
}

func (c *Client) copyPairingX() (priv, pub []byte) {
	if c == nil || c.pairing == nil {
		return nil, nil
	}
	r := c.pairing
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.xPriv...), append([]byte(nil), r.xPub...)
}

func (c *Client) persistPairingX() error {
	if c == nil {
		return nil
	}
	return c.withStateLock("pair-x", func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.st.Status == "" {
			return nil
		}
		return c.persist(c.snapshotState())
	})
}

// ensureDurableX is the pairing-runtime half of the S7f invariant: any X
// that leaves the runtime must already be on the identity file. A persist
// failure leaves no X in memory, so the next call mints rather than
// handing back a stranded key. Durability is "is it on disk?", not "did
// this call mint it?".
func (c *Client) ensureDurableX() error {
	return c.withStateLock("pair-x", func() error {
		r := pairingOf(c)
		r.mu.Lock()
		if len(r.xPub) > 0 && r.xOnDisk {
			r.mu.Unlock()
			return nil
		}
		if _, err := r.ensureX(); err != nil {
			r.mu.Unlock()
			return err
		}
		r.mu.Unlock()

		if err := c.persistPairingX(); err != nil {
			r.mu.Lock()
			if !r.xOnDisk {
				r.xPriv = nil
				r.xPub = nil
			}
			r.mu.Unlock()
			return err
		}
		r.mu.Lock()
		r.xOnDisk = true
		r.mu.Unlock()
		return nil
	})
}

func (c *Client) applyPairingXFromDisk() error {
	raw, err := os.ReadFile(c.StatePath())
	if err != nil {
		return nil
	}
	var extra persistedEnvelope
	if json.Unmarshal(raw, &extra) != nil {
		return nil
	}
	if extra.PairingXPriv == "" && extra.PairingXPub == "" {
		return nil
	}
	if extra.PairingXPriv == "" || extra.PairingXPub == "" {
		return classError(ClassCorruptIdentity, "start", "pairing_x_priv and pairing_x_pub must both be present")
	}
	priv, err := hex.DecodeString(extra.PairingXPriv)
	if err != nil || len(priv) == 0 {
		return classError(ClassCorruptIdentity, "start", "pairing_x_priv is not valid hex")
	}
	pub, err := hex.DecodeString(extra.PairingXPub)
	if err != nil || len(pub) == 0 {
		return classError(ClassCorruptIdentity, "start", "pairing_x_pub is not valid hex")
	}
	if err := pairingXCorresponds(priv, pub); err != nil {
		return err
	}
	r := pairingOf(c)
	r.mu.Lock()
	r.xPriv = priv
	r.xPub = pub
	r.xOnDisk = true
	r.mu.Unlock()
	return nil
}

func pairingXCorresponds(priv, pub []byte) error {
	curve := ecdh.P256()
	k, err := curve.NewPrivateKey(priv)
	if err != nil {
		return classError(ClassCorruptIdentity, "start", "pairing_x_priv is not a valid P-256 private key")
	}
	peer, err := curve.NewPublicKey(pub)
	if err != nil {
		return classError(ClassCorruptIdentity, "start", "pairing_x_pub is not a valid P-256 public key")
	}
	if !bytes.Equal(k.PublicKey().Bytes(), peer.Bytes()) {
		return classError(ClassCorruptIdentity, "start", "pairing_x_priv and pairing_x_pub are not a corresponding valid P-256 pair")
	}
	return nil
}

func (c *Client) origin() string {
	if c.cfg.Origin != "" {
		return c.cfg.Origin
	}
	return DefaultOrigin
}

func (c *Client) rvBase() string {
	if c.cfg.RendezvousURL != "" {
		return c.cfg.RendezvousURL
	}
	return c.origin()
}

func readJSONFile(path string) (PersistedState, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PersistedState{}, nil, err
	}
	var st PersistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return PersistedState{}, raw, err
	}
	return st, raw, nil
}

func isCompleteEnrolled(st PersistedState) bool {
	return st.Status == StateEnrolled && st.Handle != "" && st.PublicKey != "" && st.PrivateKey != ""
}

// tmpIncomplete reports whether path exists but cannot be read as JSON.
// It is a syntactic gate for a half-written temporary state file: missing
// paths are not incomplete, and a parseable object is not incomplete even
// when its fields would fail validatePersistedSemantics. Semantic validation
// stays separate.
func tmpIncomplete(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, _, uerr := readJSONFile(path)
	if uerr != nil {
		return true
	}
	_ = st
	return false
}

func classError(class Class, op, guidance string) *Error {
	return &Error{Class: class, Op: op, Guidance: guidance}
}

func classErrorf(class Class, op, guidance string, err error) *Error {
	e := classError(class, op, guidance)
	e.err = err
	return e
}

func needInviteErr() *Error {
	return classError(ClassNeedInvite, "start", "provide an invite via hidden TTY, --invite-file, or --invite-stdin")
}

// TerminalInviteError classifies a hidden-TTY invite failure with setup guidance.
func TerminalInviteError(cause error) *Error {
	return classErrorf(ClassNeedInvite, "invite", "could not read the invite from the terminal; use --invite-file or --invite-stdin", cause)
}
