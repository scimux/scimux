// claude_model_probe.go — which concrete model ids this account can launch.
//
// The old answer cost an API turn: `claude -p` asked the model to recite its
// own catalog (measured 20146 input tokens, ~$0.02, once per 7 days, on
// whatever model the user's settings happened to default to — an Opus default
// paid several times that). It was also the wrong instrument, because it asked
// a language model to recall a list the CLI already holds locally; the CLI
// says so itself when it rejects an unknown id: "isn't described by this
// version's model catalog".
//
// This reads that catalog instead, for no tokens at all. A throwaway session
// launched with --model <candidate> renders its status line before any API
// request, and the status line states the model the CLI resolved. Nothing is
// ever submitted to the pane, so nothing is billed (measured over nine
// launches: zero API calls, zero tokens).
//
// Two facts make it work:
//
//   - The status line reports the *resolved concrete id*, not the alias it was
//     given. `--model sonnet` comes back claude-sonnet-5.
//   - An id the catalog does not know comes back with display_name echoing the
//     id itself, where a known one carries a friendly name ("Opus 5"). That is
//     the validity oracle, and it is the only signal the CLI offers.
//
// The oracle is not decoration. On 2.1.267 `--model opus` still expands to the
// mis-ordered claude-4-6-opus — family after version, an id the API refuses —
// which is the whole reason scimux resolves ids rather than passing the alias
// through. Aliases answer for the other three families; the broken one falls
// back to the /model picker, whose rows name the version the alias should have
// meant ("Opus 5" -> claude-opus-5), validated by the same oracle before use.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/tmuxsession"
)

// claudeModelMarkerName is where the status-line helper leaves the resolved
// model. It is a sibling of the usage marker, written on every render rather
// than only on a quota reading: a model id is present in every payload, where
// rate_limits appears only after the session's first API response.
const claudeModelMarkerName = "model.json"

const claudeModelMarkerMax = 4096

var errClaudeModelUnreadable = errors.New("model unavailable: status line reported no model")

type claudeModelMarker struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	At          string `json:"at"`
}

func claudeModelMarkerPath(dir string) string {
	return filepath.Join(dir, claudeModelMarkerName)
}

// claudeStatusLineModel is the model block of a status-line payload. It is
// parsed separately from the quota block so that one being absent or
// malformed never costs the other.
type claudeStatusLineModel struct {
	Model *struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
}

func parseClaudeStatusLineModel(body []byte, now time.Time) (claudeModelMarker, error) {
	var snap claudeStatusLineModel
	if json.Unmarshal(body, &snap) != nil || snap.Model == nil || snap.Model.ID == "" {
		return claudeModelMarker{}, errClaudeModelUnreadable
	}
	return claudeModelMarker{
		ID:          snap.Model.ID,
		DisplayName: snap.Model.DisplayName,
		At:          now.UTC().Format(time.RFC3339Nano),
	}, nil
}

// claudeModelIDPattern anchors what agents.go's looser claudeModelID matches
// loosely: the family must sit immediately after "claude-", so the mis-ordered
// claude-4-6-opus can never be adopted.
var claudeModelIDPattern = regexp.MustCompile(`^claude-(fable|opus|sonnet|haiku)-\d+(?:-\d+)*$`)

// claudeModelResolved applies the validity oracle. A display name equal to the
// id is the CLI saying it does not know that model, and an id whose shape we
// cannot read is one we must not hand to a launch.
func claudeModelResolved(m claudeModelMarker) (family, id string, ok bool) {
	if m.ID == "" || m.DisplayName == m.ID {
		return "", "", false
	}
	match := claudeModelIDPattern.FindStringSubmatch(m.ID)
	if match == nil {
		return "", "", false
	}
	return match[1], m.ID, true
}

var claudeModelDisplayPattern = regexp.MustCompile(`^(Fable|Opus|Sonnet|Haiku)\s+(\d+(?:\.\d+)*)$`)

// claudeModelIDFromDisplay turns a picker label into a candidate id. It is a
// guess by construction — display names are lossy, and both claude-fable-5 and
// claude-fable-5-1 render as "Fable 5" — so every result is a candidate to be
// validated by a launch, never an answer.
func claudeModelIDFromDisplay(display string) string {
	m := claudeModelDisplayPattern.FindStringSubmatch(strings.TrimSpace(display))
	if m == nil {
		return ""
	}
	return "claude-" + strings.ToLower(m[1]) + "-" + strings.ReplaceAll(m[2], ".", "-")
}

var (
	claudeModelPickerRow     = regexp.MustCompile(`\d+\.\s+(\S+)`)
	claudeModelPickerVersion = regexp.MustCompile(`\b(Fable|Opus|Sonnet|Haiku)\s+(\d+(?:\.\d+)*)`)
)

