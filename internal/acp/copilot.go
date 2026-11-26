package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// copilotClientName is how scimux identifies itself to Copilot. The installed
// `copilot help acp` treats a client that names itself in clientInfo.name as
// person-driven, and a client that sends no clientInfo as automation. scimux
// is driven by a person in the browser, so it names itself and also states
// interactive explicitly, which that help says takes precedence.
const copilotClientName = "scimux"

// clientVersion is the build version scimux reports in clientInfo. The
// application sets it once at startup, before any harness launches.
var clientVersion = "dev"

// SetClientVersion supplies the build version scimux reports in clientInfo.
func SetClientVersion(v string) { clientVersion = v }

// copilotServeArgs are the arguments that make the installed Copilot CLI serve
// ACP on stdio. --no-auto-update keeps a background update from racing the
// JSON-RPC stream. The launch and the catalog probe share them, so the probe
// reads the same server the chat runs.
var copilotServeArgs = []string{"--acp", "--stdio", "--no-auto-update"}

// initializeRequest is the initialize every ACP launch and probe sends. Only
// copilot names its client and opts into person-driven classification; every
// other harness keeps the bare request it has always sent.
func initializeRequest(agent string) sdk.InitializeRequest {
	req := sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{ReadTextFile: false, WriteTextFile: false},
			Terminal: false,
		},
	}
	if agent != "copilot" {
		return req
	}
	title := copilotClientName
	req.ClientInfo = &sdk.Implementation{Name: copilotClientName, Title: &title, Version: clientVersion}
	req.ClientCapabilities.Meta = map[string]any{
		"github.com/copilot": map[string]any{"interactive": true},
	}
	return req
}

// copilotProbeTotal bounds one catalog inspection. The budgets mirror the
// Vibe probe: initialize and session/new each get a slice, and closing shares
// the total.
const (
	copilotProbeTotal      = 12 * time.Second
	copilotProbeInitialize = 8 * time.Second
	copilotProbePerCall    = 4 * time.Second
)

// copilotProbeClock is the probe's time source. Tests move it forward to show
// the probe stops before a call it has no room for.
var copilotProbeClock = time.Now

// CopilotCatalog is the model menu one disposable Copilot session advertises,
// and the effort levels known for the models it can be attributed to. Efforts
// has an entry only for the model the session reports as current, because
// the effort menu describes that model; other models stay unknown.
type CopilotCatalog struct {
	Models  []string
	Efforts map[string][]string
}

// ProbeCopilotCatalog starts the user-installed Copilot CLI in ACP mode,
// reads the model and effort menus from one session/new, closes that
// session, and reaps the process. It never calls session/prompt and never
// switches a model. A failure leaves the caller on Copilot's own default.
func ProbeCopilotCatalog(ctx context.Context, bin string) (CopilotCatalog, error) {
	if bin == "" {
		return CopilotCatalog{}, errors.New("copilot catalog: empty binary")
	}
	argv := append([]string{bin}, copilotServeArgs...)
	return probeCopilotCatalog(ctx, func(dir string) (Process, error) {
		return startGrouped(argv, dir, "copilot")
	}, copilotProbeTotal, copilotProbeInitialize, copilotProbePerCall)
}

