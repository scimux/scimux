// Package remote is the scimux computer half of remote access (S5).
//
// R2: enrollment, identity, local device controls, and fail-closed state.
// Methods perform no WebRTC and import no pion.
package remote

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ErrUnimplemented is the single R1 stub result. It is not a Class error
// and cannot satisfy an acceptance row that names a specific class.
var ErrUnimplemented = errors.New("remote: unimplemented")

// ProtocolVersion is the checked-in rendezvous Protocol-Version (docs/protocol/rendezvous-v1.md).
const ProtocolVersion = 1

// MinRequestV is the checked-in minimum accepted request v. Below this is
// refused, never downgraded (FR-37).
const MinRequestV = 1

// DefaultOrigin is the rendezvous service origin the protocol vectors use.
//
// It is cryptographic material, not a URL. buildAuthMessage domain-separates
// on it, BuildPairingTranscript binds it into the SAS both humans read aloud,
// and persist.go writes it into the enrolled state that client.go then
// refuses to run against a different one. Change it and every installation
// already enrolled must unlink and enroll afresh.
//
// Point a test or a local rehearsal somewhere
// else with --rendezvous-url; never by editing this line.
//
// It read my.scimux.com before the independent-viewer migration. That change
// intentionally requires the one enrolled installation to unlink at the old
// service and enroll afresh; editing its saved origin is not a migration.
// Three more places must agree with this value: -origin in scimux-rv's rc.d,
// the Caddyfile service name, and the vendored vectors under testdata/vectors,
// which scimux-rv generates and this repository byte-copies per
// docs/rendezvous-protocol-sync.md. Editing this constant alone turns every
// admission into a silent 404.
const DefaultOrigin = "https://rv.scimux.com"

// PrivateDirName is the owner-only directory under the data root that holds
// installation identity and remote state (0700).
const PrivateDirName = "remote"

// StateFileName is the identity/state file inside PrivateDirName (0600).
const StateFileName = "state"

// RendezvousIDBits is FR-04/NFR-17: 256-bit canonical rendezvous IDs.
const RendezvousIDBits = 256

// RendezvousIDHexLen is the canonical encoding length (lowercase hex).
const RendezvousIDHexLen = RendezvousIDBits / 4

// WaitDefaultMaxMS is the protocol long-poll default for POST /v1/wait
// `max_ms` (docs/protocol/rendezvous-v1.md §9.2). Production waits send
// this duration; vectors may use 1 ms only to make an empty 204 executable.
const WaitDefaultMaxMS = 60000

// Class names a fail-closed remote error. Tests match on Class, not strings.
type Class string

const (
	ClassNeedInvite          Class = "need-invite"
	ClassInviteFormat        Class = "invite-format"
	ClassInviteFile          Class = "invite-file"
	ClassKeyMissing          Class = "key-missing"
	ClassPartialIdentity     Class = "partial-identity"
	ClassCorruptIdentity     Class = "corrupt-identity"
	ClassAmbiguousEnrollment Class = "ambiguous-enrollment"
	// ClassEnrollRejected is the rendezvous answering the §7 constant
	// rejection to /v1/enroll. Every path that writes it returns before
	// Bind, so — unlike ClassAmbiguousEnrollment — it is positive evidence
	// that nothing was bound: the invite is unspent and a rerun is the
	// whole recovery.
	ClassEnrollRejected Class = "enroll-rejected"
	// ClassUnreachable is the pre-flight probe (§4.0) failing to get any
	// answer at all: DNS, dial, TLS, or a timeout. It is deliberately not
	// ClassAmbiguousEnrollment — nothing was sent, so nothing is in doubt,
	// the invite is untouched, and the fix is the user's network or their
	// --rendezvous-url, never an operator's.
	ClassUnreachable Class = "unreachable"
	// ClassVersionMismatch is a rendezvous that answered but named a
	// version window this build is outside of. Enrolling could only be
	// refused, so the invite is not spent to discover that.
	ClassVersionMismatch Class = "version-mismatch"
	ClassInviteConflict  Class = "invite-conflict"
	ClassRevoked         Class = "revoked"
	ClassDisabled        Class = "disabled"
	ClassPeerAbsent      Class = "peer-absent"
	ClassDowngrade       Class = "downgrade"
	ClassStateLock       Class = "state-lock"
	ClassNotFound        Class = "not-found"
	ClassUnauthorized    Class = "unauthorized"
	ClassUnavailable     Class = "unavailable"
	// ClassOriginMismatch is an enrolled identity being presented to a
	// rendezvous other than the one that issued it. The server cannot report
	// this — an unknown handle gets the same opaque rejection as a revoked one
	// — so the client refuses before the request.
	ClassOriginMismatch Class = "origin-mismatch"
)

