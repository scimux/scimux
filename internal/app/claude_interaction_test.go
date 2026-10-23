package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClaudeInteractionLimitsPinned(t *testing.T) {
	if interactionFeedbackMax != 4096 || interactionFreeTextMax != 4096 {
		t.Fatalf("reply limits feedback=%d free=%d", interactionFeedbackMax, interactionFreeTextMax)
	}
	if interactionPostMax != 32*1024 || interactionRenderedMax != 48*1024 || interactionPlanTextMax != 32*1024 {
		t.Fatalf("payload limits post=%d rendered=%d plan=%d", interactionPostMax, interactionRenderedMax, interactionPlanTextMax)
	}
	if interactionQuestionTextMax != 2048 || interactionOptionLabelMax != 512 || interactionOptionDescMax != 2048 {
		t.Fatalf("question limits text=%d label=%d desc=%d", interactionQuestionTextMax, interactionOptionLabelMax, interactionOptionDescMax)
	}
	if interactionHeaderMax != 80 || interactionPreviewMax != 2048 || interactionHookTimeoutSec != 180 {
		t.Fatalf("header=%d preview=%d timeout=%d", interactionHeaderMax, interactionPreviewMax, interactionHookTimeoutSec)
	}
	if interactionHelperDeadline != 150*time.Second || interactionRejectMargin != 15*time.Second || interactionAnswerWindow != 135*time.Second {
		t.Fatalf("deadlines helper=%s margin=%s window=%s", interactionHelperDeadline, interactionRejectMargin, interactionAnswerWindow)
	}
	if interactionHelperDeadline >= time.Duration(interactionHookTimeoutSec)*time.Second {
		t.Fatal("helper deadline must be shorter than the registered hook timeout")
	}
	if interactionAnswerWindow >= interactionHelperDeadline {
		t.Fatal("acceptance window must close before the helper deadline")
	}
	if permHookDeadline != 5*time.Second || interactionHelperDeadline == permHookDeadline {
		t.Fatalf("ordinary permission deadline changed: %s", permHookDeadline)
	}
	if interactionMultiJoin != ", " {
		t.Fatalf("multi join %q", interactionMultiJoin)
	}
}

func TestClaudeInteractionGateIsExactOne(t *testing.T) {
	if !claudeInteractionsEnvEnabled("1") {
		t.Fatal("exact 1 enables a newly minted bundle")
	}
	for _, v := range []string{"", "0", "true", "yes", " 1", "1 ", "01", "2"} {
		if claudeInteractionsEnvEnabled(v) {
			t.Fatalf("%q enabled the gate", v)
		}
	}
}

func letterQuestionRaw() []byte {
	return []byte(`{"z_note":"","questions":[{"question":"Pick a letter?","header":"Letter","options":[{"label":"Alpha","description":"first","extra":null},{"label":"Beta","description":"second"}],"multiSelect":false,"preview":"two glyphs"}]}`)
}

func colorQuestionRaw() []byte {
	return []byte(`{"questions":[{"question":"Pick colors?","header":"Colors","preview":"swatches","options":[{"label":"Red","description":"r"},{"label":"Green","description":"g"},{"label":"Blue","description":"b"}],"multiSelect":true}]}`)
}

func twoQuestionRaw() []byte {
	return []byte(`{"questions":[{"question":"Pick a letter?","header":"Letter","options":[{"label":"Alpha","description":"first"},{"label":"Beta","description":"second"}],"multiSelect":false},{"question":"Pick a digit?","header":"Digit","options":[{"label":"One","description":"1"},{"label":"Two","description":"2"}],"multiSelect":false}]}`)
}

func tinyPlanRaw() []byte {
	return []byte(`{"plan":"## Synthetic\nOnly a plan.","planFilePath":"/plans/not-read.md","allowedPrompts":[{"tool":"Bash","prompt":"do not run"}],"note":""}`)
}

func mustClassify(t *testing.T, tool string, raw []byte) claudeInteractionModel {
	t.Helper()
	model, err := classifyClaudeInteraction(tool, raw)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if model.Native {
		t.Fatalf("native fallback: %s", model.NativeReason)
	}
	return model
}

