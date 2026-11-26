package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/sessionworker"
)

// interactionNow is the clock for question deadlines. It is time.Now in
// production. Tests set it to hit an exact boundary. internal/app tests do
// not call t.Parallel, so one process-wide clock is safe.
var interactionNow = time.Now

// A standing question is the bounded record that remains after the hook
// helper exits. It is not hook stdout, not a consumption claim, and not a
// copy of the raw tool input. AskUserQuestion only: plan approval keeps the
// helper deadline. Status outstanding stays until a transcript retires the
// notice, the turn or session is tombstoned, or a later question replaces it.

const (
	standingOutstanding = "outstanding"
	standingExternal    = "external"
	standingInvalidated = "invalidated"
	standingMaxFiles    = 256
	standingMaxBytes    = 8 << 20
)

var (
	marshalStandingRecord = json.Marshal
	removeStandingRecord  = os.Remove
	syncStandingDirectory = sessionlog.SyncParentDir
)

type claudeStandingQuestion struct {
	ID          string                       `json:"id"`
	SessionID   string                       `json:"session_id"`
	BundleID    string                       `json:"bundle_id"`
	Generation  int                          `json:"generation"`
	NodeID      string                       `json:"node_id,omitempty"`
	Turn        string                       `json:"turn,omitempty"`
	TurnGen     int                          `json:"turn_gen,omitempty"`
	Tool        string                       `json:"tool"`
	Digest      string                       `json:"digest"`
	Status      string                       `json:"status"`
	Reason      string                       `json:"reason,omitempty"`
	SentEpoch   string                       `json:"sent_epoch,omitempty"`
	SentKey     string                       `json:"sent_key,omitempty"`
	Delivery    string                       `json:"delivery,omitempty"`
	Manual      bool                         `json:"manual,omitempty"`
	ManualEpoch string                       `json:"manual_epoch,omitempty"`
	Questions   []claudeStandingQuestionItem `json:"questions"`
}

type claudeStandingQuestionItem struct {
	ID      string                 `json:"id"`
	Text    string                 `json:"text"`
	Header  string                 `json:"header,omitempty"`
	Multi   bool                   `json:"multi"`
	Preview string                 `json:"preview,omitempty"`
	Options []claudeStandingOption `json:"options"`
}

type claudeStandingOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func standingFromModel(rec claudeInteractionRequest, model claudeInteractionModel) claudeStandingQuestion {
	st := claudeStandingQuestion{
		ID: rec.ID, SessionID: rec.SessionID, BundleID: rec.BundleID,
		Generation: rec.Generation, NodeID: rec.NodeID, Turn: rec.Turn, TurnGen: rec.TurnGen,
		Tool: model.Tool, Digest: model.Digest, Status: standingOutstanding,
	}
	for _, question := range model.Questions {
		item := claudeStandingQuestionItem{
			ID: question.ID, Text: question.Text, Header: question.Header,
			Multi: question.Multi, Preview: question.Preview,
		}
		for _, option := range question.Options {
			item.Options = append(item.Options, claudeStandingOption{
				ID: option.ID, Label: option.Label, Description: option.Description,
			})
		}
		st.Questions = append(st.Questions, item)
	}
	return st
}

// writeStandingQuestion persists the outstanding question after active.json.
// A failed write removes nothing by itself; publish rolls the request back.
func writeStandingQuestion(dir string, rec claudeInteractionRequest, model claudeInteractionModel) error {
	if model.Tool != "AskUserQuestion" || model.Native || rec.ID == "" {
		return nil
	}
	path, err := interactionChild(dir, "standing", rec.ID+".json")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: standing", errInteractionIO)
	}
	encoded, err := marshalStandingRecord(standingFromModel(rec, model))
	if err != nil {
		return fmt.Errorf("%w: standing", errInteractionIO)
	}
	if err := enforceStandingBudget(dir, filepath.Base(path), len(encoded)); err != nil {
		return err
	}
	if err := writeClaudePermFile(path, standingFromModel(rec, model)); err != nil {
		return fmt.Errorf("%w: standing", errInteractionIO)
	}
	if err := invalidateOtherStanding(dir, rec.SessionID, rec.ID); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// enforceStandingBudget bounds this hook-bundle side store independently of
// the user-history budget. Only already-resolved records may be collected.
func enforceStandingBudget(bundle, replacing string, incoming int) error {
	dir, err := interactionChild(bundle, "standing")
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: standing", errInteractionIO)
	}
	type entry struct {
		name string
		size int64
		st   claudeStandingQuestion
		ok   bool
	}
	var files []entry
	var total int64
	cleaned := false
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") || ent.Name() == replacing {
			continue
		}
		info, err := ent.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: standing", errInteractionIO)
		}
		item := entry{name: ent.Name(), size: info.Size()}
		item.st, item.ok = readStandingQuestion(bundle, strings.TrimSuffix(ent.Name(), ".json"))
		if item.size > standingMaxBytes || total > standingMaxBytes-item.size {
			return fmt.Errorf("%w: standing budget", errInteractionIO)
		}
		total += item.size
		files = append(files, item)
	}
	if incoming < 0 || int64(incoming) > standingMaxBytes {
		return fmt.Errorf("%w: standing budget", errInteractionIO)
	}
	for len(files)+1 > standingMaxFiles || total+int64(incoming) > standingMaxBytes {
		removed := false
		for i, item := range files {
			if !item.ok || (item.st.Status != standingExternal && item.st.Status != standingInvalidated) {
				continue
			}
			if err := removeStandingRecord(filepath.Join(dir, item.name)); err != nil {
				return fmt.Errorf("%w: standing cleanup", errInteractionIO)
			}
			total -= item.size
			files = append(files[:i], files[i+1:]...)
			cleaned = true
			removed = true
			break
		}
		if !removed {
			return fmt.Errorf("%w: standing budget", errInteractionIO)
		}
	}
	if cleaned {
		if err := syncStandingDirectory(filepath.Join(dir, replacing)); err != nil {
			return fmt.Errorf("%w: standing cleanup sync", errInteractionIO)
		}
	}
	return nil
}

