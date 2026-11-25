package team

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestReactionCatalogAndReplay(t *testing.T) {
	want := []ReactionChoice{
		{ID: "celebration", Emoji: "🎉"},
		{ID: "thinking", Emoji: "🤔"},
		{ID: "laughing", Emoji: "😂"},
		{ID: "heart", Emoji: "❤️"},
		{ID: "thumbs_down", Emoji: "👎"},
		{ID: "thumbs_up", Emoji: "👍"},
	}
	if got := ReactionChoices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReactionChoices() = %#v, want %#v", got, want)
	}
	records := []MirrorRecord{
		comment(1, "alice", `{"v":1,"t":"comment","text":"hello","anchor":0}`),
		reaction(2, "bob", 1, "thumbs_up", true),
		reaction(3, "carol", 1, "thumbs_up", true),
		reaction(4, "bob", 1, "thumbs_up", false),
		reaction(5, "bob", 1, "heart", true),
		reaction(6, "bob", 1, "heart", false),
	}
	got := ReplayReactions(records, "bob")
	wantState := []ReactionCount{{Target: 1, Emoji: "thumbs_up", Glyph: "👍", Count: 1, Mine: false}}
	if !reflect.DeepEqual(got, wantState) {
		t.Fatalf("ReplayReactions() = %#v, want %#v", got, wantState)
	}
}

func TestReactionReplayRejectsInvalidTargetsAndEvents(t *testing.T) {
	records := []MirrorRecord{
		comment(1, "alice", `{"v":1,"t":"comment","text":"hi","anchor":0}`),
		reaction(2, "alice", 1, "thumbs_up", true), // comment author cannot react to own comment
		reaction(3, "bob", 9, "thumbs_up", true),   // target has not appeared
		reaction(4, "bob", 1, "thumbsup", true),    // unknown identifier
		reaction(5, "bob", 1, "heart", true),
		{Seq: 6, Author: "carol", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"heart","active":"yes"}`)},
		comment(7, "dave", `{"v":1,"t":"comment","text":"","anchor":0}`), // not a valid message
		reaction(8, "bob", 7, "laughing", true),
		{Seq: 9, Author: "eve", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"heart","active":true,"actor":"alice"}`)},
	}
	got := ReplayReactions(records, "bob")
	want := []ReactionCount{{Target: 1, Emoji: "heart", Glyph: "❤️", Count: 1, Mine: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReplayReactions() = %#v, want %#v", got, want)
	}
}

func TestReactionReplayOrdersBySequenceAndPaginates(t *testing.T) {
	// Page boundaries retain the earlier target and latest valid event.
	first := []MirrorRecord{comment(1, "alice", `{"v":1,"t":"comment","text":"hi","anchor":0}`)}
	second := []MirrorRecord{reaction(2, "bob", 1, "laughing", true), reaction(3, "bob", 1, "laughing", false)}
	got := ReplayReactionPages([][]MirrorRecord{first, second}, "bob")
	if len(got) != 0 {
		t.Fatalf("ReplayReactionPages() = %#v, want empty after removal", got)
	}
}

