package acp

import (
	"context"
	"errors"
	"os"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

const (
	vibeProbeTotal   = 12 * time.Second
	vibeProbePerCall = 4 * time.Second
)

// vibeProbeModelLimit caps how many advertised models a disposable probe
// selects while collecting thinking menus. Models past the cap stay in the
// catalog with no invented levels.
var vibeProbeModelLimit = 32

// vibeProbeClock is the time source for the probe budget. Tests move it
// forward to prove the loop stops without waiting on a wall clock.
var vibeProbeClock = time.Now

// VibeCatalog is the advertised model list and, for models the probe had
// time to select, the thinking value ids that model returned. currentValue
// is not a model and is not a scimux default.
type VibeCatalog struct {
	Models  []string
	Efforts map[string][]string
}

// ProbeVibeCatalog starts the user-installed vibe-acp binary, reads the
// model and thinking menus from one disposable session, and reaps that
// process. It never calls session/prompt. A failure leaves the caller to
// launch the CLI default rather than spend a prompt on discovery.
func ProbeVibeCatalog(ctx context.Context, bin string) (VibeCatalog, error) {
	if bin == "" {
		return VibeCatalog{}, errors.New("vibe catalog: empty binary")
	}
	return probeVibeCatalog(ctx, func(dir string) (Process, error) {
		return startGrouped([]string{bin}, dir, "")
	}, vibeProbeTotal, vibeProbePerCall)
}

func probeVibeCatalog(ctx context.Context, start func(dir string) (Process, error), total, perCall time.Duration) (VibeCatalog, error) {
	dir, err := os.MkdirTemp("", "scimux-vibe-probe-")
	if err != nil {
		return VibeCatalog{}, err
	}
	defer os.RemoveAll(dir)
	proc, err := start(dir)
	if err != nil {
		return VibeCatalog{}, err
	}
	defer func() {
		_ = proc.Kill()
		_ = proc.Wait()
	}()

	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	client := probeClient{}
	conn := sdk.NewClientSideConnection(client, proc.Stdin(), proc.Stdout())
	if !probeHasRoom(ctx, perCall) {
		return VibeCatalog{}, errors.New("vibe catalog: probe budget exhausted")
	}
	callCtx, callCancel := context.WithTimeout(ctx, perCall)
	_, err = conn.Initialize(callCtx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	})
	callCancel()
	if err != nil {
		return VibeCatalog{}, err
	}
	if !probeHasRoom(ctx, perCall) {
		return VibeCatalog{}, errors.New("vibe catalog: probe budget exhausted")
	}
	callCtx, callCancel = context.WithTimeout(ctx, perCall)
	resp, err := conn.NewSession(callCtx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	callCancel()
	if err != nil {
		return VibeCatalog{}, err
	}
	if resp.SessionId == "" {
		return VibeCatalog{}, errors.New("vibe catalog: session/new returned no session")
	}
	defer func() {
		// Closing is cleanup after the discovery budget. Give it a short,
		// separate bound so a stalled close cannot hold startup indefinitely.
		closeBudget := min(perCall, 2*time.Second)
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeBudget)
		_, _ = conn.CloseSession(closeCtx, sdk.CloseSessionRequest{SessionId: resp.SessionId})
		closeCancel()
	}()

	cat := VibeCatalog{}
	sel := vibeModelSelect(resp.ConfigOptions)
	if sel == nil {
		return cat, nil
	}
	cat.Models = optionValueIDs(sel)
	if len(cat.Models) == 0 {
		return cat, nil
	}
	cat.Efforts = map[string][]string{}
	for i, model := range cat.Models {
		if i >= vibeProbeModelLimit || !probeHasRoom(ctx, perCall) {
			break
		}
		callCtx, callCancel = context.WithTimeout(ctx, perCall)
		switched, setErr := conn.SetSessionConfigOption(callCtx, sdk.SetSessionConfigOptionRequest{
			ValueId: &sdk.SetSessionConfigOptionValueId{
				ConfigId:  sel.Id,
				SessionId: resp.SessionId,
				Value:     sdk.SessionConfigValueId(model),
			},
		})
		callCancel()
		if setErr != nil {
			break
		}
		if think := bestVibeThinking(switched.ConfigOptions); think != nil {
			if levels := optionValueIDs(think); len(levels) > 0 {
				cat.Efforts[model] = levels
			}
		}
	}
	if len(cat.Efforts) == 0 {
		cat.Efforts = nil
	}
	return cat, nil
}

func probeHasRoom(ctx context.Context, perCall time.Duration) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return deadline.Sub(vibeProbeClock()) >= perCall
}

func vibeModelSelect(opts []sdk.SessionConfigOption) *sdk.SessionConfigOptionSelect {
	for _, opt := range opts {
		if opt.Select != nil && modelSelectMatches("vibe", opt.Select) {
			return opt.Select
		}
	}
	return nil
}

func optionValueIDs(sel *sdk.SessionConfigOptionSelect) []string {
	var ids []string
	seen := map[string]bool{}
	for _, opt := range selectOptions(sel) {
		id := string(opt.Value)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// probeClient is the ACP client for a disposable catalog session. It ignores
// session updates, cancels every permission, and declines filesystem and
// terminal access. Discovery must not approve work or touch the host.
type probeClient struct{}

func (probeClient) SessionUpdate(context.Context, sdk.SessionNotification) error { return nil }

func (probeClient) RequestPermission(context.Context, sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
}

func (probeClient) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}

func (probeClient) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}

func (probeClient) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}

func (probeClient) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}

func (probeClient) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}

func (probeClient) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}

func (probeClient) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}