// parseClaudeModelPicker reads a captured /model pane into family -> candidate
// id. Defensive like every other CLI-output parser here: an unfamiliar pane
// yields nothing rather than a partial guess.
//
// Two row kinds are skipped deliberately. A "(disabled)" row names a model
// this build cannot select, so adopting it would hand the launcher an id that
// fails at the API. The "Default (recommended)" row is not a family — it
// mirrors whichever family is currently default, and reading it would let the
// user's own default silently overwrite another family's id.
func parseClaudeModelPicker(pane string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(pane, "\n") {
		if strings.Contains(line, "(disabled)") {
			continue
		}
		row := claudeModelPickerRow.FindStringSubmatch(line)
		if row == nil {
			continue
		}
		alias := strings.ToLower(strings.Trim(row[1], "."))
		switch alias {
		case "fable", "opus", "sonnet", "haiku":
		default:
			continue
		}
		version := claudeModelPickerVersion.FindStringSubmatch(line)
		if version == nil || strings.ToLower(version[1]) != alias {
			continue
		}
		id := claudeModelIDFromDisplay(version[1] + " " + version[2])
		if id == "" {
			continue
		}
		if _, seen := out[alias]; !seen {
			out[alias] = id
		}
	}
	return out
}

// readClaudeModelMarker reads what the status line last resolved. A marker
// with no id is not a reading; it could only have come from a writer that is
// not this helper.
func readClaudeModelMarker(dir string) (claudeModelMarker, bool) {
	var empty claudeModelMarker
	if dir == "" {
		return empty, false
	}
	path := claudeModelMarkerPath(dir)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > claudeModelMarkerMax {
		return empty, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b) > claudeModelMarkerMax {
		return empty, false
	}
	var m claudeModelMarker
	if json.Unmarshal(b, &m) != nil || m.ID == "" {
		return empty, false
	}
	return m, true
}

// claudeModelProbeArgv builds a launch that resolves one candidate id and
// submits nothing. There is no prompt and no -p: a prompt would provoke an API
// response (the cost this probe exists to avoid), and headless -p renders no
// status line at all, which is where the answer comes from.
//
// --setting-sources ” is isolation, not economy: the user's own status line
// would replace ours, and their hooks have no business firing in a probe.
func claudeModelProbeArgv(settingsPath, candidate string) string {
	return strings.Join([]string{
		claudeFeedbackSurveySuppression,
		"claude",
		"--settings", shellQuote(settingsPath),
		"--setting-sources", shellQuote(""),
		"--tools", shellQuote(""),
		"--model", shellQuote(candidate),
	}, " ")
}

// runClaudeModelProbe launches one throwaway session and returns the model the
// CLI resolved the candidate to. The marker is removed first and its
// reappearance is the signal: a marker left by an earlier candidate would
// otherwise resolve every family to whatever was probed first.
func runClaudeModelProbe(ctx context.Context, opts claudeProbeOptions, candidate string) (claudeModelMarker, error) {
	if candidate == "" {
		return claudeModelMarker{}, errClaudeModelUnreadable
	}
	if err := os.Remove(claudeModelMarkerPath(opts.Dir)); err != nil && !os.IsNotExist(err) {
		return claudeModelMarker{}, errClaudeModelUnreadable
	}
	var got claudeModelMarker
	err := runClaudeProbeSession(ctx, opts, func(settings string) string {
		return claudeModelProbeArgv(settings, candidate)
	}, func(*tmuxsession.Session) bool {
		m, ok := readClaudeModelMarker(opts.Dir)
		got = m
		return ok
	})
	if err != nil {
		return claudeModelMarker{}, errClaudeModelUnreadable
	}
	return got, nil
}

// claudeModelAliases are the families scimux offers, in the order the static
// fallback in agents.go lists them.
var claudeModelAliases = []string{"fable", "opus", "sonnet", "haiku"}

// claudeModelResolver turns families into concrete ids in two rounds. The
// indirection is what lets the rounds be tested without launching anything.
type claudeModelResolver struct {
	probe  func(ctx context.Context, candidate string) (claudeModelMarker, error)
	picker func(ctx context.Context) map[string]string
}

// resolve asks each alias what it expands to, then falls back to the /model
// picker for whatever the aliases could not answer.
//
// Round one is authoritative when it succeeds: the CLI resolved the alias
// itself, so there is no guessing. Round two exists only because that
// resolution is buggy for at least one family, and it is skipped entirely when
// round one answered for all of them — a picker capture costs a session.
//
// A family is dropped rather than guessed at. The caller then passes the bare
// alias, which is what scimux did before any of this existed.
func (r claudeModelResolver) resolve(ctx context.Context) map[string]string {
	out := map[string]string{}
	var unresolved []string
	for _, alias := range claudeModelAliases {
		if id, ok := r.validate(ctx, alias, alias); ok {
			out[alias] = id
			continue
		}
		unresolved = append(unresolved, alias)
	}
	if len(unresolved) == 0 || r.picker == nil {
		return out
	}
	candidates := r.picker(ctx)
	for _, alias := range unresolved {
		candidate := candidates[alias]
		if candidate == "" {
			continue
		}
		if id, ok := r.validate(ctx, candidate, alias); ok {
			out[alias] = id
		}
	}
	return out
}