// Error is a classified remote failure. Guidance is operator-facing and
// must never contain invite plaintext, private keys, or rendezvous IDs.
type Error struct {
	Class    Class
	Op       string
	Guidance string
	err      error
}

func (e *Error) Error() string {
	if e == nil {
		return "remote: error"
	}
	msg := "remote: " + string(e.Class)
	if e.Op != "" {
		msg = "remote: " + e.Op + ": " + string(e.Class)
	}
	if e.Guidance != "" {
		msg += ": " + e.Guidance
	}
	if e.err != nil && e.err.Error() != "" {
		msg += ": " + e.err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || e == nil || t == nil {
		return false
	}
	return e.Class == t.Class
}

// EnrollmentState is the FR-30 explicit state.
type EnrollmentState string

const (
	StateAbsent     EnrollmentState = "absent"
	StateEnrolled   EnrollmentState = "enrolled"
	StateKeyMissing EnrollmentState = "key-missing"
	StatePartial    EnrollmentState = "partial"
	StateCorrupt    EnrollmentState = "corrupt"
	StateAmbiguous  EnrollmentState = "ambiguous"
	StateRevoked    EnrollmentState = "revoked"
	StateDisabled   EnrollmentState = "disabled"
)

// PersistedState is the identity/state file schema. Tests write fixtures
// of this shape; R2 must round-trip it atomically.
type PersistedState struct {
	V          int                `json:"v"`
	Status     EnrollmentState    `json:"status"`
	Handle     string             `json:"handle,omitempty"`
	PublicKey  string             `json:"public_key,omitempty"`
	PrivateKey string             `json:"private_key,omitempty"`
	Origin     string             `json:"origin,omitempty"`
	Devices    []PersistedDevice  `json:"devices,omitempty"`
	Pending    *PendingEnrollment `json:"pending,omitempty"`
}

// PersistedDevice is one paired device record (no WebRTC).
type PersistedDevice struct {
	ID      string `json:"id"`
	RID     string `json:"rid"`
	PubKey  string `json:"public_key,omitempty"`
	ECDHPub string `json:"ecdh_public_key,omitempty"`
	// Label and PairedAt are the FR-38 list's presentation, not credentials.
	// They are durable because a device the human cannot see is a device the
	// human cannot revoke: before they were persisted, a restart left the
	// Remote-access list empty while the wait loop kept polling the pairings.
	// Both are omitempty and both are optional on read — a file written
	// before they existed rehydrates with an empty label and a zero time.
	Label    string `json:"label,omitempty"`
	PairedAt string `json:"paired_at,omitempty"` // RFC 3339
}

// PendingEnrollment is partial-enrollment evidence (AT-FR-02-e). It must
// not contain invite plaintext.
type PendingEnrollment struct {
	PublicKey string `json:"public_key"`
	BoundAt   string `json:"bound_at,omitempty"`
}

// PendingWork is in-flight handshake/envelope/reply state for one device.
type PendingWork struct {
	Handshake []byte
	Envelope  []byte
	Reply     []byte
}

// Empty reports whether no handshake, envelope, or reply is outstanding.
func (p PendingWork) Empty() bool {
	return len(p.Handshake) == 0 && len(p.Envelope) == 0 && len(p.Reply) == 0
}

// WriteStep is one structural step of an atomic identity/state write (FR-30).
type WriteStep int

const (
	WriteTempCreate WriteStep = iota
	WriteData
	WriteSync
	WriteChmod
	WriteRename
	WriteParentSync
)

func (s WriteStep) String() string {
	switch s {
	case WriteTempCreate:
		return "temp-create"
	case WriteData:
		return "write"
	case WriteSync:
		return "sync"
	case WriteChmod:
		return "chmod"
	case WriteRename:
		return "rename"
	case WriteParentSync:
		return "parent-sync"
	default:
		return fmt.Sprintf("write-step(%d)", int(s))
	}
}

// AllWriteSteps is the FR-30 injected-failure table, in order.
func AllWriteSteps() []WriteStep {
	return []WriteStep{
		WriteTempCreate,
		WriteData,
		WriteSync,
		WriteChmod,
		WriteRename,
		WriteParentSync,
	}
}

// WriteFault is occurrence- or state-specific atomic-write injection.
// A zero When matches every persist; Skip is how many matching writes to
// allow before failing (0 = fail the first match).
type WriteFault struct {
	Step WriteStep
	When EnrollmentState
	Skip int
}

// FileStat is the injectable lstat view used by --invite-file checks (FR-02-d).
type FileStat struct {
	Mode    os.FileMode
	UID     int
	Regular bool
}

// Sys is the effective-UID and stat seam. Ownership tests inject this so
// they stay deterministic when the test binary runs as root.
type Sys interface {
	EffectiveUID() int
	Lstat(path string) (FileStat, error)
	ReadFile(path string) ([]byte, error)
}

// Terminal is the hidden TTY invite prompt. Echo must be off for the
// duration of the read and restored on both success and failure.
type Terminal interface {
	DisableEcho() error
	RestoreEcho() error
	EchoEnabled() bool
	ReadLine() (string, error)
	WritePrompt([]byte) error
}

// Clock is an injectable clock. Backoff tests must not sleep on the wall clock.
type Clock interface {
	Now() time.Time
}

// RNG is injectable randomness for backoff jitter.
type RNG interface {
	Float64() float64
}

// Scheduler runs one delayed callback. Cancel stops a pending invocation.
type Scheduler interface {
	After(d time.Duration, fn func()) (cancel func())
}

// Channel is a fake or real live device channel. S5 uses fakes only.
// Send is the delivery seam: Client.Deliver must call it on a live
// channel and must return a send error rather than treat it as success
// (AT-FR-24-b).
type Channel interface {
	Close() error
	Closed() bool
	Send([]byte) error
}

// Waiter is a fake rendezvous wait/ID registration.
type Waiter interface {
	ID() string
	Cancel()
	Cancelled() bool
}

// Gate is an event barrier for linearizability tests (AT-FR-29-a, AT-FR-32-b).
// Production callers pass nil. Timeouts in tests diagnose hangs; they do
// not express the ordering property.
type Gate struct {
	Arrived   chan struct{}
	Release   chan struct{}
	Done      chan struct{}
	Published chan struct{}
}

// Hooks are optional instrumentation points. Stubs never call them.
type Hooks struct {
	OnRemoteInit        func()
	OnStateLoad         func()
	OnIdentityGenerated func(ed25519.PublicKey)
	OnEnrollAttempt     func()
	OnHandleReceived    func(handle string)
	OnHookDispatch      func(name string)
	OnRendezvousStop    func()
	OnAgentTouch        func()
	OnAdmission         func(deviceID string)
	OnPendingErase      func(deviceID string)
}

// Config is the injectable client runtime. Flag parsing belongs to Command.
type Config struct {
	DataDir      string
	Origin       string
	ViewerOrigin string

	Remote      bool
	InviteFile  string
	InviteStdin bool
	// InviteString is a test/programmatic stand-in for an already-read
	// invite. Production input is TTY, --invite-file, or --invite-stdin.
	InviteString string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Terminal Terminal
	Sys      Sys
	Clock    Clock
	RNG      RNG
	Rand     io.Reader
	// NewTerminal, if set, is the production hidden-TTY factory. Command
	// uses it for a new remote enrollment when no invite file/stdin is set.
	NewTerminal func() (Terminal, error)
	// Scheduler, if set, runs delayed work. Tests inject an event-driven
	// implementation so backoff retries do not sleep on the wall clock.
	Scheduler Scheduler

	// TunnelHandler is the S3 tunnel boundary a live session serves. It is
	// configuration rather than a call argument because the wait loop is what
	// receives a device's session offer: by then there is no caller left to
	// hand one in. Nil means the installation cannot serve a tunnel, and an
	// arriving offer is refused rather than answered into nothing.
	//
	// Deprecated in favour of TunnelHandlerFor, which is consulted first. A
	// bare handler cannot know which device it is answering, so the S3
	// boundary's peer argument would be dead in production; the answering path
	// already holds the device id and RID it proved.
	TunnelHandler http.Handler

	// TunnelHandlerFor builds the S3 tunnel boundary for one proven peer.
	// Returning nil for a peer refuses that session without disabling the
	// installation.
	TunnelHandlerFor func(TunnelPeer) http.Handler

	HTTPClient    *http.Client
	RendezvousURL string
	FailWrite     *WriteFault
	// BeforeInviteUnlink, if set, runs after the invite file's opened
	// identity has been checked and immediately before the pathname is
	// revalidated for Unlinkat. Tests install a replacement at this seam.
	BeforeInviteUnlink func()
	// BeforeInviteUnlinkFinal, if set, runs after the last identity
	// check and immediately before the identity-bound consume of the
	// invite directory entry. Tests that must not use the earlier R4
	// seam inject a replacement here.
	BeforeInviteUnlinkFinal func()
	// BeforeInviteRestore, if set, runs after a quarantined directory
	// entry has been identified as not the validated invite inode and
	// immediately before the no-replace restore to the original name.
	// Tests create a second file at the original pathname here.
	BeforeInviteRestore func()
	// BeforeEnrollRequest, if set, runs after enrollPosted is set and
	// immediately before the enroll HTTP request is sent. Tests move or
	// replace the invite pathname during enrollment here.
	BeforeEnrollRequest func()
	// InviteUnlink, if set, replaces the final removal of the quarantined
	// invite during consume. Names are relative to the invite's directory.
	// Intermediate removes — dropping the original name after linking, and
	// undoing a link — go straight to the root; a seam that covers some
	// instances of an operation and not others would otherwise read as a
	// bug. Tests inject cleanup failure.
	InviteUnlink func(name string) error
	// InviteLink, if set, replaces every hard link during invite consume —
	// both the claim attempts and the restore. Names are relative to the
	// invite's directory. Tests inject cleanup failure.
	InviteLink func(oldname, newname string) error
	// BeforePersist, if set, runs after the state lock is held and
	// immediately before the identity/state file is written. Tests use
	// it as an event barrier.
	BeforePersist func()
	// OnLockHeld, if set, runs once this client owns or shares the
	// state lock inside withStateLock. Tests use it as an event barrier.
	OnLockHeld func()
	Version    *int
	MinVersion *int
	Backoff    BackoffConfig
	SuccessFor time.Duration
	Hooks      Hooks

	// LockHeldFile, if set, is created once this process owns the state lock
	// and before enrollment proceeds. LockReleaseFile, if set, is waited on
	// after that announcement. Tests use the pair as an event barrier.
	LockHeldFile    string
	LockReleaseFile string
}

// DeviceRecord is a local device/channel record. No WebRTC.
type DeviceRecord struct {
	ID  string
	RID string
	// PubKey is an optional legacy Ed25519 extension. The rendezvous-owned
	// browser does not send it and no session request is authenticated by it.
	PubKey []byte
	// ECDHPub is the device's static P-256 public key Y from the §11 pairing
	// transcript. A §12.2 reply envelope is sealed to Y; without it the
	// computer can open a device's session offer and has nowhere to send the
	// answer.
	ECDHPub []byte
	// Label and PairedAt carry the FR-38 list's presentation through the
	// durable record so a restart can rebuild it. Nothing on the signalling
	// path reads them.
	Label    string
	PairedAt time.Time
}

// BackoffConfig is exponential backoff plus bounded jitter (FR-22).
type BackoffConfig struct {
	Initial    time.Duration
	Max        time.Duration
	Factor     float64
	Jitter     float64
	SuccessFor time.Duration
}

// DefaultBackoff is the retry shape every production client runs. It is a
// value rather than five literals at each construction site because a caller
// that omits it gets a client that looks completely healthy — enrolled,
// minting codes, drawing QR codes — and never contacts the rendezvous at all.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		Initial:    100 * time.Millisecond,
		Max:        1600 * time.Millisecond,
		Factor:     2,
		Jitter:     0.2,
		SuccessFor: 5 * time.Second,
	}
}

// RunsRendezvousLoop reports whether a client built from this config will run
// the wait loop, which is what registers device and pairing waiters with the
// rendezvous. Unconfigured means off, so that the many tests which build a
// client to exercise local behaviour never reach the network; production must
// therefore opt in with DefaultBackoff (or, in tests, a Scheduler). The
// predicate is exported because the cost of getting it wrong lands on the
// other side of the process boundary, where only a person with a phone in
// their hand can see it.
func (c Config) RunsRendezvousLoop() bool {
	return c.Backoff.Initial > 0 || c.Scheduler != nil
}