func invalidateOtherStanding(dir, session, keepID string) error {
	dirPath, err := interactionChild(dir, "standing")
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: standing", errInteractionIO)
	}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		if id == keepID || !safePathComponent(id) {
			continue
		}
		st, ok := readStandingQuestion(dir, id)
		if !ok || st.Status != standingOutstanding || st.ID == keepID {
			continue
		}
		if session != "" && st.SessionID != "" && st.SessionID != session {
			continue
		}
		st.Status = standingInvalidated
		st.Reason = "replaced"
		// id was checked with safePathComponent and dirPath is the standing
		// directory, so this join cannot escape it.
		if err := writeClaudePermFile(filepath.Join(dirPath, id+".json"), st); err != nil {
			return fmt.Errorf("%w: standing", errInteractionIO)
		}
	}
	return nil
}

func standingOrZero(bundle, id string) claudeStandingQuestion {
	st, ok := readStandingQuestion(bundle, id)
	if !ok {
		return claudeStandingQuestion{}
	}
	return st
}

func readStandingQuestion(bundle, id string) (claudeStandingQuestion, bool) {
	var zero claudeStandingQuestion
	if !safePathComponent(id) {
		return zero, false
	}
	path, err := interactionChild(bundle, "standing", id+".json")
	if err != nil {
		return zero, false
	}
	b, err := readBounded(path, interactionFileMax)
	if err != nil {
		return zero, false
	}
	var st claudeStandingQuestion
	if json.Unmarshal(b, &st) != nil || st.ID != id || st.Tool != "AskUserQuestion" {
		return zero, false
	}
	switch st.Status {
	case standingOutstanding, standingExternal, standingInvalidated:
	default:
		return zero, false
	}
	if st.SessionID == "" || st.Digest == "" {
		return zero, false
	}
	return st, true
}

func listStandingQuestions(bundle string) []claudeStandingQuestion {
	dir, err := interactionChild(bundle, "standing")
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []claudeStandingQuestion
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		st, ok := readStandingQuestion(bundle, id)
		if !ok {
			continue
		}
		out = append(out, st)
	}
	return out
}

// standingVisibleForSession returns the one question the session may show.
// Two outstanding questions are not guessable, so the view stays empty.
// When none are outstanding, one external or invalidated record still shows
// its explicit status. Several of those are not guessable either.
func standingVisibleForSession(bundle, session string) (claudeStandingQuestion, bool) {
	outstanding, explicit := standingQuestionsForSession(bundle, session)
	if len(outstanding) == 1 {
		return outstanding[0], true
	}
	if len(outstanding) == 0 && len(explicit) == 1 {
		return explicit[0], true
	}
	return claudeStandingQuestion{}, false
}

func standingQuestionsForSession(bundle, session string) ([]claudeStandingQuestion, []claudeStandingQuestion) {
	var outstanding []claudeStandingQuestion
	var explicit []claudeStandingQuestion
	for _, st := range listStandingQuestions(bundle) {
		if session != "" && st.SessionID != session {
			continue
		}
		switch st.Status {
		case standingOutstanding:
			outstanding = append(outstanding, st)
		case standingExternal, standingInvalidated:
			explicit = append(explicit, st)
		}
	}
	return outstanding, explicit
}

// claudeAskedHeldByStanding keeps an AskUserQuestion notice past the age
// backstop while that question is still outstanding. Other tools, empty
// digests, and legacy bundles with no standing record keep the 4h deletion.
// The notice digest is the first 16 hex characters of the same SHA-256.
func claudeAskedHeldByStanding(perm string, note claudeAskedNotice) bool {
	if note.Tool != "AskUserQuestion" || note.Session == "" || len(note.Digest) != 16 {
		return false
	}
	if perm == "" {
		return false
	}
	bundle := filepath.Dir(perm)
	for _, st := range listStandingQuestions(bundle) {
		if st.Status != standingOutstanding || st.Tool != note.Tool || st.SessionID != note.Session {
			continue
		}
		if strings.HasPrefix(st.Digest, note.Digest) {
			return true
		}
	}
	return false
}