// validate probes one candidate and accepts it only if the CLI came back with
// a model it knows, in the family we asked about. The family check is not
// pedantry: an answer from another family would relabel a model, so picking
// "opus" in scimux would quietly launch Sonnet.
func (r claudeModelResolver) validate(ctx context.Context, candidate, want string) (string, bool) {
	if r.probe == nil {
		return "", false
	}
	m, err := r.probe(ctx, candidate)
	if err != nil {
		return "", false
	}
	family, id, ok := claudeModelResolved(m)
	if !ok || family != want {
		return "", false
	}
	return id, true
}

// claudeModelPickerDirName is the picker's own probe directory, a child of the
// shared one. Not tidiness — it is what makes the readiness oracle true.
//
// The picker is the only probe whose readiness is a marker's *existence*; a
// candidate probe reads the marker's contents and a stale one fails the family
// check. Measured on 2.1.267: a candidate probe's status line writes the
// shared marker once more after its session is killed, so it lands after the
// picker cleared the directory and is indistinguishable from the picker's own.
// Readiness then fires before the picker's TUI exists, the single /model paste
// is discarded (it is deliberately never retried), and the capture spends its
// whole budget on a pane that will never draw a picker — dropping exactly the
// family the picker exists to name. A directory whose only writer is this
// session removes the ambiguity instead of racing it.
const claudeModelPickerDirName = "picker"

func claudeModelPickerDir(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, claudeModelPickerDirName)
}

// claudeModelPickerCommand opens the picker. It is the only thing this probe
// ever submits, and it is a read: the picker's own footer says Enter sets the
// default, so no key is ever pressed on a row and the session is killed with
// the picker still open, which changes nothing persistent.
const claudeModelPickerCommand = "/model"

// claudeModelPickerArgv launches the session the picker is read from. Unlike
// the candidate probe it pins no model: the answer is what the picker draws,
// and a --model the CLI rejects can change that.
func claudeModelPickerArgv(settingsPath string) string {
	return strings.Join([]string{
		claudeFeedbackSurveySuppression,
		"claude",
		"--settings", shellQuote(settingsPath),
		"--setting-sources", shellQuote(""),
		"--tools", shellQuote(""),
	}, " ")
}

// captureClaudeModelPicker reads the /model picker's rows, or nothing.
//
// Readiness is the model marker, not a guess at the TUI's own chrome: the
// status line renders once the session is up and writes that file, so its
// arrival is mechanical proof that a paste will land rather than be discarded
// by a terminal still starting. That ordering is the whole reason this is not
// a sleep.
//
// The submission happens exactly once. A pane that never draws a picker times
// out to an empty map — defensive like every other CLI-output reader here,
// because a resolver that could not name a family falls back to the bare alias
// and the user sees no failure at all.
func captureClaudeModelPicker(ctx context.Context, opts claudeProbeOptions) map[string]string {
	opts.Dir = claudeModelPickerDir(opts.Dir)
	if err := os.Remove(claudeModelMarkerPath(opts.Dir)); err != nil && !os.IsNotExist(err) {
		return nil
	}
	var rows map[string]string
	opened := false
	err := runClaudeProbeSession(ctx, opts, claudeModelPickerArgv, func(sess *tmuxsession.Session) bool {
		if !opened {
			if _, up := readClaudeModelMarker(opts.Dir); !up {
				return false
			}
			opened = true
			// A failed send is not retried: a second /model would land in a
			// picker that did open, where it is text in a filter box.
			_ = sess.Send(claudeModelPickerCommand)
			return false
		}
		pane, err := sess.Capture()
		if err != nil {
			return false
		}
		got := parseClaudeModelPicker(pane)
		if len(got) == 0 {
			return false
		}
		rows = got
		return true
	})
	if err != nil {
		return nil
	}
	return rows
}

// claudeModelProbeTimeout bounds one candidate probe. It is a session start,
// not a round trip: the status line renders under a second on the measured
// build, so this is slack, not a budget.
const claudeModelProbeTimeout = 20 * time.Second

// installClaudeModelProbe wires the real resolver onto the app. Only the serve
// path calls it, which is what keeps every other app value — every test's —
// unable to launch a claude session.
func (a *app) installClaudeModelProbe() {
	if a == nil || a.server == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return
	}
	a.claudeVersion = claudeCLIVersion
	a.claudeResolveModels = func(ctx context.Context) map[string]string {
		// The candidate probes share one directory: they run in sequence and
		// each validates the marker it reads against the family it asked
		// about, so a marker left by a sibling fails closed. The picker
		// cannot make that check and takes a directory of its own
		// (claudeModelPickerDirName).
		opts := claudeProbeOptions{
			Dir:      claudeProbeWorkdir(a.claudeProbeDir),
			ExecPath: exe,
			Server:   a.server,
			Timeout:  claudeModelProbeTimeout,
		}
		return claudeModelResolver{
			probe: func(ctx context.Context, candidate string) (claudeModelMarker, error) {
				return runClaudeModelProbe(ctx, opts, candidate)
			},
			picker: func(ctx context.Context) map[string]string {
				return captureClaudeModelPicker(ctx, opts)
			},
		}.resolve(ctx)
	}
}