func TestReactionParserValidationAndOrderingEdges(t *testing.T) {
	validComment := comment(1, "alice", `{"v":1,"t":"comment","text":"hello","anchor":0}`)
	fileOnly := comment(2, "alice", `{"v":1,"t":"comment","text":"","anchor":0,"attachments":[{}]}`)
	if !ValidPeopleCommentRecord(validComment) || !ValidPeopleCommentRecord(fileOnly) {
		t.Fatal("valid text and file-only comments should be reaction targets")
	}
	for _, bad := range []MirrorRecord{
		{},
		comment(1, "", `{"v":1,"t":"comment","text":"x","anchor":0}`),
		comment(1, "alice", `{`),
		comment(1, "alice", `{"v":2,"t":"comment","text":"x","anchor":0}`),
		comment(1, "alice", `{"v":1,"t":"other","text":"x","anchor":0}`),
		comment(1, "alice", `{"v":1,"t":"comment","text":"","anchor":0}`),
	} {
		if ValidPeopleCommentRecord(bad) {
			t.Errorf("accepted invalid target: %+v", bad)
		}
	}
	base := reaction(3, "bob", 1, "heart", true)
	for _, test := range []struct {
		name           string
		record, target MirrorRecord
	}{
		{"zero sequence", MirrorRecord{Key: base.Key, Author: "bob", Body: base.Body}, validComment},
		{"bad key", MirrorRecord{Seq: 3, Key: "r/0123456789abcdeg", Author: "bob", Body: base.Body}, validComment},
		{"empty actor", MirrorRecord{Seq: 3, Key: base.Key, Body: base.Body}, validComment},
		{"future target", base, comment(4, "alice", `{"v":1,"t":"comment","text":"x","anchor":0}`)},
		{"self reaction", reaction(3, "alice", 1, "heart", true), validComment},
		{"malformed body", MirrorRecord{Seq: 3, Key: base.Key, Author: "bob", Body: json.RawMessage(`{`)}, validComment},
		{"missing required field", MirrorRecord{Seq: 3, Key: base.Key, Author: "bob", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"heart"}`)}, validComment},
		{"wrong target", reaction(3, "bob", 2, "heart", true), validComment},
		{"trailing value", MirrorRecord{Seq: 3, Key: base.Key, Author: "bob", Body: json.RawMessage(`{"v":1,"t":"reaction","target":1,"emoji":"heart","active":true} {}`)}, validComment},
		{"invalid target body", base, comment(1, "alice", `{"v":1,"t":"comment","text":"","anchor":0}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, ok := ParseReactionEvent(test.record, test.target); ok {
				t.Fatal("invalid event was accepted")
			}
		})
	}
	if _, ok := ParseReactionEvent(base, validComment); !ok {
		t.Fatal("valid event was rejected")
	}
	if validReactionKey("r/0123456789abcdeg") {
		t.Fatal("accepted non-hex idempotency key")
	}

	// Duplicate/out-of-order records are ignored; sorting exercises both target
	// and fixed emoji order comparisons.
	got := ReplayReactions([]MirrorRecord{
		validComment,
		reaction(2, "bob", 1, "thumbs_up", true),
		comment(3, "carol", `{"v":1,"t":"comment","text":"next","anchor":0}`),
		reaction(4, "bob", 3, "heart", true),
		reaction(5, "bob", 1, "heart", true),
		reaction(2, "dave", 1, "heart", true),
		comment(6, "eve", `{"v":1,"t":"other"}`),
		{Seq: 7, Author: "", Body: json.RawMessage(`{"v":1,"t":"reaction"}`)},
		{Seq: 8, Author: "frank", Body: json.RawMessage(`{`)},
		{Seq: 9, Author: "frank", Body: json.RawMessage(`{}`)},
		{Seq: 10, Author: "frank", Body: json.RawMessage(`{"v":1,"t":"other"}`)},
		{Seq: 11, Author: "frank", Body: json.RawMessage(`{"v":1,"t":"reaction"}`)},
	}, "bob")
	want := []ReactionCount{
		{Target: 1, Emoji: "heart", Glyph: "❤️", Count: 1, Mine: true},
		{Target: 1, Emoji: "thumbs_up", Glyph: "👍", Count: 1, Mine: true},
		{Target: 3, Emoji: "heart", Glyph: "❤️", Count: 1, Mine: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered edge replay = %#v, want %#v", got, want)
	}
}

func comment(seq uint64, author, body string) MirrorRecord {
	return MirrorRecord{Seq: seq, Author: author, Body: json.RawMessage(body)}
}

func reaction(seq uint64, author string, target uint64, emoji string, active bool) MirrorRecord {
	body, _ := json.Marshal(map[string]any{"v": 1, "t": "reaction", "target": target, "emoji": emoji, "active": active})
	return MirrorRecord{Seq: seq, Key: fmt.Sprintf("r/%016x", seq), Author: author, Body: body}
}