// paneMatchesQuestion is true when the current numbered labels are exactly
// the question's option labels, or those labels plus a final row "Other".
// "Other" is Claude's free-text row. It is not a choice this function emits.
func paneMatchesQuestion(labels, pane []string) bool {
	if len(labels) < 2 || len(pane) < len(labels) {
		return false
	}
	for i := range labels {
		if labels[i] == "" || labels[i] != pane[i] {
			return false
		}
	}
	trailing := pane[len(labels):]
	if len(trailing) == 0 {
		return true
	}
	if len(trailing) == 1 {
		return trailing[0] == "Other"
	}
	return len(trailing) == 2 && trailing[0] == "Other" && trailing[1] == "Chat about this"
}

func standingOptionMatches(option claudeStandingOption, name string) bool {
	if option.Label == name {
		return true
	}
	return option.Description != "" && name == option.Label+" — "+option.Description
}

func paneMatchesStandingOptions(options []claudeStandingOption, pane []string) bool {
	if len(options) < 2 || len(pane) < len(options) {
		return false
	}
	for i, option := range options {
		if option.Label == "" || !standingOptionMatches(option, pane[i]) {
			return false
		}
	}
	trailing := pane[len(options):]
	if len(trailing) == 0 {
		return true
	}
	if len(trailing) == 1 {
		return trailing[0] == "Other"
	}
	return len(trailing) == 2 && trailing[0] == "Other" && trailing[1] == "Chat about this"
}

// matchStandingMenu resolves one single-select question onto the current
// menu. Zero matches, several matches, and identical option lists are
// menu_mismatch. Every question multi-select is multi. An empty menu is
// menu_unavailable. The returned choices never include Other.
func matchStandingMenu(questions []claudeStandingQuestionItem, opts []PermOption) (string, []sessionworker.InteractionNativeChoice, string) {
	if len(questions) == 0 {
		return "", nil, "menu_mismatch"
	}
	nonMulti := 0
	for _, question := range questions {
		if !question.Multi {
			nonMulti++
		}
	}
	if nonMulti == 0 {
		return "", nil, "multi"
	}
	if len(opts) == 0 {
		return "", nil, "menu_unavailable"
	}
	pane := make([]string, len(opts))
	for i, opt := range opts {
		pane[i] = opt.Name
	}
	var matched []claudeStandingQuestionItem
	for _, question := range questions {
		if question.Multi || len(question.Options) == 0 {
			continue
		}
		if paneMatchesStandingOptions(question.Options, pane) {
			matched = append(matched, question)
		}
	}
	if len(matched) != 1 {
		return "", nil, "menu_mismatch"
	}
	question := matched[0]
	choices := make([]sessionworker.InteractionNativeChoice, 0, len(question.Options))
	seenLabels := make(map[string]bool, len(question.Options))
	for i, option := range question.Options {
		if i >= len(opts) || !standingOptionMatches(option, opts[i].Name) || option.ID == "" || option.Label == "" || seenLabels[option.Label] {
			return "", nil, "menu_mismatch"
		}
		seenLabels[option.Label] = true
		choices = append(choices, sessionworker.InteractionNativeChoice{
			QuestionID: question.ID, OptionID: option.ID, Key: opts[i].Key, Label: option.Label,
		})
	}
	return question.ID, choices, ""
}

// standingMenuMatchesLabels fails closed when a refreshed dialog still shows
// the standing question's labels but its prompt no longer matches. Matching
// labels only rejects an ambiguous key path; it never establishes identity.
func standingMenuMatchesLabels(questions []claudeStandingQuestionItem, opts []PermOption) bool {
	if len(opts) == 0 {
		return false
	}
	for _, question := range questions {
		if len(question.Options) > len(opts) {
			continue
		}
		extra := opts[len(question.Options):]
		if len(extra) > 0 && !(len(extra) == 1 && extra[0].Name == "Other") &&
			!(len(extra) == 2 && extra[0].Name == "Other" && extra[1].Name == "Chat about this") {
			continue
		}
		pane := make([]string, len(opts))
		for i, option := range opts {
			pane[i] = option.Name
		}
		if paneMatchesStandingOptions(question.Options, pane) {
			return true
		}
	}
	return false
}