func TestClaudeInteractionClassifySupported(t *testing.T) {
	letter := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	if letter.Tool != "AskUserQuestion" || len(letter.Questions) != 1 {
		t.Fatalf("letter model %+v", letter)
	}
	q := letter.Questions[0]
	if q.ID != "q1" || q.Text != "Pick a letter?" || q.Header != "Letter" || q.Multi || q.Preview != "two glyphs" {
		t.Fatalf("question %+v", q)
	}
	if len(q.Options) != 2 || q.Options[0].ID != "q1o1" || q.Options[0].Label != "Alpha" || q.Options[0].Description != "first" {
		t.Fatalf("options %+v", q.Options)
	}
	if q.Options[1].ID != "q1o2" || q.Options[1].Label != "Beta" {
		t.Fatalf("second option %+v", q.Options[1])
	}
	sum := sha256.Sum256(letterQuestionRaw())
	if letter.Digest != hex.EncodeToString(sum[:]) || !bytes.Equal(letter.Original, letterQuestionRaw()) {
		t.Fatal("digest or original bytes were not preserved")
	}

	colors := mustClassify(t, "AskUserQuestion", colorQuestionRaw())
	if !colors.Questions[0].Multi || len(colors.Questions[0].Options) != 3 || colors.Questions[0].Options[2].ID != "q1o3" {
		t.Fatalf("colors %+v", colors.Questions)
	}

	batch := mustClassify(t, "AskUserQuestion", twoQuestionRaw())
	if len(batch.Questions) != 2 || batch.Questions[1].ID != "q2" || batch.Questions[1].Options[0].ID != "q2o1" {
		t.Fatalf("batch %+v", batch.Questions)
	}

	plan := mustClassify(t, "ExitPlanMode", tinyPlanRaw())
	if plan.Plan != "## Synthetic\nOnly a plan." || plan.PlanFilePath != "/plans/not-read.md" || len(plan.Questions) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	if !bytes.Equal(plan.Original, tinyPlanRaw()) {
		t.Fatal("plan original was rewritten")
	}

	escaped := []byte(`{"questions":[{"question":"say \"hi\"\n","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`)
	got := mustClassify(t, "AskUserQuestion", escaped)
	if got.Questions[0].Text != "say \"hi\"\n" {
		t.Fatalf("decoded question %q", got.Questions[0].Text)
	}

	benign := []byte(`{"flag":false,"list":[],"obj":{},"empty":null,"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":"","flag":false},{"label":"B","description":""}],"multiSelect":false}]}`)
	if model := mustClassify(t, "AskUserQuestion", benign); model.Questions[0].Text != "Pick?" {
		t.Fatalf("benign unknown members rejected: %+v", model)
	}
}

func TestClaudeInteractionClassifyNativeFallback(t *testing.T) {
	letter := letterQuestionRaw()
	longText := `{"questions":[{"question":"` + strings.Repeat("q", interactionQuestionTextMax+1) + `","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`
	longLabel := `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"` + strings.Repeat("L", interactionOptionLabelMax+1) + `","description":""},{"label":"B","description":""}]}]}`
	longHeader := `{"questions":[{"question":"Pick?","header":"` + strings.Repeat("H", interactionHeaderMax+1) + `","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`
	longPreview := `{"questions":[{"question":"Pick?","header":"H","preview":"` + strings.Repeat("p", interactionPreviewMax+1) + `","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`
	longDesc := `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":"` + strings.Repeat("d", interactionOptionDescMax+1) + `"},{"label":"B","description":""}]}]}`
	exactText := `{"questions":[{"question":"` + strings.Repeat("q", interactionQuestionTextMax) + `","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`
	exact := mustClassify(t, "AskUserQuestion", []byte(exactText))
	if len(exact.Questions) != 1 || len(exact.Questions[0].Text) != interactionQuestionTextMax {
		t.Fatalf("exact question length model=%+v", exact.Questions)
	}
	cases := []struct {
		tool string
		raw  string
		want string
	}{
		{"Bash", string(letter), "unsupported tool"},
		{"", string(letter), "unsupported tool"},
		{"askuserquestion", string(letter), "unsupported tool"},
		{"AskUserQuestion", "{", "malformed json"},
		{"AskUserQuestion", "[]", "not object"},
		{"AskUserQuestion", "null", "not object"},
		{"AskUserQuestion", `{}`, "question count"},
		{"AskUserQuestion", `{"questions":[]}`, "question count"},
		{"AskUserQuestion", `{"questions":[{"header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "empty question"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H"}]}`, "option count"},
		{"AskUserQuestion", `{"questions":[{"question":"\uD800","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "question shape"},
		{"AskUserQuestion", `{"questions":[1,]}`, "malformed json"},
		{"AskUserQuestion", `{"questions":[1 2]}`, "malformed json"},
		{"AskUserQuestion", `{"questions":[{},{},{},{},{}]}`, "question count"},
		{"AskUserQuestion", `{"questions":"nope"}`, "question shape"},
		{"AskUserQuestion", `{"questions":[1]}`, "question shape"},
		{"AskUserQuestion", `{"questions":[{"question":"Same?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]},{"question":"Same?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "duplicate question"},
		{"AskUserQuestion", `{"questions":[{"question":"","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "empty question"},
		{"AskUserQuestion", `{"questions":[{"question":1,"header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "question type"},
		{"AskUserQuestion", longText, "question length"},
		{"AskUserQuestion", longHeader, "header length"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":1,"options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "header type"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""}]}]}`, "option count"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""},{"label":"C","description":""},{"label":"D","description":""},{"label":"E","description":""}]}]}`, "option count"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":{"label":"A"}}]}`, "option shape"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[1,2]}]}`, "option shape"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"","description":""},{"label":"B","description":""}]}]}`, "empty label"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"A","description":""}]}]}`, "duplicate label"},
		{"AskUserQuestion", longLabel, "label length"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":1,"description":""},{"label":"B","description":""}]}]}`, "label type"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A, B","description":""},{"label":"C","description":""}]}]}`, "ambiguous labels"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""},{"label":"A, B","description":""}]}]}`, "ambiguous labels"},
		{"AskUserQuestion", longDesc, "description length"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":1},{"label":"B","description":""}]}]}`, "description type"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}],"multiSelect":"yes"}]}`, "multiSelect type"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}],"multiSelect":null}]}`, "multiSelect type"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","preview":{"html":"<b>"},"options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "preview type"},
		{"AskUserQuestion", longPreview, "preview length"},
		{"AskUserQuestion", `{"note":"hello","questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "unknown member"},
		{"AskUserQuestion", `{"flag":true,"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "unknown member"},
		{"AskUserQuestion", `{"n":0,"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "unknown member"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","kind":"preview","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "unknown member"},
		{"AskUserQuestion", `{"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":"","href":"http://example.test"},{"label":"B","description":""}]}]}`, "unknown member"},
		{"AskUserQuestion", `{"answers":{"Pick?":"Alpha"},"questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "answers present"},
		{"AskUserQuestion", `{"plan":"not a question","questions":[{"question":"Pick?","header":"H","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`, "unknown member"},
		{"ExitPlanMode", `{}`, "plan missing"},
		{"ExitPlanMode", `{"plan":""}`, "plan missing"},
		{"ExitPlanMode", `{"plan":1}`, "plan type"},
		{"ExitPlanMode", `{"plan":"## Synthetic","planFilePath":1}`, "plan path type"},
		{"ExitPlanMode", `{"plan":"## Synthetic","allowedPrompts":{"tool":"Bash"}}`, "allowedPrompts type"},
		{"ExitPlanMode", `{"plan":"` + strings.Repeat("p", interactionPlanTextMax+1) + `"}`, "plan length"},
		{"ExitPlanMode", `{"plan":"## Synthetic","note":"meaningful"}`, "unknown member"},
		{"Write", `{"plan":"## Synthetic"}`, "unsupported tool"},
		{"AskUserQuestion", `{"a":1,"a":2}`, "malformed json"},
	}
	for _, tc := range cases {
		model, err := classifyClaudeInteraction(tc.tool, []byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		if !model.Native || model.NativeReason != tc.want {
			t.Fatalf("%s: native=%v reason=%q", tc.want, model.Native, model.NativeReason)
		}
		if len(model.Questions) != 0 || model.Plan != "" {
			t.Fatalf("%s: native model still exposed content", tc.want)
		}
	}
	model, err := classifyClaudeInteraction("AskUserQuestion", []byte{0xff, 0xfe})
	if err != nil || !model.Native || model.NativeReason != "invalid utf-8" {
		t.Fatalf("utf-8: err=%v model=%+v", err, model)
	}
	if utf8.Valid([]byte{0xff}) {
		t.Fatal("fixture was valid utf-8")
	}
	exactPlan := `{"plan":"` + strings.Repeat("p", interactionPlanTextMax) + `"}`
	plan := mustClassify(t, "ExitPlanMode", []byte(exactPlan))
	if len(plan.Plan) != interactionPlanTextMax {
		t.Fatalf("exact plan length %d", len(plan.Plan))
	}
	// allowedPrompts is retained for the echo and is not an authorization source.
	withPrompts := mustClassify(t, "ExitPlanMode", tinyPlanRaw())
	if !bytes.Contains(withPrompts.Original, []byte(`"allowedPrompts"`)) || withPrompts.Plan == "" {
		t.Fatal("allowedPrompts was dropped from the original plan input")
	}
}

