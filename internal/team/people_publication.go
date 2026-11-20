package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// ErrPeoplePublicationConflict means the relay's first body for a key does
// not match the frozen publication; it must never be treated as delivery.
var ErrPeoplePublicationConflict = errors.New("team: publication conflicts with existing entry")

// AppendPeopleComment verifies the exact recorded author, key, generation and
// canonical body before reporting success, including duplicate-key results.
func (c *Client) AppendPeopleComment(ctx context.Context, baseURL, token, owner, chat, generation, key, author string, body json.RawMessage) (uint64, error) {
	if !validHex(author, 32) {
		return 0, ErrProtocol
	}
	if ctx != nil && ctx.Err() != nil {
		return 0, ctx.Err()
	}
	gen, last, results, err := c.appendLog(ctx, baseURL, token, owner, chat, generation, "/comments", []MirrorEntry{{Key: key, Body: body}})
	if err != nil {
		return 0, err
	}
	if gen != generation || len(results) != 1 || results[0].Seq == 0 || results[0].Seq > uint64(last) {
		return 0, ErrProtocol
	}
	seq := results[0].Seq
	if ctx != nil && ctx.Err() != nil {
		return 0, ctx.Err()
	}
	page, err := c.ReadComments(ctx, baseURL, token, owner, chat, seq-1)
	if err != nil {
		return 0, err
	}
	if page.Generation != generation || len(page.Entries) == 0 {
		return 0, ErrPeoplePublicationConflict
	}
	entry := page.Entries[0]
	stored, storedErr := json.Marshal(entry.Body)
	expected, expectedErr := json.Marshal(body)
	if entry.Seq != seq || entry.Key != key || entry.Author != author || storedErr != nil || expectedErr != nil || !bytes.Equal(stored, expected) {
		return 0, ErrPeoplePublicationConflict
	}
	return seq, nil
}