func (a *app) projectStandingQuestion(n *Node, st claudeStandingQuestion) (sessionworker.InteractionView, bool) {
	var zero sessionworker.InteractionView
	if n == nil || st.ID == "" || st.Tool != "AskUserQuestion" {
		return zero, false
	}
	if n.SessionID != "" && st.SessionID != n.SessionID {
		return zero, false
	}
	view := sessionworker.InteractionView{
		RequestID: st.ID, Tool: st.Tool, Digest: st.Digest, Status: st.Status,
	}
	for _, question := range st.Questions {
		item := sessionworker.InteractionQuestion{
			ID: question.ID, Text: question.Text, Header: question.Header,
			Multi: question.Multi, Preview: question.Preview,
		}
		for _, option := range question.Options {
			item.Options = append(item.Options, sessionworker.InteractionOption{
				ID: option.ID, Label: option.Label, Description: option.Description,
			})
		}
		view.Questions = append(view.Questions, item)
	}
	switch st.Status {
	case standingInvalidated:
		view.Phase = "invalidated"
		view.Block = "invalidated"
		view.NativeKeys = true
		return view, true
	case standingExternal:
		view.Phase = "external"
		view.Block = "external"
		view.NativeKeys = true
		return view, true
	case standingOutstanding:
	default:
		return zero, false
	}
	dlg := a.claudeVisibleDialog(n)
	view.DialogID = dlg.DialogID
	if st.SentKey != "" {
		view.Phase = "sent"
		if st.Delivery != "sent" {
			view.Phase = "unconfirmed"
		}
		view.NativeKeys = false
		view.Block = "already_sent"
		return view, true
	}
	if standingStructuredDecisionPending(a.claudeHookBundle(n), st.ID) {
		view.Phase = "recorded"
		view.NativeKeys = false
		view.Block = "structured_decision"
		return view, true
	}
	if st.Manual && dlg.DialogID != "" && st.ManualEpoch == dlg.DialogID {
		view.Phase = "manual"
		view.NativeKeys = true
		visible := ""
		if a.server != nil {
			visible, _ = a.server.Session(n.ID).CaptureVisible()
		}
		_, choices, block := matchStandingMenu(st.Questions, a.claudeDialogOptions(n, dlg))
		if block == "" && !standingPaneQuestionMatches(st.Questions, visible) {
			block = "menu_mismatch"
		}
		view.Block = block
		if view.Block == "" {
			view.NativeChoices = choices
		}
		return view, true
	}
	view.Phase = "native"
	view.NativeKeys = interactionNativeKeys(view.Phase)
	if dlg.DialogID == "" {
		view.Block = "no_dialog"
		return view, true
	}
	visible := ""
	if a.server != nil {
		visible, _ = a.server.Session(n.ID).CaptureVisible()
	}
	_, choices, block := matchStandingMenu(st.Questions, a.claudeDialogOptions(n, dlg))
	if block == "" && !standingPaneQuestionMatches(st.Questions, visible) {
		block = "menu_mismatch"
	}
	view.Block = block
	if block == "" {
		view.NativeChoices = choices
	}
	return view, true
}

// standingPaneQuestionMatches binds the live numbered menu to the question
// text in the current visible snapshot. Option labels alone are not request
// identity: unrelated questions can offer the same labels.
func standingPaneQuestionMatches(questions []claudeStandingQuestionItem, pane string) bool {
	var matched *claudeStandingQuestionItem
	for _, question := range questions {
		if question.Multi || question.Text == "" || len(question.Options) == 0 {
			continue
		}
		if paneMatchesStandingOptions(question.Options, paneLines(pane)) {
			if matched != nil {
				return false
			}
			matched = &question
		}
	}
	if matched == nil {
		return false
	}
	var prompt []string
	for _, line := range strings.Split(pane, "\n") {
		line = strings.TrimSpace(line)
		if numberedMenuLine(line) {
			break
		}
		if line != "" {
			if matched.Header != "" && (line == "Question: "+matched.Header || line == matched.Header) {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, ">"))
			if line == "" {
				continue
			}
			prompt = append(prompt, line)
		}
	}
	return strings.Join(prompt, "\n") == strings.TrimSpace(matched.Text)
}

func paneLines(pane string) []string {
	var out []string
	for _, line := range strings.Split(pane, "\n") {
		line = strings.TrimSpace(line)
		if numberedMenuLine(line) {
			out = append(out, strings.TrimSpace(line[strings.Index(line, ". ")+2:]))
			continue
		}
		if len(out) > 0 && line == "Other" {
			out = append(out, line)
		}
	}
	return out
}

func numberedMenuLine(line string) bool {
	dot := strings.Index(line, ". ")
	if dot <= 0 || len(line) < dot+3 || line[0] < '1' || line[0] > '9' {
		return false
	}
	for i := 1; i < dot; i++ {
		if line[i] < '0' || line[i] > '9' {
			return false
		}
	}
	return true
}

func (a *app) claudeStandingInteractionView(n *Node, bundle string) (sessionworker.InteractionView, bool) {
	var zero sessionworker.InteractionView
	if n == nil || bundle == "" {
		return zero, false
	}
	standing, _ := standingQuestionsForSession(bundle, n.SessionID)
	if len(standing) > 1 {
		view := sessionworker.InteractionView{
			RequestID: "ambiguous-standing", Phase: "ambiguous", Tool: "AskUserQuestion",
			Block: "multiple_questions", Status: standingOutstanding, NativeKeys: true,
		}
		for _, st := range standing {
			for _, question := range st.Questions {
				item := sessionworker.InteractionQuestion{
					ID: st.ID + ":" + question.ID, Text: question.Text, Header: question.Header,
					Multi: question.Multi, Preview: question.Preview,
				}
				for _, option := range question.Options {
					item.Options = append(item.Options, sessionworker.InteractionOption{ID: option.ID, Label: option.Label, Description: option.Description})
				}
				view.Questions = append(view.Questions, item)
			}
		}
		return view, true
	}
	st, ok := standingVisibleForSession(bundle, n.SessionID)
	if !ok {
		return zero, false
	}
	return a.projectStandingQuestion(n, st)
}

