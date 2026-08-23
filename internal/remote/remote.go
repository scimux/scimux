// Package remote is the scimux laptop half of remote access (S5).
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

// DefaultOrigin is the rendezvous origin the protocol vectors use.
const DefaultOrigin = "https://my.scimux.eu"

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
	ClassInviteConflict      Class = "invite-conflict"
	ClassRevoked             Class = "revoked"
	ClassDisabled            Class = "disabled"
	ClassPeerAbsent          Class = "peer-absent"
	ClassDowngrade           Class = "downgrade"
	ClassStateLock           Class = "state-lock"
	ClassNotFound            Class = "not-found"
	ClassUnauthorized        Class = "unauthorized"
	ClassUnavailable         Class = "unavailable"
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
	DataDir string
	Origin  string

	Remote      bool
	InviteFile  string
	InviteStdin bool
	// InviteString is a test/programmatic stand-in for an already-read
	// invite. Production input is TTY, --invite-file, or --invite-stdin.
	InviteString string
	// ExplicitRetry is the operator-driven recovery of AT-FR-02-e. Restart
	// must not set this implicitly.
	ExplicitRetry bool
	// Rotate is the explicit identity rotation action. A different invite
	// on an enrolled installation without Rotate is ClassInviteConflict.
	Rotate bool

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
	// Unlinkat, if set, replaces syscall.Unlinkat during invite consume.
	// Tests inject cleanup failure.
	Unlinkat func(dirfd int, name string) error
	// Renameat, if set, replaces syscall.Renameat during invite consume.
	// Tests inject cleanup failure.
	Renameat func(olddirfd int, oldpath string, newdirfd int, newpath string) error
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
	ID     string
	RID    string
	PubKey []byte
	// ECDHPub is the device's static P-256 public key Y from the §11 pairing
	// transcript. PubKey is the ed25519 identity that signs; Y is what a
	// §12.2 reply envelope is sealed *to*, so the two cannot be the same
	// field. Without it the laptop can open a device's session offer and has
	// nowhere to send the answer.
	ECDHPub []byte
}

// BackoffConfig is exponential backoff plus bounded jitter (FR-22).
type BackoffConfig struct {
	Initial    time.Duration
	Max        time.Duration
	Factor     float64
	Jitter     float64
	SuccessFor time.Duration
}
