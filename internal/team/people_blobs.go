package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
)

// ErrBlobUnsupported means the configured relay did not advertise fencing.
var ErrBlobUnsupported = errors.New("team: generation-safe blobs unsupported")
var ErrPeopleBlobCorrupt = errors.New("team: attachment bytes do not match")

// PutPeopleBlob checks server identity and fencing before sending any bytes.
// Callers additionally own configuration, permission and operation lifetime
// checks; this capability exchange is not an authentication credential.
func (c *Client) PutPeopleBlob(ctx context.Context, baseURL, token, serverID, owner, chat, generation, id string, data []byte) error {
	if !validHex(serverID, 32) || !validHex(generation, 16) || !validHex(id, 64) || len(data) == 0 || len(data) > 4<<20 {
		return ErrProtocol
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		return ErrProtocol
	}
	path, err := channelPath(owner, chat, "/blobs/"+id)
	if err != nil {
		return err
	}
	server, err := c.Hello(ctx, baseURL)
	if err != nil {
		return err
	}
	if server.ID != serverID {
		return ErrProtocol
	}
	if !server.BlobGeneration {
		return ErrBlobUnsupported
	}
	status, raw, err := c.doAuthedQuery(ctx, http.MethodPut, baseURL, path, url.Values{"generation": {generation}}.Encode(), token, data, maxResponseBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return mapChannelStatus(status)
	}
	var result struct {
		ID   string `json:"id"`
		Size int    `json:"size"`
	}
	if decodeJSON(raw, &result) != nil || result.ID != id || result.Size != len(data) {
		return ErrProtocol
	}
	return nil
}

// GetPeopleBlob returns only bytes matching the expected size and hash.
// Authorization of the comment or stage association is the caller's job.
func (c *Client) GetPeopleBlob(ctx context.Context, baseURL, token, serverID, owner, chat, generation, id string, size int64) ([]byte, error) {
	if !validHex(serverID, 32) || !validHex(generation, 16) || !validHex(id, 64) || size < 1 || size > 4<<20 {
		return nil, ErrProtocol
	}
	path, err := channelPath(owner, chat, "/blobs/"+id)
	if err != nil {
		return nil, err
	}
	server, err := c.Hello(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	if server.ID != serverID {
		return nil, ErrProtocol
	}
	if !server.BlobGeneration {
		return nil, ErrBlobUnsupported
	}
	status, raw, err := c.doAuthedQueryLimits(ctx, http.MethodGet, baseURL, path, url.Values{"generation": {generation}}.Encode(), token, nil, int(size), maxResponseBody)
	if err != nil {
		if status == http.StatusOK && errors.Is(err, errResponseTooLarge) {
			return nil, ErrPeopleBlobCorrupt
		}
		return nil, err
	}
	if status != http.StatusOK {
		return nil, mapChannelStatus(status)
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != size || hex.EncodeToString(sum[:]) != id {
		return nil, ErrPeopleBlobCorrupt
	}
	return raw, nil
}