// noteStandingExternal marks the outstanding question whose notice the
// transcript just retired. A Bash notice, a different session, or a digest
// that is not a prefix of this question does not change it. Invalidated
// records stay invalidated. This is not a consumption claim.
func noteStandingExternal(perm string, note claudeAskedNotice) {
	if note.Tool != "AskUserQuestion" || note.Session == "" || note.Digest == "" || perm == "" {
		return
	}
	bundle := filepath.Dir(perm)
	for _, st := range listStandingQuestions(bundle) {
		if st.Status != standingOutstanding || st.Tool != note.Tool || st.SessionID != note.Session {
			continue
		}
		if !strings.HasPrefix(st.Digest, note.Digest) {
			continue
		}
		st.Status = standingExternal
		st.Reason = "transcript"
		_ = writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", st.ID+".json"), st)
		return
	}
}

// invalidateStandingSession closes every still-open question for the session.
// An already invalidated record keeps its reason. External may become
// invalidated. A write failure leaves that record visible rather than
// deleting it.
func invalidateStandingSession(perm, session, reason string) {
	if perm == "" {
		return
	}
	bundle := filepath.Dir(perm)
	for _, st := range listStandingQuestions(bundle) {
		if session != "" && st.SessionID != "" && st.SessionID != session {
			continue
		}
		if st.Status == standingInvalidated {
			continue
		}
		st.Status = standingInvalidated
		st.Reason = reason
		_ = writeClaudePermFile(filepath.Join(bundle, "perm", "interact", "standing", st.ID+".json"), st)
	}
}

// standingHelperCanDeliver is true only while the structured helper can still
// accept stdout. A retired request cannot. Equality with AnswerUntil is closed.
func standingHelperCanDeliver(bundle, id string) bool {
	rec, err := readInteractionRequest(bundle, id)
	if err != nil {
		return false
	}
	return !interactionSubmissionExpired(bundle, id, rec, interactionNow())
}

// standingStructuredDecisionPending prevents native or terminal handoff from
// taking authority away from a structured answer already published for this
// request. A published terminal response is the explicit exception: it ends
// structured authority and permits the user to continue in the native dialog.
func standingStructuredDecisionPending(bundle, id string) bool {
	if ans, err := readInteractionAns(bundle, id); err == nil {
		return !interactionAnsTerminal(ans)
	}
	if ack, ok := readInteractionAck(bundle, id); ok {
		return !ack.Terminal
	}
	if interactionFileExists(bundle, "ans", id+".json") || interactionFileExists(bundle, "ack", id+".json") {
		return true
	}
	return interactionFileExists(bundle, "claim", id) || interactionFileExists(bundle, "recorded", id)
}

// standingNativeCancel routes a cancel into the native dialog only after the
// helper can no longer deliver, or when the standing record is already
// external or invalidated so a structured stdout write cannot run as well.
func standingNativeCancel(bundle string, action sessionworker.InteractionAction) bool {
	if action.Action != "cancel" {
		return false
	}
	st, ok := readStandingQuestion(bundle, action.RequestID)
	if !ok {
		return false
	}
	if st.Status != standingOutstanding {
		return true
	}
	return !standingHelperCanDeliver(bundle, action.RequestID)
}

// standingNativeRelockPause is nil in production. A test uses it to change
// the pane or the node copy after the pre-send audit and before the delivery
// lock is taken again.
var standingNativeRelockPause func(*Node)

// standingNativeAfterSend is nil in production. A test uses it after SendKeys
// has returned, to fail the standing write or the post-send audit.
var standingNativeAfterSend func()

// standingNativeBeforeOwnershipCheck is nil in production. A test uses it to
// model a competing structured claim after the second menu snapshot.
var standingNativeBeforeOwnershipCheck func(string, string)

// standingManualAfterValidation is nil in production. A test uses it to
// replace the menu between validation and key mapping.
var standingManualAfterValidation func(*Node)

// The worker owns one Claude node and may serve concurrent HTTP submissions.
// Serialize native delivery and terminal handoff in-process as well as with
// the on-disk helper fence, so a handoff cannot strand an orphan claim.
var standingDeliveryMu sync.Mutex

type standingNativeShot struct {
	key      string
	keys     []string
	excerpt  string
	question string
	qid      string
	oid      string
	label    string
	st       claudeStandingQuestion
}

func standingConflict(reason string) error {
	return fmt.Errorf("%w: %s", errInteractionConflict, reason)
}

func lockStandingFence(bundle string) (func(), error) {
	unlock, err := lockInteractionFence(bundle)
	if err != nil {
		return nil, fmt.Errorf("%w: fence", errInteractionIO)
	}
	return unlock, nil
}

func standingShotMatches(a, b standingNativeShot) bool {
	if a.key != b.key || a.oid != b.oid || a.label != b.label || len(a.keys) != len(b.keys) {
		return false
	}
	for i := range a.keys {
		if a.keys[i] != b.keys[i] {
			return false
		}
	}
	return true
}

