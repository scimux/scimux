package remote

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

const tempSuffix = ".tmp"

func (c *Client) tempPath() string {
	return c.StatePath() + tempSuffix
}

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
	raw, err := json.Marshal(st)
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
			ID:     d.ID,
			RID:    d.RID,
			PubKey: hex.EncodeToString(d.PubKey),
		}
	}
	return st
}

func (c *Client) applyState(st PersistedState) {
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