func TestClaudeInteractionRenderedBounds(t *testing.T) {
	exact := renderedFixture(t, interactionOptionDescMax-1)
	over := renderedFixture(t, interactionOptionDescMax)
	if got := renderedSize(exact); got != interactionRenderedMax {
		t.Fatalf("fixture sum %d want %d", got, interactionRenderedMax)
	}
	if got := renderedSize(over); got != interactionRenderedMax+16 {
		t.Fatalf("over fixture sum %d", got)
	}
	model := mustClassify(t, "AskUserQuestion", exact)
	if len(model.Questions) != 4 || len(model.Questions[3].Options) != 4 {
		t.Fatalf("exact rendered model %+v", len(model.Questions))
	}
	native, err := classifyClaudeInteraction("AskUserQuestion", over)
	if err != nil || !native.Native || native.NativeReason != "rendered length" {
		t.Fatalf("over rendered: err=%v reason=%q native=%v", err, native.NativeReason, native.Native)
	}
}

func renderedFixture(t *testing.T, descLen int) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"questions":[`)
	for i := 0; i < 4; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		text := "Q" + string(rune('0'+i))
		text += strings.Repeat("x", interactionQuestionTextMax-len(text))
		b.WriteString(`{"question":"`)
		b.WriteString(text)
		b.WriteString(`","header":"","preview":"`)
		b.WriteString(strings.Repeat("p", interactionPreviewMax))
		b.WriteString(`","options":[`)
		for j, label := range []string{"A", "B", "C", "D"} {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"label":"`)
			b.WriteString(label)
			b.WriteString(`","description":"`)
			b.WriteString(strings.Repeat("d", descLen))
			b.WriteString(`"}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func renderedSize(raw []byte) int {
	var top struct {
		Questions []struct {
			Question string
			Header   string
			Preview  string
			Options  []struct {
				Label       string
				Description string
			}
		}
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return -1
	}
	n := 0
	for _, q := range top.Questions {
		n += len(q.Question) + len(q.Header) + len(q.Preview)
		for _, opt := range q.Options {
			n += len(opt.Label) + len(opt.Description)
		}
	}
	return n
}

type replyEnvelope struct {
	HookSpecificOutput struct {
		HookEventName string `json:"hookEventName"`
		Decision      struct {
			Behavior           string          `json:"behavior"`
			UpdatedInput       json.RawMessage `json:"updatedInput"`
			UpdatedPermissions json.RawMessage `json:"updatedPermissions"`
			Message            string          `json:"message"`
			Interrupt          *bool           `json:"interrupt"`
		} `json:"decision"`
	} `json:"hookSpecificOutput"`
}

func decodeReply(t *testing.T, stdout []byte) replyEnvelope {
	t.Helper()
	var env replyEnvelope
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode reply: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("trailing reply bytes: %s", stdout)
	}
	return env
}

func forbiddenReplyKeys(t *testing.T, stdout []byte) {
	t.Helper()
	var walk func(any)
	seen := map[string]bool{}
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				seen[k] = true
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(stdout, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc)
	for _, key := range []string{"addRules", "replaceRules", "removeRules", "addDirectories", "removeDirectories", "localSettings", "userSettings", "projectSettings", "bypassPermissions", "continue", "permissionDecision"} {
		if seen[key] {
			t.Fatalf("forbidden key %s in %s", key, stdout)
		}
	}
}

func TestClaudeInteractionReplyValidation(t *testing.T) {
	handBuilt := claudeInteractionModel{
		Tool: "AskUserQuestion",
		Questions: []claudeInteractionQuestion{{
			ID: "q1", Text: "Pick a letter?", Options: []claudeInteractionOption{{ID: "q1o1", Label: "Alpha"}, {ID: "q1o2", Label: "Beta"}},
		}},
	}
	if env, err := buildClaudeInteractionReply(handBuilt, claudeInteractionReply{Action: "cancel"}); err != nil || !bytes.Contains(env.Stdout, []byte(`"interrupt":true`)) {
		t.Fatalf("cancel: err=%v stdout=%s", err, env.Stdout)
	}
	letter := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	colors := mustClassify(t, "AskUserQuestion", colorQuestionRaw())
	batch := mustClassify(t, "AskUserQuestion", twoQuestionRaw())
	plan := mustClassify(t, "ExitPlanMode", tinyPlanRaw())
	native, err := classifyClaudeInteraction("Bash", letterQuestionRaw())
	if err != nil || !native.Native {
		t.Fatal("expected native bash")
	}
	bad := []struct {
		name  string
		model claudeInteractionModel
		reply claudeInteractionReply
	}{
		{"native", native, claudeInteractionReply{Action: "answer"}},
		{"answer on plan", plan, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}},
		{"approve on question", letter, claudeInteractionReply{Action: "approve", Mode: "restore"}},
		{"discuss on plan", plan, claudeInteractionReply{Action: "discuss", Feedback: "wait"}},
		{"changes on question", letter, claudeInteractionReply{Action: "request_changes", Feedback: "wait"}},
		{"missing action", letter, claudeInteractionReply{}},
		{"answer with feedback", letter, claudeInteractionReply{Action: "answer", Feedback: "aside"}},
		{"empty question id", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "", OptionIDs: []string{"q1o1"}}}}},
		{"missing question", batch, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}},
		{"duplicate question id", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}, {QuestionID: "q1", FreeText: "no"}}}},
		{"unknown question", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q9", OptionIDs: []string{"q1o1"}}}}},
		{"unknown option", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o9"}}}}},
		{"duplicate option", colors, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1", "q1o1"}}}}},
		{"two options on single", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1", "q1o2"}}}}},
		{"empty single", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1"}}}},
		{"option and free text", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}, FreeText: "else"}}}},
		{"empty multi", colors, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1"}}}},
		{"free text on multi", colors, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: "purple"}}}},
		{"blank free text", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: "  \n"}}}},
		{"long free text", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: strings.Repeat("f", interactionFreeTextMax+1)}}}},
		{"invalid free text", letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: string([]byte{0xff})}}}},
		{"blank discuss", letter, claudeInteractionReply{Action: "discuss", Feedback: " \t"}},
		{"long feedback", letter, claudeInteractionReply{Action: "discuss", Feedback: strings.Repeat("d", interactionFeedbackMax+1)}},
		{"discuss with mode", letter, claudeInteractionReply{Action: "discuss", Feedback: "talk", Mode: "acceptEdits"}},
		{"discuss with answer", letter, claudeInteractionReply{Action: "discuss", Feedback: "talk", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}},
		{"blank changes", plan, claudeInteractionReply{Action: "request_changes", Feedback: ""}},
		{"changes with mode", plan, claudeInteractionReply{Action: "request_changes", Feedback: "revise", Mode: "default"}},
		{"cancel with feedback", letter, claudeInteractionReply{Action: "cancel", Feedback: "stop"}},
		{"cancel with mode", plan, claudeInteractionReply{Action: "cancel", Mode: "acceptEdits"}},
		{"approve without mode", plan, claudeInteractionReply{Action: "approve"}},
		{"approve bad mode", plan, claudeInteractionReply{Action: "approve", Mode: "bypassPermissions"}},
		{"approve auto", plan, claudeInteractionReply{Action: "approve", Mode: "auto"}},
		{"approve plan mode", plan, claudeInteractionReply{Action: "approve", Mode: "plan"}},
		{"approve manual alias", plan, claudeInteractionReply{Action: "approve", Mode: "manual"}},
		{"approve with feedback", plan, claudeInteractionReply{Action: "approve", Mode: "restore", Feedback: "ok"}},
		{"terminal with feedback", letter, claudeInteractionReply{Action: "terminal", Feedback: "later"}},
		{"terminal with mode", plan, claudeInteractionReply{Action: "terminal", Mode: "default"}},
		{"invalid question text", claudeInteractionModel{
			Tool: "AskUserQuestion", Original: []byte(`{}`),
			Questions: []claudeInteractionQuestion{{ID: "q1", Text: string([]byte{0xff}), Options: []claudeInteractionOption{{ID: "q1o1", Label: "Alpha"}, {ID: "q1o2", Label: "Beta"}}}},
		}, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}},
		{"unspliceable original", claudeInteractionModel{
			Tool: "AskUserQuestion", Original: []byte(`[]`),
			Questions: []claudeInteractionQuestion{{ID: "q1", Text: "Pick?", Options: []claudeInteractionOption{{ID: "q1o1", Label: "Alpha"}, {ID: "q1o2", Label: "Beta"}}}},
		}, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}},
	}
	for _, tc := range bad {
		env, err := buildClaudeInteractionReply(tc.model, tc.reply)
		if err == nil || len(env.Stdout) != 0 || env.Terminal {
			t.Fatalf("%s: err=%v stdout=%s terminal=%v", tc.name, err, env.Stdout, env.Terminal)
		}
	}
	// Boundary lengths that must still be accepted.
	longFree := mustReply(t, letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: strings.Repeat("f", interactionFreeTextMax)}}})
	if !bytes.Contains(longFree.Stdout, []byte(strings.Repeat("f", interactionFreeTextMax))) {
		t.Fatal("exact free-text limit was rejected or truncated")
	}
	longTalk := mustReply(t, letter, claudeInteractionReply{Action: "discuss", Feedback: strings.Repeat("d", interactionFeedbackMax)})
	if !bytes.Contains(longTalk.Stdout, []byte(strings.Repeat("d", interactionFeedbackMax))) {
		t.Fatal("exact feedback limit was rejected or truncated")
	}
}

func mustReply(t *testing.T, model claudeInteractionModel, reply claudeInteractionReply) claudeInteractionEnvelope {
	t.Helper()
	env, err := buildClaudeInteractionReply(model, reply)
	if err != nil {
		t.Fatalf("reply %s: %v", reply.Action, err)
	}
	return env
}

func TestClaudeInteractionEnvelopes(t *testing.T) {
	letter := mustClassify(t, "AskUserQuestion", letterQuestionRaw())
	colors := mustClassify(t, "AskUserQuestion", colorQuestionRaw())
	batch := mustClassify(t, "AskUserQuestion", twoQuestionRaw())
	plan := mustClassify(t, "ExitPlanMode", tinyPlanRaw())

	single := mustReply(t, letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o2"}}}})
	env := decodeReply(t, single.Stdout)
	if env.HookSpecificOutput.HookEventName != "PermissionRequest" || env.HookSpecificOutput.Decision.Behavior != "allow" {
		t.Fatalf("single envelope %+v", env.HookSpecificOutput)
	}
	if env.HookSpecificOutput.Decision.Interrupt != nil || len(env.HookSpecificOutput.Decision.UpdatedPermissions) != 0 {
		t.Fatal("single answer carried interrupt or permissions")
	}
	if !bytes.Contains(env.HookSpecificOutput.Decision.UpdatedInput, []byte(`"z_note":""`)) || !bytes.HasPrefix(bytes.TrimSpace(env.HookSpecificOutput.Decision.UpdatedInput), []byte(`{"z_note":""`)) {
		t.Fatalf("original prefix lost: %s", env.HookSpecificOutput.Decision.UpdatedInput)
	}
	assertAnswers(t, env.HookSpecificOutput.Decision.UpdatedInput, map[string]string{"Pick a letter?": "Beta"})
	if len(single.Audit.Selections) != 1 || single.Audit.Selections[0].Kind != "option" || single.Audit.Selections[0].FreeText != "" || single.Audit.Selections[0].Labels[0] != "Beta" {
		t.Fatalf("single audit %+v", single.Audit)
	}
	forbiddenReplyKeys(t, single.Stdout)
	if bytes.Equal(single.Stdout, []byte(claudePermAllowJSON)) {
		t.Fatal("question answer used the bare auto-approve envelope")
	}

	// Click order is not wire order. Free text that equals a label stays free text.
	multi := mustReply(t, colors, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o3", "q1o1"}}}})
	menv := decodeReply(t, multi.Stdout)
	assertAnswers(t, menv.HookSpecificOutput.Decision.UpdatedInput, map[string]string{"Pick colors?": "Red, Blue"})
	if multi.Audit.Selections[0].Kind != "multi" || strings.Join(multi.Audit.Selections[0].Labels, "|") != "Red|Blue" {
		t.Fatalf("multi audit %+v", multi.Audit.Selections)
	}
	one := mustReply(t, colors, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o2"}}}})
	oenv := decodeReply(t, one.Stdout)
	assertAnswers(t, oenv.HookSpecificOutput.Decision.UpdatedInput, map[string]string{"Pick colors?": "Green"})

	free := mustReply(t, letter, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", FreeText: "Alpha <b>&"}}})
	if free.Audit.Selections[0].Kind != "free_text" || free.Audit.Selections[0].FreeText != "Alpha <b>&" || len(free.Audit.Selections[0].OptionIDs) != 0 {
		t.Fatalf("free text was mapped onto an option: %+v", free.Audit.Selections)
	}
	fenv := decodeReply(t, free.Stdout)
	assertAnswers(t, fenv.HookSpecificOutput.Decision.UpdatedInput, map[string]string{"Pick a letter?": "Alpha <b>&"})
	if !bytes.Contains(free.Stdout, []byte(`Alpha <b>&`)) {
		t.Fatalf("html escaped: %s", free.Stdout)
	}

	both := mustReply(t, batch, claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{
		{QuestionID: "q2", OptionIDs: []string{"q2o2"}},
		{QuestionID: "q1", FreeText: "Gamma"},
	}})
	benv := decodeReply(t, both.Stdout)
	rawAnswers := answerObject(t, benv.HookSpecificOutput.Decision.UpdatedInput)
	if !bytes.Contains(rawAnswers, []byte(`"Pick a letter?":"Gamma","Pick a digit?":"Two"`)) && !bytes.Contains(rawAnswers, []byte(`"Pick a letter?":"Gamma", "Pick a digit?":"Two"`)) {
		// Key order follows the question array, not the submission order or alpha sort.
		if !bytes.HasPrefix(bytes.TrimLeft(rawAnswers, "{"), []byte(`"Pick a letter?"`)) {
			t.Fatalf("answer key order: %s", rawAnswers)
		}
	}
	assertAnswers(t, benv.HookSpecificOutput.Decision.UpdatedInput, map[string]string{"Pick a letter?": "Gamma", "Pick a digit?": "Two"})

	discuss := mustReply(t, letter, claudeInteractionReply{Action: "discuss", Feedback: "  not yet <x>"})
	denv := decodeReply(t, discuss.Stdout)
	if denv.HookSpecificOutput.Decision.Behavior != "deny" || denv.HookSpecificOutput.Decision.Interrupt == nil || *denv.HookSpecificOutput.Decision.Interrupt {
		t.Fatalf("discuss decision %+v", denv.HookSpecificOutput.Decision)
	}
	if len(denv.HookSpecificOutput.Decision.UpdatedInput) != 0 || len(denv.HookSpecificOutput.Decision.UpdatedPermissions) != 0 {
		t.Fatal("discuss rewrote the tool input or permissions")
	}
	wantMsg := "Human feedback:\n  not yet <x>\n\nApplication context:\n" + interactionContextDiscuss
	if denv.HookSpecificOutput.Decision.Message != wantMsg || discuss.Audit.Feedback != "  not yet <x>" || discuss.Audit.AppContext != interactionContextDiscuss {
		t.Fatalf("discuss message=%q audit=%+v", denv.HookSpecificOutput.Decision.Message, discuss.Audit)
	}
	if bytes.Contains(discuss.Stdout, []byte(`\u003c`)) {
		t.Fatalf("feedback html-escaped: %s", discuss.Stdout)
	}
	forbiddenReplyKeys(t, discuss.Stdout)

	changes := mustReply(t, plan, claudeInteractionReply{Action: "request_changes", Feedback: "add a rollback"})
	cenv := decodeReply(t, changes.Stdout)
	if cenv.HookSpecificOutput.Decision.Behavior != "deny" || cenv.HookSpecificOutput.Decision.Interrupt == nil || *cenv.HookSpecificOutput.Decision.Interrupt {
		t.Fatal("request_changes did not stay a non-interrupting denial")
	}
	if len(cenv.HookSpecificOutput.Decision.UpdatedPermissions) != 0 || changes.Audit.Mode != "" || changes.Audit.Feedback != "add a rollback" || changes.Audit.AppContext != interactionContextChanges {
		t.Fatalf("changes gained a mode: %+v %+v", cenv.HookSpecificOutput.Decision, changes.Audit)
	}
	if !strings.Contains(cenv.HookSpecificOutput.Decision.Message, "Stay in plan mode") {
		t.Fatal("request_changes context omitted the plan-mode instruction")
	}

	cancel := mustReply(t, plan, claudeInteractionReply{Action: "cancel"})
	xenv := decodeReply(t, cancel.Stdout)
	if xenv.HookSpecificOutput.Decision.Behavior != "deny" || xenv.HookSpecificOutput.Decision.Interrupt == nil || !*xenv.HookSpecificOutput.Decision.Interrupt {
		t.Fatal("cancel was not an interrupting denial")
	}
	if len(xenv.HookSpecificOutput.Decision.UpdatedInput) != 0 || len(xenv.HookSpecificOutput.Decision.UpdatedPermissions) != 0 {
		t.Fatal("cancel fabricated an input or a mode")
	}
	if cancel.Audit.Feedback != "" || cancel.Audit.AppContext != interactionContextCancel || len(cancel.Audit.Selections) != 0 || cancel.Audit.Mode != "" {
		t.Fatalf("cancel audit %+v", cancel.Audit)
	}
	if xenv.HookSpecificOutput.Decision.Message != "Application context:\n"+interactionContextCancel {
		t.Fatalf("cancel message %q", xenv.HookSpecificOutput.Decision.Message)
	}
	forbiddenReplyKeys(t, cancel.Stdout)

	restore := mustReply(t, plan, claudeInteractionReply{Action: "approve", Mode: "restore"})
	renv := decodeReply(t, restore.Stdout)
	if renv.HookSpecificOutput.Decision.Behavior != "allow" || !bytes.Equal(renv.HookSpecificOutput.Decision.UpdatedInput, tinyPlanRaw()) {
		t.Fatalf("restore did not echo the original plan bytes: %s", renv.HookSpecificOutput.Decision.UpdatedInput)
	}
	if len(renv.HookSpecificOutput.Decision.UpdatedPermissions) != 0 || restore.Audit.Mode != "restore" || restore.Audit.ModeLabel != interactionModeRestoreLabel || restore.Audit.EstablishedMode != "" {
		t.Fatalf("restore mode %+v perms=%s", restore.Audit, renv.HookSpecificOutput.Decision.UpdatedPermissions)
	}
	if bytes.Equal(restore.Stdout, []byte(claudePermAllowJSON)) {
		t.Fatal("plan approval used a bare allow")
	}

	for _, mode := range []string{"default", "acceptEdits"} {
		approved := mustReply(t, plan, claudeInteractionReply{Action: "approve", Mode: mode})
		aenv := decodeReply(t, approved.Stdout)
		if !bytes.Equal(aenv.HookSpecificOutput.Decision.UpdatedInput, tinyPlanRaw()) {
			t.Fatal("approve rewrote the plan input")
		}
		var perms []map[string]string
		if err := json.Unmarshal(aenv.HookSpecificOutput.Decision.UpdatedPermissions, &perms); err != nil {
			t.Fatal(err)
		}
		if len(perms) != 1 || perms[0]["type"] != "setMode" || perms[0]["mode"] != mode || perms[0]["destination"] != "session" || len(perms[0]) != 3 {
			t.Fatalf("permissions %+v", perms)
		}
		wantLabel := interactionModeDefaultLabel
		if mode == "acceptEdits" {
			wantLabel = interactionModeEditsLabel
		}
		if approved.Audit.Mode != mode || approved.Audit.ModeLabel != wantLabel || approved.Audit.EstablishedMode != "" || approved.Audit.Feedback != "" {
			t.Fatalf("approve audit %+v", approved.Audit)
		}
		forbiddenReplyKeys(t, approved.Stdout)
	}

	term := mustReply(t, letter, claudeInteractionReply{Action: "terminal"})
	if !term.Terminal || len(term.Stdout) != 0 || term.Audit.Action != "terminal" || term.Audit.Feedback != "" || len(term.Audit.Selections) != 0 {
		t.Fatalf("terminal %+v", term)
	}
}

func assertAnswers(t *testing.T, updated json.RawMessage, want map[string]string) {
	t.Helper()
	var doc struct {
		Answers map[string]string `json:"answers"`
	}
	if err := json.Unmarshal(updated, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Answers) != len(want) {
		t.Fatalf("answers %+v", doc.Answers)
	}
	for k, v := range want {
		if doc.Answers[k] != v {
			t.Fatalf("answer %q=%q want %q", k, doc.Answers[k], v)
		}
	}
}

func answerObject(t *testing.T, updated json.RawMessage) []byte {
	t.Helper()
	fields, err := walkJSONObject(bytes.TrimSpace(updated))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if field.Key == "answers" {
			return field.Raw
		}
	}
	t.Fatalf("no answers in %s", updated)
	return nil
}

func TestClaudeInteractionPlanPolicy(t *testing.T) {
	plan := mustClassify(t, "ExitPlanMode", tinyPlanRaw())
	if strings.Contains(plan.Plan, "not-read.md") {
		t.Fatal("plan text was replaced by the file path")
	}
	// The path is not opened. A missing file must not change classification.
	if _, err := classifyClaudeInteraction("ExitPlanMode", tinyPlanRaw()); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeInteractionMultiJoin(t *testing.T) {
	if !claudeMultiJoinAmbiguous([]string{"A", "B", "A, B"}) {
		t.Fatal("subset join matched another label")
	}
	if !claudeMultiJoinAmbiguous([]string{"A,B", "C"}) {
		t.Fatal("comma label was treated as structured-safe")
	}
	if claudeMultiJoinAmbiguous([]string{"Red", "Green", "Blue"}) {
		t.Fatal("distinct labels were marked ambiguous")
	}
	if claudeMultiJoinAmbiguous(nil) {
		t.Fatal("empty label set was marked ambiguous")
	}
}

func TestClaudeInteractionJSONWalk(t *testing.T) {
	fields, err := walkJSONObject([]byte(` {"a":1,"b":"x","c":{"d":[true,false,null]},"e":-12.5e+1} `))
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields[0].Key != "a" || string(fields[0].Raw) != "1" || fields[1].Key != "b" || string(fields[1].Raw) != `"x"` || fields[2].Key != "c" || fields[3].Key != "e" || string(fields[3].Raw) != "-12.5e+1" {
		t.Fatalf("fields %+v", fields)
	}
	if !bytes.Contains(fields[2].Raw, []byte(`"d":[true,false,null]`)) {
		t.Fatalf("nested value %s", fields[2].Raw)
	}
	empty, err := walkJSONObject([]byte(` { } `))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty err=%v fields=%v", err, empty)
	}
	escaped, err := walkJSONObject([]byte(`{"a\u0041":"\"\\\b\f\n\r\t\u0042\/"}`))
	if err != nil || escaped[0].Key != "aA" {
		t.Fatalf("escape err=%v fields=%v", err, escaped)
	}
	emoji, err := walkJSONObject([]byte(`{"face":"\uD83D\uDE00"}`))
	if err != nil || len(emoji) != 1 {
		t.Fatal(err)
	}
	bad := []string{
		``,
		`[]`,
		`{`,
		`{"a"}`,
		`{"a":}`,
		`{,"a":1}`,
		`{"a":1,}`,
		`{"a":1,"a":2}`,
		`{"a":tru}`,
		`{"a":01}`,
		`{"a":"unterminated}`,
		`{"a":"\q"}`,
		`{"\uD800":1}`,
		`{"a":"bad` + "\n" + `"}`,
		`{"a":1} x`,
		`{"a":1.}`,
		`{"a":1e}`,
		`{"a":-}`,
		`{"a":trueX}`,
	}
	zero, err := walkJSONObject([]byte(`{"z":0}`))
	if err != nil || len(zero) != 1 || string(zero[0].Raw) != "0" {
		t.Fatalf("zero err=%v fields=%v", err, zero)
	}
	for _, raw := range bad {
		if _, err := walkJSONObject([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if benignJSONValue([]byte(`nope`)) || benignJSONValue([]byte(`false true`)) {
		t.Fatal("benign check accepted damaged JSON")
	}
	if _, err := endJSONString([]byte(`abc`), 0); err == nil {
		t.Fatal("endJSONString accepted a non-string")
	}
	if _, err := endJSONString([]byte(`"\`), 0); err == nil {
		t.Fatal("endJSONString accepted a truncated escape")
	}
	if _, err := endJSONString([]byte(`"\u12"`), 0); err == nil {
		t.Fatal("endJSONString accepted a short unicode escape")
	}
	if _, err := endJSONString([]byte(`"\uGGGG"`), 0); err == nil {
		t.Fatal("endJSONString accepted a bad hex escape")
	}
	if _, err := endJSONString([]byte(`"unterminated`), 0); err == nil {
		t.Fatal("endJSONString accepted an unterminated string")
	}
	if _, _, err := decodeJSONString([]byte(`"\q"`), 0); err == nil {
		t.Fatal("decodeJSONString accepted a bad escape")
	}
	if _, err := skipJSONContainer([]byte{}, 0, '{', '}'); err == nil {
		t.Fatal("skipJSONContainer accepted an empty buffer")
	}
	if _, err := skipInteractionValue([]byte{}, 0); err == nil {
		t.Fatal("skipInteractionValue accepted an empty buffer")
	}
	if _, err := objectFields([]byte(`{"a":1`)); err == nil {
		t.Fatal("objectFields accepted a truncated object")
	}
	if _, err := arrayElements([]byte(`[1`)); err == nil {
		t.Fatal("arrayElements accepted a truncated array")
	}
	if _, err := arrayElements([]byte(`[1 2]`)); err == nil {
		t.Fatal("arrayElements accepted a missing comma")
	}
	if _, err := arrayElements([]byte(`[1,]`)); err == nil {
		t.Fatal("arrayElements accepted a trailing comma")
	}
	if _, err := walkJSONArray([]byte(`[1] x`)); err == nil {
		t.Fatal("walkJSONArray accepted trailing bytes")
	}
	if _, err := walkJSONArray([]byte(`[`)); err == nil {
		t.Fatal("walkJSONArray accepted a truncated array")
	}
	if _, err := arrayElements([]byte(`[`)); err == nil {
		t.Fatal("arrayElements accepted a missing value")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("marshal accepted a function")
		}
	}()
	marshalInteractionValue(func() {})
}

func TestClaudeInteractionQuestionsStayIneligibleForLease(t *testing.T) {
	now := time.Now()
	ctx := claudePermContext{Armed: true, LeaseID: "lease-1", SessionID: "sess-1", Now: now}
	base := claudePermRequest{
		Lease: "lease-1",
		ID:    "req-1",
		At:    now.Add(-time.Second).UTC().Format(time.RFC3339Nano),
		Event: claudePermissionEvent{
			HookEventName:  "PermissionRequest",
			SessionID:      "sess-1",
			PermissionMode: "acceptEdits",
			ToolName:       "Bash",
			ToolInput:      json.RawMessage(`{"command":"true"}`),
		},
	}
	if !eligibleClaudePermission(ctx, base) {
		t.Fatal("ordinary bash approval stopped being eligible")
	}
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode"} {
		req := base
		req.Event.ToolName = tool
		req.Event.PermissionMode = "default"
		if eligibleClaudePermission(ctx, req) {
			t.Fatalf("%s became lease-eligible", tool)
		}
		if reason := claudePermDeclineReason(ctx, req); reason != "auto-approve does not answer questions or plan choices" {
			t.Fatalf("%s decline %q", tool, reason)
		}
	}
	// An armed lease still does not make a later question eligible, and a plan-mode
	// ordinary tool stays excluded by the existing fence.
	base.Event.PermissionMode = "plan"
	base.Event.ToolName = "Bash"
	if eligibleClaudePermission(ctx, base) {
		t.Fatal("plan-mode bash became eligible")
	}
}