// readStandingNativeShot checks the live fence, the current epoch, and the
// current menu. It does not send a key. dialog_id is an epoch token, not a
// question id. The key number comes from the pane, not from the browser.
func (h *claudeSessionHarness) readStandingNativeShot(n *Node, bundle string, caller claudeInteractionCaller, action sessionworker.InteractionAction) (standingNativeShot, error) {
	var zero standingNativeShot
	st, ok := readStandingQuestion(bundle, action.RequestID)
	if !ok || st.Status != standingOutstanding {
		return zero, standingConflict("stale")
	}
	if standingHelperCanDeliver(bundle, action.RequestID) {
		return zero, standingConflict("structured reply still owns this question")
	}
	rec := claudeInteractionRequest{
		ID: st.ID, SessionID: st.SessionID, BundleID: st.BundleID,
		Generation: st.Generation, NodeID: st.NodeID, Turn: st.Turn, TurnGen: st.TurnGen,
	}
	if err := matchInteractionIdentity(bundle, caller, rec); err != nil {
		return zero, err
	}
	dlg := h.app.claudeVisibleDialog(n)
	if dlg.DialogID == "" {
		return zero, standingConflict("no_dialog")
	}
	if action.DialogID != dlg.DialogID {
		return zero, standingConflict("stale dialog")
	}
	if st.SentKey != "" {
		return zero, standingConflict("already sent; delivery will not be replayed")
	}
	if standingStructuredDecisionPending(bundle, st.ID) {
		return zero, standingConflict("structured decision already owns or answered this question")
	}
	cap, err := h.app.server.Session(n.ID).Capture()
	if err != nil {
		return zero, standingConflict("menu_unavailable")
	}
	opts := h.app.claudeDialogOptions(n, dlg)
	qid, choices, block := matchStandingMenu(st.Questions, opts)
	if block != "" {
		return zero, standingConflict(block)
	}
	visible, err := h.app.server.Session(n.ID).CaptureVisible()
	if err != nil || !standingPaneQuestionMatches(st.Questions, visible) {
		return zero, standingConflict("menu_mismatch")
	}
	shot := standingNativeShot{excerpt: lastLines(cap, 12), st: st}
	if action.Action == "cancel" {
		shot.key = "Escape"
		shot.keys = tmuxKeySequence(n, "Escape")
		return shot, nil
	}
	if len(action.Answers) != 1 {
		return zero, errInteractionBadRequest
	}
	ans := action.Answers[0]
	if strings.TrimSpace(ans.FreeText) != "" {
		return zero, standingConflict("free text cannot be sent through the native menu")
	}
	if len(ans.OptionIDs) != 1 || ans.OptionIDs[0] == "" {
		if len(ans.OptionIDs) > 1 {
			return zero, standingConflict("multiple selections cannot be sent through the native menu")
		}
		return zero, errInteractionBadRequest
	}
	if ans.QuestionID != qid {
		return zero, standingConflict("menu_mismatch")
	}
	var choice sessionworker.InteractionNativeChoice
	found := false
	for _, candidate := range choices {
		if candidate.OptionID == ans.OptionIDs[0] && candidate.QuestionID == qid {
			choice = candidate
			found = true
			break
		}
	}
	if !found || choice.Key == "" || choice.Label == "" {
		return zero, standingConflict("menu_mismatch")
	}
	for _, question := range st.Questions {
		if question.ID == qid {
			shot.question = question.Text
			break
		}
	}
	shot.key = choice.Key
	shot.keys = tmuxKeySequence(n, choice.Key)
	shot.qid = qid
	shot.oid = choice.OptionID
	shot.label = choice.Label
	return shot, nil
}

func standingIntentEvent(st claudeStandingQuestion, shot standingNativeShot, action sessionworker.InteractionAction) sessionlog.Event {
	manual := &sessionlog.ManualInteraction{
		Action:      action.Action,
		Tool:        st.Tool,
		InputDigest: st.Digest,
		AppContext:  "Recorded. The key has not been sent.",
		Delivery:    "recorded",
		Turn:        st.Turn,
		TurnGen:     st.TurnGen,
		SessionID:   st.SessionID,
		BundleID:    st.BundleID,
		Generation:  st.Generation,
	}
	name := "Native key selected"
	if action.Action == "cancel" {
		name = "Cancel selected"
	} else {
		manual.Selections = []sessionlog.ManualSelection{{
			QuestionID: shot.qid,
			Question:   shot.question,
			OptionIDs:  []string{shot.oid},
			Labels:     []string{shot.label},
			Kind:       "option",
		}}
	}
	return sessionlog.NewDecision(sessionlog.DecisionEvent{
		Source:    "manual",
		RequestID: st.ID,
		Agent:     "claude",
		Title:     st.Tool,
		Selected:  sessionlog.DecOption{Name: name, Kind: action.Action},
		Manual:    manual,
	})
}

func standingHandoffEvent(st claudeStandingQuestion) sessionlog.Event {
	manual := &sessionlog.ManualInteraction{
		Action: "terminal", Tool: st.Tool, InputDigest: st.Digest,
		AppContext: "Handed off to the current native dialog controls. No key was sent.",
		Delivery:   "recorded", Turn: st.Turn, TurnGen: st.TurnGen,
		SessionID: st.SessionID, BundleID: st.BundleID, Generation: st.Generation,
	}
	return sessionlog.NewDecision(sessionlog.DecisionEvent{
		Source: "manual", RequestID: st.ID, Agent: "claude", Title: st.Tool,
		Selected: sessionlog.DecOption{Name: "Use native dialog controls", Kind: "terminal"}, Manual: manual,
	})
}

