package team

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// ReactionChoice is one supported People reaction, in presentation order.
type ReactionChoice struct {
	ID    string `json:"id"`
	Emoji string `json:"emoji"`
}

// ReactionCount is the current aggregate for one target and emoji.
type ReactionCount struct {
	Target uint64 `json:"target"`
	Emoji  string `json:"emoji"`
	Glyph  string `json:"glyph"`
	Count  int    `json:"count"`
	Mine   bool   `json:"mine"`
}

// ReactionEvent is a validated reaction whose participant comes from the
// authenticated comments-log envelope.
type ReactionEvent struct {
	Version      int    `json:"v"`
	Type         string `json:"t"`
	Seq          uint64 `json:"seq"`
	Key          string `json:"key"`
	Target       uint64 `json:"target"`
	TargetAuthor string `json:"target_author"`
	Emoji        string `json:"emoji"`
	Active       bool   `json:"active"`
	Author       string `json:"author"`
	Mine         bool   `json:"mine"`
}

var reactionChoices = [...]ReactionChoice{
	{ID: "celebration", Emoji: "🎉"},
	{ID: "thinking", Emoji: "🤔"},
	{ID: "laughing", Emoji: "😂"},
	{ID: "heart", Emoji: "❤️"},
	{ID: "thumbs_down", Emoji: "👎"},
	{ID: "thumbs_up", Emoji: "👍"},
}

// ReactionChoices returns the fixed reaction set in its UI order.
func ReactionChoices() []ReactionChoice {
	return append([]ReactionChoice(nil), reactionChoices[:]...)
}

// ValidPeopleCommentRecord accepts a valid text or file-only People comment.
func ValidPeopleCommentRecord(record MirrorRecord) bool {
	return record.Seq > 0 && record.Author != "" && validReactionTarget(record.Body)
}

// ParseReactionEvent independently validates one reaction against its exact
// earlier target comment. Actor identity is taken only from record.Author.
func ParseReactionEvent(record, target MirrorRecord) (ReactionEvent, bool) {
	if record.Seq == 0 || !validReactionKey(record.Key) || record.Author == "" || target.Seq == 0 || target.Seq >= record.Seq || !ValidPeopleCommentRecord(target) {
		return ReactionEvent{}, false
	}
	var body reactionBody
	dec := json.NewDecoder(bytes.NewReader(record.Body))
	dec.DisallowUnknownFields()
	if dec.Decode(&body) != nil || body.Version == nil || *body.Version != 1 || body.Type == nil || *body.Type != "reaction" || body.Target == nil || *body.Target != target.Seq || body.Emoji == nil || body.Active == nil {
		return ReactionEvent{}, false
	}
	var extra json.RawMessage
	if dec.Decode(&extra) != io.EOF || !knownReaction(*body.Emoji) || target.Author == record.Author {
		return ReactionEvent{}, false
	}
	return ReactionEvent{Version: 1, Type: "reaction", Seq: record.Seq, Key: record.Key, Target: target.Seq, TargetAuthor: target.Author, Emoji: *body.Emoji, Active: *body.Active, Author: record.Author}, true
}

func validReactionKey(key string) bool {
	if len(key) != 18 || !strings.HasPrefix(key, "r/") {
		return false
	}
	for _, char := range key[2:] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

type reactionKey struct {
	target             uint64
	participant, emoji string
}

type reactionValue struct {
	active bool
	seq    uint64
}

type reactionBody struct {
	Version *int    `json:"v"`
	Type    *string `json:"t"`
	Target  *uint64 `json:"target"`
	Emoji   *string `json:"emoji"`
	Active  *bool   `json:"active"`
}

type peopleCommentBody struct {
	Version     *int              `json:"v"`
	Type        *string           `json:"t"`
	Text        *string           `json:"text"`
	Anchor      *uint64           `json:"anchor"`
	Attachments []json.RawMessage `json:"attachments"`
}

// ReplayReactions validates records in sequence order and returns the latest
// valid state for each target, authenticated participant, and reaction.
func ReplayReactions(records []MirrorRecord, viewer string) []ReactionCount {
	comments := make(map[uint64]string)
	commentBodies := make(map[uint64]json.RawMessage)
	state := make(map[reactionKey]reactionValue)
	var previous uint64
	for _, record := range records {
		if record.Seq == 0 || record.Seq <= previous || record.Author == "" {
			continue
		}
		previous = record.Seq
		var kind struct {
			Type *string `json:"t"`
		}
		if json.Unmarshal(record.Body, &kind) != nil || kind.Type == nil {
			continue
		}
		if *kind.Type == "comment" {
			if validReactionTarget(record.Body) {
				comments[record.Seq] = record.Author
				commentBodies[record.Seq] = record.Body
			}
			continue
		}
		if *kind.Type != "reaction" {
			continue
		}
		var body reactionBody
		if json.Unmarshal(record.Body, &body) != nil || body.Target == nil {
			continue
		}
		target := *body.Target
		targetAuthor, exists := comments[target]
		if !exists {
			continue
		}
		targetRecord := MirrorRecord{Seq: target, Author: targetAuthor, Body: commentBodies[target]}
		event, valid := ParseReactionEvent(record, targetRecord)
		if !valid {
			continue
		}
		key := reactionKey{target: event.Target, participant: event.Author, emoji: event.Emoji}
		state[key] = reactionValue{active: event.Active, seq: record.Seq}
	}

	participants := make(map[countKey]map[string]bool)
	for key, value := range state {
		if !value.active {
			continue
		}
		count := countKey{target: key.target, emoji: key.emoji}
		if participants[count] == nil {
			participants[count] = make(map[string]bool)
		}
		participants[count][key.participant] = true
	}
	out := make([]ReactionCount, 0, len(participants))
	order := make(map[string]int, len(reactionChoices))
	glyphs := make(map[string]string, len(reactionChoices))
	for index, choice := range reactionChoices {
		order[choice.ID], glyphs[choice.ID] = index, choice.Emoji
	}
	for key, people := range participants {
		out = append(out, ReactionCount{Target: key.target, Emoji: key.emoji, Glyph: glyphs[key.emoji], Count: len(people), Mine: people[viewer]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return order[out[i].Emoji] < order[out[j].Emoji]
	})
	return out
}

type countKey struct {
	target uint64
	emoji  string
}

// ReplayReactionPages replays a bounded set of already-paged comments while
// preserving sequence context across page boundaries.
func ReplayReactionPages(pages [][]MirrorRecord, viewer string) []ReactionCount {
	var all []MirrorRecord
	for _, page := range pages {
		all = append(all, page...)
	}
	return ReplayReactions(all, viewer)
}

func validReactionTarget(raw []byte) bool {
	var body peopleCommentBody
	if json.Unmarshal(raw, &body) != nil || body.Version == nil || *body.Version != 1 || body.Type == nil || *body.Type != "comment" || body.Text == nil || body.Anchor == nil {
		return false
	}
	return *body.Text != "" || len(body.Attachments) > 0
}

func knownReaction(id string) bool {
	for _, choice := range reactionChoices {
		if choice.ID == id {
			return true
		}
	}
	return false
}