func probeCopilotCatalog(ctx context.Context, start func(dir string) (Process, error), total, initCall, perCall time.Duration) (CopilotCatalog, error) {
	dir, err := os.MkdirTemp("", "scimux-copilot-probe-")
	if err != nil {
		return CopilotCatalog{}, err
	}
	defer os.RemoveAll(dir)
	proc, err := start(dir)
	if err != nil {
		return CopilotCatalog{}, err
	}
	defer func() {
		_ = proc.Kill()
		_ = proc.Wait()
	}()

	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	conn := sdk.NewClientSideConnection(probeClient{}, proc.Stdin(), proc.Stdout())
	if !copilotProbeRoom(ctx, initCall) {
		return CopilotCatalog{}, errors.New("copilot catalog: probe budget exhausted")
	}
	callCtx, callCancel := context.WithTimeout(ctx, initCall)
	_, err = conn.Initialize(callCtx, initializeRequest("copilot"))
	callCancel()
	if err != nil {
		return CopilotCatalog{}, err
	}
	if !copilotProbeRoom(ctx, perCall) {
		return CopilotCatalog{}, errors.New("copilot catalog: probe budget exhausted")
	}
	callCtx, callCancel = context.WithTimeout(ctx, perCall)
	resp, err := conn.NewSession(callCtx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	callCancel()
	if err != nil {
		return CopilotCatalog{}, err
	}
	if resp.SessionId == "" {
		return CopilotCatalog{}, errors.New("copilot catalog: session/new returned no session")
	}
	defer func() {
		// Closing shares the discovery budget; reaping the process stays
		// authoritative if the agent has already spent it.
		closeBudget := min(perCall, 2*time.Second)
		closeCtx, closeCancel := context.WithTimeout(ctx, closeBudget)
		_, _ = conn.CloseSession(closeCtx, sdk.CloseSessionRequest{SessionId: resp.SessionId})
		closeCancel()
	}()
	return copilotCatalogFrom(resp.ConfigOptions), nil
}

// copilotProbeRoom reports whether the probe's remaining budget covers perCall.
// Every probe runs under a deadline, so the budget is always measured.
func copilotProbeRoom(ctx context.Context, perCall time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, _ := ctx.Deadline()
	return deadline.Sub(copilotProbeClock()) >= perCall
}

// copilotCatalogFrom reads the menus a session advertised. Duplicate and blank
// ids are dropped; an absent model menu publishes no models.
func copilotCatalogFrom(opts []sdk.SessionConfigOption) CopilotCatalog {
	var cat CopilotCatalog
	var model, effort *sdk.SessionConfigOptionSelect
	for _, opt := range opts {
		if opt.Select == nil {
			continue
		}
		switch {
		case model == nil && modelSelectMatches("copilot", opt.Select):
			model = opt.Select
		case effort == nil && copilotEffortSelect(opt.Select):
			effort = opt.Select
		}
	}
	if model == nil {
		return cat
	}
	cat.Models = copilotOptionIDs(model)
	current := string(model.CurrentValue)
	levels := []string(nil)
	if effort != nil {
		levels = copilotOptionIDs(effort)
	}
	if len(levels) == 0 || !slices.Contains(cat.Models, current) {
		return cat
	}
	cat.Efforts = map[string][]string{current: levels}
	return cat
}

// copilotOptionOffered is the one availability rule for Copilot menus. Copilot
// marks an advertised option with _meta.copilotEnablement, and "enabled" is the
// value observed on an offered model. An option with no enablement key makes no
// availability claim, so it is offered. Any other value, or a non-string one, is
// not offered, and an explicit choice of it is refused: the meaning of the other
// states has not been verified, so scimux does not offer them.
func copilotOptionOffered(opt sdk.SessionConfigSelectOption) bool {
	raw, present := opt.Meta["copilotEnablement"]
	if !present {
		return true
	}
	state, ok := raw.(string)
	return ok && state == "enabled"
}

// copilotValueOffered reports whether the menu offers the given value. A value
// listed more than once is offered when any entry for it is, the same rule the
// catalog applies, so an explicit choice and the offer always agree.
func copilotValueOffered(sel *sdk.SessionConfigOptionSelect, value sdk.SessionConfigValueId) bool {
	for _, opt := range selectOptions(sel) {
		if opt.Value == value && copilotOptionOffered(opt) {
			return true
		}
	}
	return false
}

// copilotOptionIDs is the offer a menu makes: distinct, non-blank ids that are
// offered. A duplicate that is offered still counts once.
func copilotOptionIDs(sel *sdk.SessionConfigOptionSelect) []string {
	var ids []string
	seen := map[string]bool{}
	for _, opt := range selectOptions(sel) {
		id := string(opt.Value)
		if id == "" || seen[id] || !copilotOptionOffered(opt) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// copilotEffortSelect finds the reasoning-effort menu: the standard thought
// level category, or an id or name spelled as effort. Anything else is not an
// effort menu, so it is never offered as one.
func copilotEffortSelect(sel *sdk.SessionConfigOptionSelect) bool {
	if sel.Category != nil && *sel.Category == sdk.SessionConfigOptionCategoryThoughtLevel {
		return true
	}
	for _, name := range []string{string(sel.Id), sel.Name} {
		switch normalizeOptionName(name) {
		case "reasoningeffort", "effort":
			return true
		}
	}
	return false
}

// applyCopilotEffort sends the requested effort through the advertised effort
// menu. An effort with no menu, or a value the menu lacks, is a refusal: a
// requested level is never silently left at Copilot's default.
func (s *Session) applyCopilotEffort(ctx context.Context, effort string, opts []sdk.SessionConfigOption) error {
	var sel *sdk.SessionConfigOptionSelect
	for _, opt := range opts {
		if opt.Select != nil && copilotEffortSelect(opt.Select) {
			sel = opt.Select
			break
		}
	}
	if sel == nil {
		return fmt.Errorf("%w: effort %q was chosen but this agent advertises no effort option", ErrConfigRejected, effort)
	}
	var val sdk.SessionConfigValueId
	ok := false
	for _, opt := range selectOptions(sel) {
		if !copilotOptionOffered(opt) {
			continue
		}
		if strings.EqualFold(string(opt.Value), effort) || strings.EqualFold(opt.Name, effort) {
			val, ok = opt.Value, true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: effort %q is not one this agent offers", ErrConfigRejected, effort)
	}
	if _, err := s.conn.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{
		ValueId: &sdk.SetSessionConfigOptionValueId{ConfigId: sel.Id, SessionId: s.sessionID, Value: val},
	}); err != nil {
		return configSetError(err, "effort", effort)
	}
	return nil
}

// copilotPermissionQueueCap bounds the copilot requests held behind the one
// shown. Overflow is refused, never queued without limit.
const copilotPermissionQueueCap = 8

// copilotTurnUsage pairs Copilot's turn-end totals with its occupancy shell.
// The totals are spend across the turn's model calls, so they never become the
// gauge: the turn's last shell occupancy does, and Used is 0 when no shell
// arrived. Copilot reports no cost, and the shared pairing policy keeps cost
// and cache fields empty; the shell's size wins over the advertised window.
func copilotTurnUsage(ev *UsageEvent, shellUsed, shellSize int) *UsageEvent {
	ev = applyOpencodeUsagePairing(ev, shellUsed, shellSize)
	ev.Used = shellUsed
	return ev
}