func markStandingManual(bundle, id, epoch string) error {
	path, err := interactionChild(bundle, "standing", id+".json")
	if err != nil {
		return err
	}
	unlock, err := lockStandingFence(bundle)
	if err != nil {
		return err
	}
	defer unlock()
	st, ok := readStandingQuestion(bundle, id)
	if !ok || st.Status != standingOutstanding {
		return standingConflict("stale")
	}
	if epoch == "" || st.SentKey != "" || standingStructuredDecisionPending(bundle, id) {
		return standingConflict("native controls cannot be handed off")
	}
	st.Manual = true
	st.ManualEpoch = epoch
	if err := writeClaudePermFile(path, st); err != nil {
		return fmt.Errorf("%w: handoff state", errInteractionIO)
	}
	return nil
}

func (h *claudeSessionHarness) handoffStandingQuestion(n *Node, bundle string, caller claudeInteractionCaller, action sessionworker.InteractionAction, audit func(sessionlog.Event) error) (sessionworker.InteractionResult, error) {
	var zero sessionworker.InteractionResult
	standingDeliveryMu.Lock()
	defer standingDeliveryMu.Unlock()
	unlock, err := lockStandingFence(bundle)
	if err != nil {
		return zero, err
	}
	st, ok := readStandingQuestion(bundle, action.RequestID)
	if !ok || st.Status != standingOutstanding {
		unlock()
		return zero, standingConflict("stale")
	}
	if st.SentKey != "" {
		unlock()
		phase := "sent"
		if st.Delivery != "sent" {
			phase = "unconfirmed"
		}
		return sessionworker.InteractionResult{Status: "accepted", Delivery: st.Delivery, RequestID: st.ID, Emitted: false, Phase: phase}, nil
	}
	if st.Manual && st.ManualEpoch != "" {
		dlg := h.app.claudeVisibleDialog(n)
		if dlg.DialogID == st.ManualEpoch {
			unlock()
			return sessionworker.InteractionResult{Status: "accepted", Delivery: "recorded", RequestID: st.ID, Emitted: false, Phase: "manual"}, nil
		}
	}
	if standingStructuredDecisionPending(bundle, st.ID) {
		unlock()
		return zero, standingConflict("structured decision already owns or answered this question")
	}
	if standingHelperCanDeliver(bundle, st.ID) {
		unlock()
		return zero, standingConflict("structured reply still owns this question")
	}
	rec := claudeInteractionRequest{ID: st.ID, SessionID: st.SessionID, BundleID: st.BundleID, Generation: st.Generation, NodeID: st.NodeID, Turn: st.Turn, TurnGen: st.TurnGen}
	if err := matchInteractionIdentity(bundle, caller, rec); err != nil {
		unlock()
		return zero, err
	}
	dlg := h.app.claudeVisibleDialog(n)
	if dlg.DialogID == "" {
		unlock()
		return zero, standingConflict("no_dialog")
	}
	if err := h.validateStandingDialog(n, st, dlg); err != nil {
		unlock()
		return zero, err
	}
	unlock()
	if err := audit(standingHandoffEvent(st)); err != nil {
		return zero, fmt.Errorf("%w: %v", errInteractionAudit, err)
	}
	unlock, err = lockStandingFence(bundle)
	if err != nil {
		return zero, err
	}
	defer unlock()
	current, ok := readStandingQuestion(bundle, st.ID)
	if !ok || current.Status != standingOutstanding || current.Digest != st.Digest || current.SentKey != "" || standingStructuredDecisionPending(bundle, st.ID) {
		return zero, standingConflict("question changed during handoff")
	}
	currentDialog := h.app.claudeVisibleDialog(n)
	if currentDialog.DialogID != dlg.DialogID {
		return zero, standingConflict("stale dialog")
	}
	if err := h.validateStandingDialog(n, current, currentDialog); err != nil {
		return zero, err
	}
	current.Manual = true
	current.ManualEpoch = dlg.DialogID
	path, err := interactionChild(bundle, "standing", st.ID+".json")
	if err == nil {
		err = writeClaudePermFile(path, current)
	}
	if err != nil {
		return zero, fmt.Errorf("%w: handoff state", errInteractionIO)
	}
	return sessionworker.InteractionResult{Status: "accepted", Delivery: "recorded", RequestID: current.ID, Emitted: false, Phase: "manual"}, nil
}

func (h *claudeSessionHarness) validateStandingDialog(n *Node, st claudeStandingQuestion, dlg claudeVisibleDialog) error {
	if dlg.DialogID == "" {
		return standingConflict("no_dialog")
	}
	_, _, block := matchStandingMenu(st.Questions, h.app.claudeDialogOptions(n, dlg))
	if block != "" {
		return standingConflict(block)
	}
	visible, err := h.app.server.Session(n.ID).CaptureVisible()
	if err != nil || !standingPaneQuestionMatches(st.Questions, visible) {
		return standingConflict("menu_mismatch")
	}
	return nil
}

func standingOutcomeEvent(requestID, delivery, name, context string) sessionlog.Event {
	return sessionlog.NewDecision(sessionlog.DecisionEvent{
		Source:    "application",
		RequestID: requestID,
		Agent:     "claude",
		Selected:  sessionlog.DecOption{Name: name, Kind: delivery},
		Outcome: &sessionlog.DeliveryOutcome{
			Delivery:   delivery,
			AppContext: context,
		},
	})
}

