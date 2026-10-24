package app

import (
	"bytes"
	"testing"
)

type pinOpt struct {
	id, label string
}

type pinQ struct {
	text  string
	multi bool
	opts  []pinOpt
}

type pinRow struct {
	name   string
	tool   string
	raw    string
	native bool
	reason string
	qs     []pinQ
	plan   string
	digest string
	reply  claudeInteractionReply
	stdout string
	hasOut bool
}

// pin: BASE classification, parsed model and stdout for inputs that are not
// changed by the replacement-rune policy, including D2 inputs BASE already
// rejects as malformed json.
func TestClaudeInteractionJSONCharacterization(t *testing.T) {
	rows := []pinRow{
		{name: "minimal two options", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "23158d133e1f2bad1bad5c17b27f3213a22e469d22f5a71a59ae7e6a7afdb7e1", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "one option", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"Only\"}]}]}", native: true, reason: "option count"},
		{name: "invalid utf-8 in question", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q\xff?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "invalid utf-8"},
		{name: "top-level replacement string", tool: "AskUserQuestion", raw: "\"\\ufffd\"", native: true, reason: "not object"},
		{name: "minimal plan", tool: "ExitPlanMode", raw: "{\"plan\":\"P\"}", native: false, reason: "", plan: "P", digest: "951936c03ca790450c06c107a9e6378605248c7907db46eaf2264fbd850ef30d", reply: claudeInteractionReply{Action: "approve", Mode: "restore"}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"plan\":\"P\"}}}}", hasOut: true},
		{name: "multi questions preview description", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q1?\",\"options\":[{\"label\":\"A\",\"description\":\"d\"},{\"label\":\"B\",\"description\":null}],\"preview\":\"seen\",\"multiSelect\":false},{\"question\":\"Q2?\",\"options\":[{\"label\":\"C\"},{\"label\":\"D\",\"description\":null}],\"preview\":null,\"multiSelect\":true}]}", native: false, reason: "", plan: "", digest: "32e4220d7c2b2852f1906e2dfbb4af70b4d02a3e03c128c427144d159f9ad604", qs: []pinQ{{text: "Q1?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}, {text: "Q2?", multi: true, opts: []pinOpt{{id: "q2o1", label: "C"}, {id: "q2o2", label: "D"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}, {QuestionID: "q2", OptionIDs: []string{"q2o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"questions\":[{\"question\":\"Q1?\",\"options\":[{\"label\":\"A\",\"description\":\"d\"},{\"label\":\"B\",\"description\":null}],\"preview\":\"seen\",\"multiSelect\":false},{\"question\":\"Q2?\",\"options\":[{\"label\":\"C\"},{\"label\":\"D\",\"description\":null}],\"preview\":null,\"multiSelect\":true}],\"answers\":{\"Q1?\":\"A\",\"Q2?\":\"C\"}}}}}", hasOut: true},
		{name: "questions null", tool: "AskUserQuestion", raw: "{\"questions\":null}", native: true, reason: "question shape"},
		{name: "options null", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":null}]}", native: true, reason: "option shape"},
		{name: "benign \"\"", tool: "AskUserQuestion", raw: "{\"extra\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "68568c46f18d7f30d0ef532e17f9d6ca208aebedf788c892015c25bc6dc81fee", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"extra\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "benign false", tool: "AskUserQuestion", raw: "{\"extra\":false,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "0238e8571ba71d3078de22464172d0f9d3c4c0e9e209db89b445b8fc3c367308", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"extra\":false,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "benign []", tool: "AskUserQuestion", raw: "{\"extra\":[],\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "93b79b43e2ea5bd5d57286452acffb326871d8d65f001f5c759ebb0311cd8367", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"extra\":[],\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "benign {}", tool: "AskUserQuestion", raw: "{\"extra\":{},\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "8742dc5f0248f020a11135be1ceb097dddac61d4c1df800ce8641e494db22250", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"extra\":{},\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "benign null", tool: "AskUserQuestion", raw: "{\"extra\":null,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "d5ab3a6577c630807b836cbfedc8fbd24cbcb46320e188ef787666a59bee7ba2", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"extra\":null,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "nonbenign true", tool: "AskUserQuestion", raw: "{\"extra\":true,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "unknown member"},
		{name: "answers present", tool: "AskUserQuestion", raw: "{\"answers\":{},\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "answers present"},
		{name: "dup top", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"questions\":[]}", native: true, reason: "malformed json"},
		{name: "dup question key", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"question\":\"Z?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "question shape"},
		{name: "dup option key", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\",\"label\":\"Z\"},{\"label\":\"B\"}]}]}", native: true, reason: "option shape"},
		{name: "dup escape key", tool: "AskUserQuestion", raw: "{\"a\":\"\",\"\\u0061\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "malformed json"},
		{name: "escapes in question", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "616cee5e7d4d4289fba20eae82f461aacd47e6aa63e518fd611e22592ababf8c", qs: []pinQ{{text: "quote \" slash \\ solidus / nl \n e é face 😀", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"questions\":[{\"question\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"quote \\\" slash \\\\ solidus / nl \\n e é face 😀\":\"A\"}}}}}", hasOut: true},
		{name: "escapes in label", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\"},{\"label\":\"B\"}]}]}", native: false, reason: "", plan: "", digest: "2eb6e4cd0d8d6165926038750aca16d0bec3ef5681267379f33a209d67ab398f", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "quote \" slash \\ solidus / nl \n e é face 😀"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"quote \\\" slash \\\\ solidus / nl \\n e é face 😀\"}}}}}", hasOut: true},
		{name: "escapes in plan", tool: "ExitPlanMode", raw: "{\"plan\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\"}", native: false, reason: "", plan: "quote \" slash \\ solidus / nl \n e é face 😀", digest: "78b73baa505d05ac6eb9916b77f1f6883fa01a89c1efc5bea196bd60742048a2", reply: claudeInteractionReply{Action: "approve", Mode: "restore"}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"plan\":\"quote \\\" slash \\\\ solidus \\/ nl \\n e \\u00e9 face \\ud83d\\ude00\"}}}}", hasOut: true},
		{name: "whitespace", tool: "AskUserQuestion", raw: " \n\t { \"questions\" : [ { \"question\" : \"Q?\" , \"options\" : [ { \"label\" : \"A\" } , { \"label\" : \"B\" } ] } ] } \r\n", native: false, reason: "", plan: "", digest: "85975ceb88ae7f76a863a0e9031801fe2ac7f0702eda80976add7edd6ad608f1", qs: []pinQ{{text: "Q?", multi: false, opts: []pinOpt{{id: "q1o1", label: "A"}, {id: "q1o2", label: "B"}}}}, reply: claudeInteractionReply{Action: "answer", Answers: []claudeInteractionAnswer{{QuestionID: "q1", OptionIDs: []string{"q1o1"}}}}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}],\"answers\":{\"Q?\":\"A\"}}}}}", hasOut: true},
		{name: "trailing garbage", tool: "AskUserQuestion", raw: "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]} x", native: true, reason: "malformed json"},
		{name: "top-level array", tool: "AskUserQuestion", raw: "[{\"question\":\"Q?\"}]", native: true, reason: "not object"},
		{name: "empty object question", tool: "AskUserQuestion", raw: "{}", native: true, reason: "question count"},
		{name: "empty object plan", tool: "ExitPlanMode", raw: "{}", native: true, reason: "plan missing"},
		{name: "number nonbenign 0", tool: "AskUserQuestion", raw: "{\"n\":0,\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "unknown member"},
		{name: "number nonbenign sci", tool: "ExitPlanMode", raw: "{\"plan\":\"P\",\"n\":-1.5e3}", native: true, reason: "unknown member"},
		{name: "number benign 0", tool: "ExitPlanMode", raw: "{\"plan\":\"P\",\"allowedPrompts\":[0]}", native: false, reason: "", plan: "P", digest: "9caf532fdbdb7cff277f1bc743abe3eeccbd80dadc9777db64ceb1e055c5db6e", reply: claudeInteractionReply{Action: "approve", Mode: "restore"}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"plan\":\"P\",\"allowedPrompts\":[0]}}}}", hasOut: true},
		{name: "number benign sci", tool: "ExitPlanMode", raw: "{\"plan\":\"P\",\"allowedPrompts\":[-1.5e3]}", native: false, reason: "", plan: "P", digest: "269834ae7b94b4aede9756cd7b34d93c2b45333dce4fe8ccb128cc5aa198da68", reply: claudeInteractionReply{Action: "approve", Mode: "restore"}, stdout: "{\"hookSpecificOutput\":{\"hookEventName\":\"PermissionRequest\",\"decision\":{\"behavior\":\"allow\",\"updatedInput\":{\"plan\":\"P\",\"allowedPrompts\":[-1.5e3]}}}}", hasOut: true},
		{name: "lit 00", tool: "AskUserQuestion", raw: " {\"a\":1,\"b\":\"x\",\"c\":{\"d\":[true,false,null]},\"e\":-12.5e+1} ", native: true, reason: "unknown member"},
		{name: "lit 01", tool: "AskUserQuestion", raw: " { } ", native: true, reason: "question count"},
		{name: "lit 02", tool: "AskUserQuestion", raw: "{\"a\\u0041\":\"\\\"\\\\\\b\\f\\n\\r\\t\\u0042\\/\"}", native: true, reason: "unknown member"},
		{name: "lit 03", tool: "AskUserQuestion", raw: "{\"face\":\"\\uD83D\\uDE00\"}", native: true, reason: "unknown member"},
		{name: "lit 04", tool: "AskUserQuestion", raw: "", native: true, reason: "not object"},
		{name: "lit 05", tool: "AskUserQuestion", raw: "[]", native: true, reason: "not object"},
		{name: "lit 06", tool: "AskUserQuestion", raw: "{", native: true, reason: "malformed json"},
		{name: "lit 07", tool: "AskUserQuestion", raw: "{\"a\"}", native: true, reason: "malformed json"},
		{name: "lit 08", tool: "AskUserQuestion", raw: "{\"a\":}", native: true, reason: "malformed json"},
		{name: "lit 09", tool: "AskUserQuestion", raw: "{,\"a\":1}", native: true, reason: "malformed json"},
		{name: "lit 10", tool: "AskUserQuestion", raw: "{\"a\":1,}", native: true, reason: "malformed json"},
		{name: "lit 11", tool: "AskUserQuestion", raw: "{\"a\":1,\"a\":2}", native: true, reason: "malformed json"},
		{name: "lit 12", tool: "AskUserQuestion", raw: "{\"a\":tru}", native: true, reason: "malformed json"},
		{name: "lit 13", tool: "AskUserQuestion", raw: "{\"a\":01}", native: true, reason: "malformed json"},
		{name: "lit 14", tool: "AskUserQuestion", raw: "{\"a\":\"unterminated}", native: true, reason: "malformed json"},
		{name: "lit 15", tool: "AskUserQuestion", raw: "{\"a\":\"\\q\"}", native: true, reason: "malformed json"},
		{name: "lit 16", tool: "AskUserQuestion", raw: "{\"\\uD800\":1}", native: true, reason: "malformed json"},
		{name: "lit 17", tool: "AskUserQuestion", raw: "{\"a\":\"bad\n\"}", native: true, reason: "malformed json"},
		{name: "lit 18", tool: "AskUserQuestion", raw: "{\"a\":1} x", native: true, reason: "malformed json"},
		{name: "lit 19", tool: "AskUserQuestion", raw: "{\"a\":1.}", native: true, reason: "malformed json"},
		{name: "lit 20", tool: "AskUserQuestion", raw: "{\"a\":1e}", native: true, reason: "malformed json"},
		{name: "lit 21", tool: "AskUserQuestion", raw: "{\"a\":-}", native: true, reason: "malformed json"},
		{name: "lit 22", tool: "AskUserQuestion", raw: "{\"a\":trueX}", native: true, reason: "malformed json"},
		{name: "lit 23", tool: "AskUserQuestion", raw: "{\"z\":0}", native: true, reason: "unknown member"},
		{name: "lit 24", tool: "AskUserQuestion", raw: "nope", native: true, reason: "not object"},
		{name: "lit 25", tool: "AskUserQuestion", raw: "false true", native: true, reason: "not object"},
		{name: "lit 26", tool: "AskUserQuestion", raw: "abc", native: true, reason: "not object"},
		{name: "lit 27", tool: "AskUserQuestion", raw: "\"\\", native: true, reason: "not object"},
		{name: "lit 28", tool: "AskUserQuestion", raw: "\"\\u12\"", native: true, reason: "not object"},
		{name: "lit 29", tool: "AskUserQuestion", raw: "\"\\uGGGG\"", native: true, reason: "not object"},
		{name: "lit 30", tool: "AskUserQuestion", raw: "\"unterminated", native: true, reason: "not object"},
		{name: "lit 31", tool: "AskUserQuestion", raw: "\"\\q\"", native: true, reason: "not object"},
		{name: "lit 32", tool: "AskUserQuestion", raw: "{\"a\":1", native: true, reason: "malformed json"},
		{name: "lit 33", tool: "AskUserQuestion", raw: "[1", native: true, reason: "not object"},
		{name: "lit 34", tool: "AskUserQuestion", raw: "[1 2]", native: true, reason: "not object"},
		{name: "lit 35", tool: "AskUserQuestion", raw: "[1,]", native: true, reason: "not object"},
		{name: "lit 36", tool: "AskUserQuestion", raw: "[1] x", native: true, reason: "not object"},
		{name: "lit 37", tool: "AskUserQuestion", raw: "[", native: true, reason: "not object"},
		// BASE already native "malformed json".
		{name: "d2 plan high", tool: "ExitPlanMode", raw: "{\"plan\":\"\\ud800\"}", native: true, reason: "malformed json"},
		// BASE already native "malformed json".
		{name: "d2 key high", tool: "AskUserQuestion", raw: "{\"\\ud800\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "malformed json"},
		// BASE already native "malformed json".
		{name: "d2 plan low", tool: "ExitPlanMode", raw: "{\"plan\":\"\\udc00\"}", native: true, reason: "malformed json"},
		// BASE already native "malformed json".
		{name: "d2 key low", tool: "AskUserQuestion", raw: "{\"\\udc00\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "malformed json"},
		// BASE already native "malformed json".
		{name: "d2 plan mispair", tool: "ExitPlanMode", raw: "{\"plan\":\"\\ud800\\u0041\"}", native: true, reason: "malformed json"},
		// BASE already native "malformed json".
		{name: "d2 key mispair", tool: "AskUserQuestion", raw: "{\"\\ud800\\u0041\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", native: true, reason: "malformed json"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			model, err := classifyClaudeInteraction(row.tool, []byte(row.raw))
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if model.Native != row.native || model.NativeReason != row.reason {
				t.Fatalf("native=%v reason=%q, want native=%v reason=%q", model.Native, model.NativeReason, row.native, row.reason)
			}
			if model.Plan != row.plan || model.Digest != row.digest {
				t.Fatalf("plan=%q digest=%s, want plan=%q digest=%s", model.Plan, model.Digest, row.plan, row.digest)
			}
			if len(model.Questions) != len(row.qs) {
				t.Fatalf("questions=%d, want %d", len(model.Questions), len(row.qs))
			}
			for i, want := range row.qs {
				got := model.Questions[i]
				if got.Text != want.text || got.Multi != want.multi || len(got.Options) != len(want.opts) {
					t.Fatalf("question %d = %+v, want text %q multi %v", i, got, want.text, want.multi)
				}
				for j, opt := range want.opts {
					if got.Options[j].ID != opt.id || got.Options[j].Label != opt.label {
						t.Fatalf("option %d.%d = %+v, want %+v", i, j, got.Options[j], opt)
					}
				}
			}
			if !row.hasOut {
				return
			}
			env, err := buildClaudeInteractionReply(model, row.reply)
			if err != nil {
				t.Fatalf("reply: %v", err)
			}
			if !bytes.Equal(env.Stdout, []byte(row.stdout)) {
				t.Fatalf("stdout = %s, want %s", env.Stdout, row.stdout)
			}
		})
	}
}

// pin: marshalInteractionValue panics on a value encoding/json cannot encode.
func TestMarshalInteractionValuePanicsOnUnencodable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("marshal accepted a function")
		}
	}()
	marshalInteractionValue(func() {})
}

// pin: decodeJSONStringValue rejects the invalid Unicode inputs already
// covered by TestSixthCachesRemainBoundedAndFailClosed.
func TestDecodeJSONStringValueRejectsInvalidUnicode(t *testing.T) {
	for _, raw := range []string{`"ok" trailing`, `"\ud800\u0041"`, `"\ud800\uZZZZ"`, `"\udc00\ud800"`} {
		if _, ok := decodeJSONStringValue([]byte(raw)); ok {
			t.Fatalf("invalid Unicode or trailing data accepted: %s", raw)
		}
	}
	if _, ok := decodeJSONStringValue([]byte{'"', 0xff, '"'}); ok {
		t.Fatal("invalid UTF-8 accepted")
	}
}

// pin: walkJSONObject keeps BASE's object contract.
func TestWalkJSONObjectContract(t *testing.T) {
	if _, err := walkJSONObject([]byte(`[]`)); err == nil {
		t.Fatal("non-object accepted")
	}
	empty, err := walkJSONObject([]byte(` { } `))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty err=%v fields=%v", err, empty)
	}
	if _, err := walkJSONObject([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := walkJSONObject([]byte(`{"a":1} x`)); err == nil {
		t.Fatal("trailing bytes accepted")
	}
	fields, err := walkJSONObject([]byte(`{"a": { "b" : 1 }}`))
	if err != nil || len(fields) != 1 || string(fields[0].Raw) != `{ "b" : 1 }` {
		t.Fatalf("raw err=%v fields=%v", err, fields)
	}
}

// pin: walkJSONArray keeps BASE's array contract.
func TestWalkJSONArrayContract(t *testing.T) {
	if _, err := walkJSONArray([]byte(`null`)); err == nil {
		t.Fatal("null accepted")
	}
	if _, err := walkJSONArray([]byte(`{}`)); err == nil {
		t.Fatal("non-array accepted")
	}
	empty, err := walkJSONArray([]byte(` [ ] `))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty err=%v elems=%v", err, empty)
	}
	elems, err := walkJSONArray([]byte(`[ { "a" : 1 } ]`))
	if err != nil || len(elems) != 1 || string(elems[0]) != `{ "a" : 1 }` {
		t.Fatalf("raw err=%v elems=%q", err, elems)
	}
}

// pin: decodeJSONStringValue rejects a non-string.
func TestDecodeJSONStringValueRejectsNonString(t *testing.T) {
	if _, ok := decodeJSONStringValue([]byte(`1`)); ok {
		t.Fatal("number accepted as a string")
	}
	if _, ok := decodeJSONStringValue([]byte(`null`)); ok {
		t.Fatal("null accepted as a string")
	}
}

// pin: spliceJSONMember keeps BASE's insertion points.
func TestSpliceJSONMemberContract(t *testing.T) {
	got, err := spliceJSONMember([]byte(`{}`), "k", []byte(`1`))
	if err != nil || string(got) != `{"k":1}` {
		t.Fatalf("empty err=%v got=%s", err, got)
	}
	got, err = spliceJSONMember([]byte(`{"a":1}`), "k", []byte(`2`))
	if err != nil || string(got) != `{"a":1,"k":2}` {
		t.Fatalf("nonempty err=%v got=%s", err, got)
	}
	got, err = spliceJSONMember([]byte("{ \"a\" : 1 } \n"), "k", []byte(`2`))
	if err != nil || string(got) != "{ \"a\" : 1 ,\"k\":2} \n" {
		t.Fatalf("whitespace err=%v got=%q", err, string(got))
	}
	if _, err := spliceJSONMember([]byte(`[]`), "k", []byte(`1`)); err == nil {
		t.Fatal("non-object accepted")
	}
}

// spliceJSONMember reports appendJSONString's error for a key that is not UTF-8.
func TestSpliceJSONMemberRejectsInvalidUTF8Key(t *testing.T) {
	if _, err := spliceJSONMember([]byte(`{}`), "\xff", []byte(`1`)); err == nil {
		t.Fatal("invalid UTF-8 key was accepted")
	}
}

// red: D2 inputs that BASE did not already classify as native malformed json.
func TestClaudeInteractionJSONUnicodePolicy(t *testing.T) {
	rows := []struct {
		name, tool, raw, base string
	}{
		{"d2 question high", "AskUserQuestion", "{\"questions\":[{\"question\":\"\\ud800\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=true reason=\"question shape\""},
		{"d2 label high", "AskUserQuestion", "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"\\ud800\"},{\"label\":\"B\"}]}]}", "native=true reason=\"option shape\""},
		{"d2 opaque high", "ExitPlanMode", "{\"plan\":\"P\",\"allowedPrompts\":[{\"prompt\":\"\\ud800\"}]}", "native=false reason=\"\""},
		{"d2 question low", "AskUserQuestion", "{\"questions\":[{\"question\":\"\\udc00\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=true reason=\"question shape\""},
		{"d2 label low", "AskUserQuestion", "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"\\udc00\"},{\"label\":\"B\"}]}]}", "native=true reason=\"option shape\""},
		{"d2 opaque low", "ExitPlanMode", "{\"plan\":\"P\",\"allowedPrompts\":[{\"prompt\":\"\\udc00\"}]}", "native=false reason=\"\""},
		{"d2 question mispair", "AskUserQuestion", "{\"questions\":[{\"question\":\"\\ud800\\u0041\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=true reason=\"question shape\""},
		{"d2 label mispair", "AskUserQuestion", "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"\\ud800\\u0041\"},{\"label\":\"B\"}]}]}", "native=true reason=\"option shape\""},
		{"d2 opaque mispair", "ExitPlanMode", "{\"plan\":\"P\",\"allowedPrompts\":[{\"prompt\":\"\\ud800\\u0041\"}]}", "native=false reason=\"\""},
		{"d2 question literal", "AskUserQuestion", "{\"questions\":[{\"question\":\"�\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 label literal", "AskUserQuestion", "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"�\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 plan literal", "ExitPlanMode", "{\"plan\":\"�\"}", "native=false reason=\"\""},
		{"d2 key literal", "AskUserQuestion", "{\"�\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 opaque literal", "ExitPlanMode", "{\"plan\":\"P\",\"allowedPrompts\":[{\"prompt\":\"�\"}]}", "native=false reason=\"\""},
		{"d2 question esc", "AskUserQuestion", "{\"questions\":[{\"question\":\"\\ufffd\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 label esc", "AskUserQuestion", "{\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"\\ufffd\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 plan esc", "ExitPlanMode", "{\"plan\":\"\\ufffd\"}", "native=false reason=\"\""},
		{"d2 key esc", "AskUserQuestion", "{\"\\ufffd\":\"\",\"questions\":[{\"question\":\"Q?\",\"options\":[{\"label\":\"A\"},{\"label\":\"B\"}]}]}", "native=false reason=\"\""},
		{"d2 opaque esc", "ExitPlanMode", "{\"plan\":\"P\",\"allowedPrompts\":[{\"prompt\":\"\\ufffd\"}]}", "native=false reason=\"\""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			model, err := classifyClaudeInteraction(row.tool, []byte(row.raw))
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !model.Native || model.NativeReason != "malformed json" {
				t.Fatalf("got native=%v reason=%q, want native malformed json (%s)", model.Native, model.NativeReason, row.base)
			}
		})
	}
}

// red: jsonHasReplacementRune reports U+FFFD from every source, in keys and values.
func TestJSONHasReplacementRune(t *testing.T) {
	trueCases := []string{
		`{"a":{"b":["\ud800"]}}`,
		`{"\ud800":[1]}`,
		`{"a":[{"b":"\udc00"}]}`,
		`{"\udc00":{"x":1}}`,
		`{"a":{"b":"\ud800\u0041"}}`,
		`{"\ud800\u0041":[true]}`,
		"{\"nested\":[{\"\uFFFD\":1}]}",
		"{\"a\":[\"\uFFFD\"]}",
		`{"a":{"b":["\ufffd"]}}`,
		`{"\ufffd":{"ok":[]}}`,
	}
	for _, raw := range trueCases {
		if !jsonHasReplacementRune([]byte(raw)) {
			t.Fatalf("%s: got false, want true", raw)
		}
	}
	falseCases := []string{
		`{"plan":"P","face":"\ud83d\ude00"}`,
		`{"\ud83d\ude00":["ok"]}`,
		`{"a":1}`,
		``,
		`{`,
		`not-json`,
		`{"a":`,
	}
	for _, raw := range falseCases {
		if jsonHasReplacementRune([]byte(raw)) {
			t.Fatalf("%s: got true, want false", raw)
		}
	}
}

// red: decodeJSONStringValue accepts a replacement character at BASE.
func TestDecodeJSONStringValueRejectsReplacementChar(t *testing.T) {
	if _, ok := decodeJSONStringValue([]byte("\"" + "\uFFFD" + "\"")); ok {
		t.Fatal("literal U+FFFD was accepted")
	}
	if _, ok := decodeJSONStringValue([]byte("\"\\ufffd\"")); ok {
		t.Fatal("\\ufffd was accepted")
	}
}

// pin: walkJSONObject accepts a key encoded as a valid surrogate pair.
func TestWalkJSONObjectAcceptsSurrogatePairKey(t *testing.T) {
	fields, err := walkJSONObject([]byte(`{"\ud83d\ude00":1}`))
	if err != nil {
		t.Fatalf("valid surrogate pair key: %v", err)
	}
	if len(fields) != 1 || fields[0].Key != "\U0001F600" || string(fields[0].Raw) != "1" {
		t.Fatalf("fields=%+v", fields)
	}
}

// red: walkJSONObject accepts a decoded key containing U+FFFD at BASE.
func TestWalkJSONObjectRejectsReplacementKey(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "literal U+FFFD", raw: []byte("{\"\ufffd\":1}")},
		{name: `escaped \ufffd`, raw: []byte(`{"\ufffd":1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := walkJSONObject(tc.raw); err == nil {
				t.Fatal("replacement key was accepted")
			}
		})
	}
}