// deliverStandingNative sends one whitelisted key into the open dialog after
// the helper can no longer deliver. The first lock only reads. Audit is
// outside the fence. The second lock re-checks, then SendKeys. Recording the
// key, retiring the epoch, and a later transcript are not consumption.
func (h *claudeSessionHarness) deliverStandingNative(n *Node, bundle string, caller claudeInteractionCaller, action sessionworker.InteractionAction, audit func(sessionlog.Event) error) (result sessionworker.InteractionResult, resultErr error) {
	var zero sessionworker.InteractionResult
	standingDeliveryMu.Lock()
	defer standingDeliveryMu.Unlock()
	if action.DialogID == "" {
		return zero, errInteractionBadRequest
	}
	unlock, err := lockStandingFence(bundle)
	if err != nil {
		return zero, err
	}
	shot, err := h.readStandingNativeShot(n, bundle, caller, action)
	unlock()
	if err != nil {
		return zero, err
	}
	if err := h.app.appendRecord(storeRecord{
		Type: "key", ID: n.ID, Key: shot.key, Keys: append([]string(nil), shot.keys...),
		Excerpt: shot.excerpt, Time: interactionNow().UTC().Format(time.RFC3339),
	}); err != nil {
		return zero, fmt.Errorf("%w: %v", errInteractionAudit, err)
	}
	if err := audit(standingIntentEvent(shot.st, shot, action)); err != nil {
		return zero, fmt.Errorf("%w: %v", errInteractionAudit, err)
	}
	committed := false
	defer func() {
		if committed {
			err := audit(standingOutcomeEvent(shot.st.ID, "sent", "Sent to the open dialog", "Sent to the open dialog. Claude has not confirmed it."))
			if err != nil {
				resultErr = fmt.Errorf("%w: key was sent, but persisting the audit record failed: %v", errInteractionAudit, err)
			}
			return
		}
		delivery, name := "not_delivered", "Not delivered"
		context := "Application delivery outcome: this submission was rejected before delivery."
		if !errors.Is(resultErr, errInteractionConflict) {
			delivery, name = "unconfirmed", "Delivery unconfirmed"
			context = "Application delivery outcome: submission failed; delivery is unconfirmed."
		}
		if err := audit(standingOutcomeEvent(shot.st.ID, delivery, name, context)); err != nil {
			resultErr = fmt.Errorf("%w: outcome for %v: %v", errInteractionAudit, resultErr, err)
		}
	}()
	if pause := standingNativeRelockPause; pause != nil {
		pause(n)
	}
	unlock, err = lockStandingFence(bundle)
	if err != nil {
		return zero, err
	}
	defer unlock()
	again, err := h.readStandingNativeShot(n, bundle, caller, action)
	if err != nil {
		return zero, err
	}
	if pause := standingNativeBeforeOwnershipCheck; pause != nil {
		pause(bundle, again.st.ID)
	}
	if !standingShotMatches(shot, again) {
		return zero, standingConflict("menu_mismatch")
	}
	if interactionFileExists(bundle, "claim", again.st.ID) || interactionFileExists(bundle, "recorded", again.st.ID) || interactionFileExists(bundle, "ans", again.st.ID+".json") || interactionFileExists(bundle, "ack", again.st.ID+".json") {
		return zero, standingConflict("structured reply already owns or answered this question")
	}
	// Persist a no-replay fence before touching tmux. A crash or ambiguous
	// SendKeys result must not make a second answer authorized.
	st := again.st
	st.SentEpoch = action.DialogID
	st.SentKey = again.key
	st.Delivery = "unconfirmed"
	path, pathErr := interactionChild(bundle, "standing", st.ID+".json")
	if pathErr == nil {
		pathErr = writeClaudePermFile(path, st)
	}
	if pathErr != nil {
		h.app.retireClaudeVisibleDialog(n, action.DialogID)
		return zero, fmt.Errorf("%w: could not persist no-replay fence", errInteractionIO)
	}
	if err := markInteractionExpired(bundle, st.ID); err != nil {
		h.app.retireClaudeVisibleDialog(n, action.DialogID)
		return zero, fmt.Errorf("%w: could not fence structured helper", errInteractionIO)
	}
	if err := h.app.server.Session(n.ID).SendKeys(again.keys...); err != nil {
		return zero, fmt.Errorf("%w: %v", errInteractionIO, err)
	}
	if pause := standingNativeAfterSend; pause != nil {
		pause()
	}
	st.Delivery = "sent"
	path, pathErr = interactionChild(bundle, "standing", st.ID+".json")
	if pathErr == nil {
		pathErr = writeClaudePermFile(path, st)
	}
	if pathErr != nil {
		h.app.retireClaudeVisibleDialog(n, action.DialogID)
		return zero, fmt.Errorf("%w: key was sent, but persisting the standing record failed", errInteractionIO)
	}
	h.app.retireClaudeVisibleDialog(n, action.DialogID)
	committed = true
	return sessionworker.InteractionResult{
		Status: "accepted", Delivery: "sent", RequestID: st.ID, Emitted: false, Phase: "sent",
	}, nil
}
