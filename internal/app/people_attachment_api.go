package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	"github.com/scimux/scimux/internal/identity"
	"github.com/scimux/scimux/internal/storagebudget"
	"github.com/scimux/scimux/internal/team"
)

type peopleCopyOperationKey struct{}

type peopleCopyOperation struct {
	state          *peopleCopyRemovalEpoch
	epoch          uint64
	stageID        string
	stageEpoch     uint64
	cancel         context.CancelFunc
	copyID         team.CopyID
	dataDir        string
	instance       string
	publicationKey string
}

// beginPeopleCopyOperation captures a local copy incarnation before any body
// or relay I/O. The state exists only while requests for that copy are active.
func (h *teamHandler) beginPeopleCopyOperation(r *http.Request, id team.CopyID, stageID string) (*http.Request, func()) {
	h.mu.Lock()
	if h.copyRemovalEpoch == nil {
		h.copyRemovalEpoch = make(map[team.CopyID]*peopleCopyRemovalEpoch)
	}
	state := h.copyRemovalEpoch[id]
	if state == nil {
		state = &peopleCopyRemovalEpoch{}
		h.copyRemovalEpoch[id] = state
	}
	state.active++
	ctx, cancel := context.WithCancel(r.Context())
	op := &peopleCopyOperation{state: state, epoch: state.epoch, stageID: stageID, stageEpoch: state.stages[stageID], cancel: cancel, copyID: id, dataDir: h.dataDir}
	if state.operations == nil {
		state.operations = make(map[*peopleCopyOperation]struct{})
	}
	state.operations[op] = struct{}{}
	h.mu.Unlock()
	return r.WithContext(context.WithValue(ctx, peopleCopyOperationKey{}, op)), func() {
		h.mu.Lock()
		delete(state.operations, op)
		state.active--
		if state.active == 0 {
			delete(h.copyRemovalEpoch, id)
		}
		h.mu.Unlock()
		cancel()
	}
}

// peopleCopyCurrentLocked requires h.mu and is checked at local commit points.
func peopleCopyCurrentLocked(r *http.Request) bool {
	return peopleCopyContextCurrentLocked(r.Context())
}

func peopleCopyContextCurrentLocked(ctx context.Context) bool {
	op, ok := ctx.Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if !ok || ctx.Err() != nil || op.state.epoch != op.epoch || (op.stageID != "" && op.state.stages[op.stageID] != op.stageEpoch) {
		return false
	}
	if op.instance != "" {
		instance, err := team.ReadPeopleCopyInstance(op.dataDir, op.copyID)
		return err == nil && instance == op.instance
	}
	return true
}

// requirePeopleCopy binds file work to the local copy selected by the client.
// The public instance is a concurrency precondition, not an authorization.
func (h *teamHandler) requirePeopleCopy(w http.ResponseWriter, r *http.Request, publicationKey ...string) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	values := query["copy_instance"]
	if err != nil || len(values) != 1 || !subHex(values[0], 32) {
		writeTeamError(w, http.StatusConflict, "copy_instance_required")
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	op, ok := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if !ok || !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return false
	}
	instance, err := team.ReadPeopleCopyInstance(h.dataDir, op.copyID)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "copy_identity_unavailable")
		return false
	}
	if instance != values[0] {
		writeTeamError(w, http.StatusConflict, "copy_changed")
		return false
	}
	op.instance = instance
	if len(publicationKey) != 0 {
		op.publicationKey = publicationKey[0]
	}
	return true
}

func (h *teamHandler) peopleCopyCurrent(r *http.Request) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return peopleCopyCurrentLocked(r)
}

func (h *teamHandler) readStage(id team.CopyID, stageID string) (team.PeopleStage, error) {
	if h.stageRead != nil {
		return h.stageRead(h.dataDir, id, stageID)
	}
	return team.ReadPeopleStage(h.dataDir, id, stageID)
}

func (h *teamHandler) saveStage(id team.CopyID, stage team.PeopleStage) error {
	if h.stageSave != nil {
		return h.stageSave(h.dataDir, id, stage)
	}
	return team.SavePeopleStage(h.dataDir, id, stage)
}

// stagePeopleAttachment validates access and relay capability before consuming
// the body, then rechecks the binding before committing staged bytes.
func (h *teamHandler) stagePeopleAttachment(w http.ResponseWriter, r *http.Request, id team.CopyID) {
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if sub.Access == "read" {
		writeTeamError(w, http.StatusForbidden, "read_only")
		return
	}
	if sub.State != "live" || !joined {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !h.requirePeopleCopy(w, r) {
		return
	}
	instance, ok := h.peopleWriteInstance(r, sub)
	if !ok || !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return
	}
	hello, err := h.client.Hello(r.Context(), server.URL)
	if err != nil {
		writeTeamError(w, http.StatusConflict, "relay_unavailable")
		return
	}
	if hello.ID != server.ID {
		writeTeamError(w, http.StatusConflict, "server_changed")
		return
	}
	if !hello.BlobGeneration {
		writeTeamError(w, http.StatusConflict, "attachments_unsupported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, (4<<20)+(64<<10))
	reader, err := r.MultipartReader()
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "files" || !peopleSafeName(part.FileName()) {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	data, err := io.ReadAll(io.LimitReader(part, (4<<20)+1))
	if err != nil {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	if len(data) == 0 {
		writeTeamError(w, http.StatusBadRequest, "empty_file")
		return
	}
	if len(data) > 4<<20 {
		writeTeamError(w, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	if _, err := reader.NextPart(); err != io.EOF {
		writeTeamError(w, http.StatusBadRequest, "bad_request")
		return
	}
	current := func() bool {
		next, configured, found, joined, err := h.lookupSubscription(id)
		return err == nil && found && joined && next.State == "live" && next.Access == sub.Access && h.peopleWriteBound(next, instance) && h.peopleFilesEnabled() && peopleSameServer(configured, server) && h.peopleCopyCurrent(r)
	}
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	values := query["stage_id"]
	stageID := ""
	if len(values) != 0 {
		if len(values) != 1 || !subHex(values[0], 16) {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		stageID = values[0]
	} else {
		var random [8]byte
		_, _ = rand.Read(random[:]) // Go 1.26 fills the buffer or terminates the process.
		stageID = hex.EncodeToString(random[:])
	}
	digest := sha256.Sum256(data)
	stage := team.PeopleStage{ID: stageID, Hash: hex.EncodeToString(digest[:]), Name: part.FileName(), MIME: http.DetectContentType(data), ConfigHash: peopleServerFingerprint(server), OwnerInstance: instance, Data: data}
	matches := func() (bool, bool) {
		prior, err := h.readStage(id, stage.ID)
		if err != nil {
			return false, false
		}
		return true, prior.Hash == stage.Hash && prior.Name == stage.Name && prior.MIME == stage.MIME && prior.ConfigHash == stage.ConfigHash && prior.OwnerInstance == stage.OwnerInstance && bytes.Equal(prior.Data, stage.Data)
	}
	h.mu.Lock()
	if !peopleCopyCurrentLocked(r) || !h.peopleWriteCurrentLocked(id, sub, server, instance) {
		h.mu.Unlock()
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if exists, same := matches(); exists {
		if !same {
			h.mu.Unlock()
			writeTeamError(w, http.StatusConflict, "stage_conflict")
			return
		}
	} else if err := h.saveStage(id, stage); err != nil {
		if exists, same := matches(); !exists || !same {
			h.mu.Unlock()
			if errors.Is(err, storagebudget.ErrBudget) {
				writeTeamError(w, http.StatusInsufficientStorage, "storage_full")
			} else {
				writeTeamError(w, http.StatusConflict, "stage_conflict")
			}
			return
		}
	}
	h.mu.Unlock()
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if err := h.client.PutPeopleBlob(r.Context(), server.URL, server.Token, server.ID, id.Owner, id.Chat, id.Generation, stage.Hash, data); err != nil {
		status, code := peopleBlobFailure(err)
		writeJSONStatus(w, status, map[string]any{"error": code, "stage_id": stage.ID})
		return
	}
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"attachments": []map[string]any{{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME, "revision": team.PeopleStageRevision(stage)}}})
}

func peopleServerFingerprint(server team.Entry) string {
	sum := sha256.Sum256([]byte(server.ID + "\x00" + server.URL + "\x00" + server.Token))
	return hex.EncodeToString(sum[:])
}

func peopleSameServer(a, b team.Entry) bool {
	return a.ID == b.ID && a.URL == b.URL && a.Token == b.Token
}

func (h *teamHandler) authorizedPeopleStage(w http.ResponseWriter, r *http.Request, id team.CopyID, stageID string) (team.PeopleStage, team.Subscription, team.Entry, bool) {
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	if !joined || sub.State != "live" || !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	instance, ok := h.peopleWriteInstance(r, sub)
	if !ok {
		writeTeamError(w, http.StatusForbidden, "read_only")
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	if !h.requirePeopleCopy(w, r) {
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	stage, err := h.readStage(id, stageID)
	if err != nil || stage.ConfigHash != peopleServerFingerprint(server) || stage.OwnerInstance != instance {
		writeTeamError(w, http.StatusNotFound, "stage_unavailable")
		return team.PeopleStage{}, team.Subscription{}, team.Entry{}, false
	}
	return stage, sub, server, true
}

func (h *teamHandler) readPeopleStage(w http.ResponseWriter, r *http.Request, id team.CopyID, stageID string) {
	stage, _, _, ok := h.authorizedPeopleStage(w, r, id, stageID)
	if !ok {
		return
	}
	if !h.peopleCopyCurrent(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"id": stage.ID, "sha256": stage.Hash, "size": len(stage.Data), "name": stage.Name, "mime": stage.MIME, "revision": team.PeopleStageRevision(stage)})
}

func (h *teamHandler) removePeopleStage(w http.ResponseWriter, r *http.Request, id team.CopyID, stageID string) {
	if !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["revision"]) != 1 || !subHex(query.Get("revision"), 64) {
		writeTeamError(w, http.StatusConflict, "revision_required")
		return
	}
	if !h.requirePeopleCopy(w, r) {
		return
	}
	if _, err := identity.LoadInstallation(h.dataDir); err != nil {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.peopleFilesEnabled() || !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	op := r.Context().Value(peopleCopyOperationKey{}).(*peopleCopyOperation)
	if err := team.DeletePeopleStageExact(h.dataDir, id, stageID, op.instance, query.Get("revision")); err != nil {
		writeTeamError(w, http.StatusConflict, "stage_conflict")
		return
	}
	if op.state.stages == nil {
		op.state.stages = make(map[string]uint64)
	}
	op.state.stages[stageID]++
	for other := range op.state.operations {
		if other != op && other.stageID == stageID {
			other.cancel()
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *teamHandler) peopleFilesEnabled() bool {
	return h.peopleEnabled == nil || h.peopleEnabled()
}

func (h *teamHandler) peopleWriteInstance(r *http.Request, sub team.Subscription) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", false
	}
	if sub.Access == "write" {
		return "", true
	}
	if sub.Access != "owner" {
		return "", false
	}
	values := query["chat_instance"]
	if len(values) != 1 || !team.ValidPeopleOwnerInstance(values[0]) || !h.peopleWriteBound(sub, values[0]) {
		return "", false
	}
	return values[0], true
}

func (h *teamHandler) peopleWriteBound(sub team.Subscription, instance string) bool {
	if sub.Access == "write" {
		return instance == ""
	}
	if sub.Access != "owner" || !team.ValidPeopleOwnerInstance(instance) || h.ownerInstance == nil {
		return false
	}
	node := h.localOwnerNode(sub)
	return node != "" && h.ownerInstance(node, instance)
}

// peopleWriteCurrentLocked requires h.mu, serializing the final admission and local write with
// subscription removal and configuration updates. No relay I/O occurs here.
func (h *teamHandler) peopleWriteCurrentLocked(id team.CopyID, original team.Subscription, server team.Entry, instance string) bool {
	entries, subs, err := h.loadSubState()
	if err != nil || !h.peopleFilesEnabled() {
		return false
	}
	for _, sub := range subs {
		if !sameCopy(sub, id) || sub.State != "live" || sub.Access != original.Access || !h.peopleWriteBound(sub, instance) {
			continue
		}
		for _, entry := range entries {
			if peopleSameServer(entry, server) {
				return true
			}
		}
	}
	return false
}

// readPeopleAttachment serves a retained association or fetches one from a
// validated local comment while current read permission still exists.
func (h *teamHandler) readPeopleAttachment(w http.ResponseWriter, r *http.Request, id team.CopyID, seq uint64, attachmentID string) {
	if !h.peopleFilesEnabled() || !h.peopleCopyCurrent(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	sub, server, found, joined, err := h.lookupSubscription(id)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "store_unavailable")
		return
	}
	if !found {
		writeTeamError(w, http.StatusNotFound, "not_found")
		return
	}
	if !h.requirePeopleCopy(w, r) {
		return
	}
	kept := true
	stage, err := team.ReadPeoplePublished(h.dataDir, id, seq, attachmentID)
	if errors.Is(err, team.ErrPeopleStageMissing) {
		if !joined || sub.State != "live" || (sub.Access != "read" && sub.Access != "write" && sub.Access != "owner") {
			writeTeamError(w, http.StatusNotFound, "attachment_unavailable")
			return
		}
		stage, kept, err = h.fetchPeopleAttachment(r, id, seq, attachmentID, server)
		if err != nil {
			if errors.Is(err, team.ErrPeopleBlobCorrupt) {
				writeTeamError(w, http.StatusBadGateway, "attachment_corrupt")
				return
			}
			if errors.Is(err, team.ErrBlobUnsupported) {
				writeTeamError(w, http.StatusConflict, "attachments_unsupported")
				return
			}
			writeTeamError(w, http.StatusNotFound, "attachment_unavailable")
			return
		}
	}
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "attachment_corrupt")
		return
	}
	if !h.peopleFilesEnabled() || !h.peopleCopyCurrent(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if !kept {
		w.Header().Set("X-Scimux-People-Kept", "false")
	}
	if r.URL.Query().Get("preview") == "1" {
		media, ok := peopleRasterPreview(stage.Data)
		if !ok {
			writeTeamError(w, http.StatusUnsupportedMediaType, "preview_unsupported")
			return
		}
		w.Header().Set("Content-Type", media)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(stage.Data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stage.Data)
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": stage.Name})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(stage.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(stage.Data)
}

func peopleRasterPreview(data []byte) (string, bool) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 16<<20 {
		return "", false
	}
	decoded, actual, err := image.Decode(bytes.NewReader(data))
	if err != nil || actual != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return "", false
	}
	if format == "png" {
		return "image/png", true
	}
	return "image/jpeg", true
}

// fetchPeopleAttachment admits remote bytes only through a descriptor in the
// retained comment. The relay call happens outside all local storage locks.
func (h *teamHandler) fetchPeopleAttachment(r *http.Request, id team.CopyID, seq uint64, attachmentID string, server team.Entry) (team.PeopleStage, bool, error) {
	records, _, err := team.ReadCopy(h.dataDir, id, team.CommentsLog, seq-1, 1, 4<<20)
	if err != nil || len(records) != 1 || records[0].Seq != seq {
		return team.PeopleStage{}, false, team.ErrPeopleStageMissing
	}
	entry, _, ok := projectCommentRecord(records[0], "")
	if !ok {
		return team.PeopleStage{}, false, team.ErrPeopleStage
	}
	files, ok := entry["attachments"].([]map[string]any)
	if !ok {
		return team.PeopleStage{}, false, team.ErrPeopleStageMissing
	}
	var descriptor map[string]any
	for _, file := range files {
		if file["id"] == attachmentID {
			descriptor = file
			break
		}
	}
	if descriptor == nil {
		return team.PeopleStage{}, false, team.ErrPeopleStageMissing
	}
	// The fresh validated projector constructs these fields from typed descriptors.
	hash := descriptor["sha256"].(string)
	size := descriptor["size"].(int64)
	name := descriptor["name"].(string)
	media := descriptor["mime"].(string)
	data, err := h.client.GetPeopleBlob(r.Context(), server.URL, server.Token, server.ID, id.Owner, id.Chat, id.Generation, hash, size)
	if err != nil {
		return team.PeopleStage{}, false, err
	}
	stage := team.PeopleStage{ID: attachmentID, Hash: hash, Name: name, MIME: media, ConfigHash: peopleServerFingerprint(server), Data: data}
	// Serialize the final permission/configuration check with local membership
	// mutations; no relay I/O is performed while this lock is held.
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, subs, err := h.loadSubState()
	if err != nil {
		return team.PeopleStage{}, false, err
	}
	allowed := false
	for _, sub := range subs {
		if sameCopy(sub, id) && sub.State == "live" && (sub.Access == "read" || sub.Access == "write" || sub.Access == "owner") {
			for _, configured := range entries {
				if peopleSameServer(configured, server) {
					allowed = true
				}
			}
		}
	}
	if !allowed || !h.peopleFilesEnabled() || !peopleCopyCurrentLocked(r) {
		return team.PeopleStage{}, false, team.ErrPeopleStageMissing
	}
	if err := team.SavePeoplePublished(h.dataDir, id, seq, stage); err != nil {
		if errors.Is(err, storagebudget.ErrBudget) {
			return stage, false, nil
		}
		return team.PeopleStage{}, false, err
	}
	return stage, true, nil
}

// postPeopleFileComment freezes one canonical manifest before publication.
// A budget refusal of the kept copy still confirms when the comment log
// matches the pending body and the staged bytes match their hash.
// Pending recovery remains incomplete until the browser integration is wired.
func (h *teamHandler) postPeopleFileComment(w http.ResponseWriter, r *http.Request, id team.CopyID, sub team.Subscription, server team.Entry, text string, anchor uint64, key string, stageIDs []string) {
	instance, ok := h.peopleWriteInstance(r, sub)
	if !ok || !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return
	}
	current := func() bool {
		next, configured, found, joined, err := h.lookupSubscription(id)
		return err == nil && found && joined && next.State == "live" && next.Access == sub.Access && h.peopleWriteBound(next, instance) && h.peopleFilesEnabled() && peopleSameServer(configured, server) && h.peopleCopyCurrent(r)
	}
	hello, err := h.client.Hello(r.Context(), server.URL)
	if err != nil || hello.ID != server.ID || !hello.BlobGeneration || !current() {
		writeTeamError(w, http.StatusConflict, "attachments_unavailable")
		return
	}
	config := peopleServerFingerprint(server)
	seen := make(map[string]bool, len(stageIDs))
	stages := make([]team.PeopleStage, 0, len(stageIDs))
	descriptors := make([]peopleAttachmentDescriptor, 0, len(stageIDs))
	var total int64
	for _, stageID := range stageIDs {
		if !subHex(stageID, 16) || seen[stageID] {
			writeTeamError(w, http.StatusBadRequest, "bad_request")
			return
		}
		seen[stageID] = true
		stage, err := h.readStage(id, stageID)
		if err != nil || stage.ConfigHash != config || stage.OwnerInstance != instance {
			writeTeamError(w, http.StatusConflict, "stage_unavailable")
			return
		}
		total += int64(len(stage.Data))
		if total > 16<<20 {
			writeTeamError(w, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		stages = append(stages, stage)
		descriptors = append(descriptors, peopleAttachmentDescriptor{ID: stage.ID, SHA256: stage.Hash, Size: int64(len(stage.Data)), Name: stage.Name, MIME: stage.MIME})
	}
	fallback := peopleAttachmentNotice
	if text != "" {
		fallback = text + "\n" + fallback
	}
	body := struct {
		Version            int                          `json:"v"`
		Type               string                       `json:"t"`
		Text               string                       `json:"text"`
		Anchor             uint64                       `json:"anchor"`
		ContentText        string                       `json:"content_text"`
		AttachmentsVersion int                          `json:"people_attachments_version"`
		Attachments        []peopleAttachmentDescriptor `json:"people_attachments"`
	}{1, "comment", fallback, anchor, text, 1, descriptors}
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body) // Fixed scalar fields and bytes.Buffer cannot fail encoding.
	author, err := identity.LoadInstallation(h.dataDir)
	if err != nil {
		writeTeamError(w, http.StatusInternalServerError, "identity_unavailable")
		return
	}
	pending := team.PeoplePending{Key: key, Author: string(author), ConfigHash: config, OwnerInstance: instance, Body: bytes.TrimSpace(encoded.Bytes()), StageIDs: append([]string(nil), stageIDs...)}
	h.mu.Lock()
	if !peopleCopyCurrentLocked(r) || !h.peopleWriteCurrentLocked(id, sub, server, instance) {
		h.mu.Unlock()
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	err = team.SavePeoplePending(h.dataDir, id, pending, stages)
	h.mu.Unlock()
	if err != nil {
		if errors.Is(err, storagebudget.ErrBudget) {
			writeTeamError(w, http.StatusInsufficientStorage, "storage_full")
		} else {
			writeTeamError(w, http.StatusConflict, "publication_conflict")
		}
		return
	}
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	// A staged upload may have failed or its response may have been lost.
	// Recheck each immutable payload with the guarded, idempotent blob PUT
	// before publishing a comment that claims the attachment exists.
	for _, stage := range stages {
		if err := h.client.PutPeopleBlob(r.Context(), server.URL, server.Token, server.ID, id.Owner, id.Chat, id.Generation, stage.Hash, stage.Data); err != nil {
			status, code := peopleBlobFailure(err)
			writeTeamError(w, status, code)
			return
		}
		if !current() {
			writeTeamError(w, http.StatusConflict, "not_active")
			return
		}
	}
	seq, err := h.client.AppendPeopleComment(r.Context(), server.URL, server.Token, id.Owner, id.Chat, id.Generation, "c/"+key, string(author), pending.Body)
	if err != nil {
		writeTeamError(w, http.StatusBadGateway, "publication_unconfirmed")
		return
	}
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	if err := h.retainPeopleComment(r, id, sub, server, instance, seq); err != nil {
		writeTeamError(w, http.StatusBadGateway, "publication_unconfirmed")
		return
	}
	h.mu.Lock()
	if !peopleCopyCurrentLocked(r) || !h.peopleWriteCurrentLocked(id, sub, server, instance) {
		h.mu.Unlock()
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	unkept := false
	for _, stage := range stages {
		if err := team.SavePeoplePublished(h.dataDir, id, seq, stage); err != nil {
			if !errors.Is(err, storagebudget.ErrBudget) {
				h.mu.Unlock()
				writeTeamError(w, http.StatusInsufficientStorage, "storage_full")
				return
			}
			unkept = true
		}
	}
	err = team.ConfirmPeoplePending(h.dataDir, id, key, seq, unkept)
	h.mu.Unlock()
	if err != nil {
		writeTeamError(w, http.StatusBadGateway, "publication_unconfirmed")
		return
	}
	if !current() {
		writeTeamError(w, http.StatusConflict, "not_active")
		return
	}
	writeJSONStatus(w, http.StatusOK, struct {
		Seq uint64 `json:"seq"`
	}{seq})
}

// peopleBlobFailure reports known relay refusals without disclosing transport details.
func peopleBlobFailure(err error) (int, string) {
	switch {
	case errors.Is(err, team.ErrQuota):
		return http.StatusInsufficientStorage, "relay_quota"
	case errors.Is(err, team.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "file_too_large"
	case errors.Is(err, team.ErrUnauthorized):
		return http.StatusUnauthorized, "relay_unauthorized"
	case errors.Is(err, team.ErrForbidden):
		return http.StatusForbidden, "relay_forbidden"
	case errors.Is(err, team.ErrStaleGeneration):
		return http.StatusConflict, "stale_generation"
	case errors.Is(err, team.ErrNoChannel):
		return http.StatusNotFound, "channel_unavailable"
	case errors.Is(err, team.ErrBlobUnsupported):
		return http.StatusConflict, "attachments_unsupported"
	default:
		return http.StatusBadGateway, "upload_unconfirmed"
	}
}

// tryPeopleCompletion reconciles only a previously verified local receipt.
// It performs no relay or agent operation, including after sharing has ended.
func (h *teamHandler) tryPeopleCompletion(w http.ResponseWriter, r *http.Request, id team.CopyID, original team.Subscription, server team.Entry, text string, anchor uint64, key string, stageIDs []string) bool {
	instance, ok := "", original.Access == "read"
	if !ok {
		instance, ok = h.peopleWriteInstance(r, original)
	}
	if !ok || !h.peopleFilesEnabled() {
		writeTeamError(w, http.StatusConflict, "identity_unavailable")
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !peopleCopyCurrentLocked(r) {
		writeTeamError(w, http.StatusConflict, "not_active")
		return true
	}
	completed, err := team.ReadPeopleCompletion(h.dataDir, id, key)
	if errors.Is(err, team.ErrPeopleStageMissing) {
		return false
	}
	if err != nil {
		writeTeamError(w, http.StatusConflict, "publication_conflict")
		return true
	}
	entries, subs, err := h.loadSubState()
	current := false
	if err == nil {
		for _, sub := range subs {
			bound := sub.Access == "read" && instance == "" || h.peopleWriteBound(sub, instance)
			if !sameCopy(sub, id) || sub.Access != original.Access || !bound {
				continue
			}
			for _, entry := range entries {
				if peopleSameServer(entry, server) {
					current = true
				}
			}
		}
	}
	author, authorErr := identity.LoadInstallation(h.dataDir)
	if !current || !h.peopleFilesEnabled() || authorErr != nil || completed.Pending.Author != string(author) || completed.Pending.ConfigHash != peopleServerFingerprint(server) || completed.Pending.OwnerInstance != instance || !peopleCompletionMatches(completed.Pending, text, anchor, stageIDs) {
		writeTeamError(w, http.StatusConflict, "publication_conflict")
		return true
	}
	if !h.peopleCompletionRetained(id, completed) {
		writeTeamError(w, http.StatusConflict, "publication_unavailable")
		return true
	}
	writeJSONStatus(w, http.StatusOK, struct {
		Seq uint64 `json:"seq"`
	}{completed.Seq})
	return true
}

func (h *teamHandler) peopleCompletionRetained(id team.CopyID, completed team.PeopleCompletion) bool {
	records, _, err := team.ReadCopy(h.dataDir, id, team.CommentsLog, completed.Seq-1, 1, 4<<20)
	if err != nil || len(records) != 1 || records[0].Seq != completed.Seq || records[0].Key != "c/"+completed.Pending.Key || records[0].Author != completed.Pending.Author {
		return false
	}
	want, wantErr := json.Marshal(completed.Pending.Body)
	got, gotErr := json.Marshal(records[0].Body)
	if wantErr != nil || gotErr != nil || !bytes.Equal(want, got) {
		return false
	}
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(completed.Pending.Body, &body) != nil {
		return false
	}
	projected := map[string]any{}
	projectPeopleAttachments(completed.Pending.Body, body.Text, projected)
	files, ok := projected["attachments"].([]map[string]any)
	if !ok || len(files) != len(completed.Pending.StageIDs) {
		return false
	}
	for _, file := range files {
		attachmentID := file["id"].(string) // The fresh validated projector supplies a string.
		stage, err := team.ReadPeoplePublished(h.dataDir, id, completed.Seq, attachmentID)
		if err != nil || stage.Hash != file["sha256"] || int64(len(stage.Data)) != file["size"] || stage.Name != file["name"] || stage.MIME != file["mime"] {
			return false
		}
	}
	return true
}

func peopleCompletionMatches(pending team.PeoplePending, text string, anchor uint64, ids []string) bool {
	var body struct {
		Version     int    `json:"v"`
		Type        string `json:"t"`
		Text        string `json:"text"`
		Anchor      uint64 `json:"anchor"`
		ContentText string `json:"content_text"`
	}
	if json.Unmarshal(pending.Body, &body) != nil || body.Version != 1 || body.Type != "comment" || body.Anchor != anchor || body.ContentText != text || len(ids) != len(pending.StageIDs) {
		return false
	}
	for i, id := range ids {
		if id != pending.StageIDs[i] {
			return false
		}
	}
	projected := map[string]any{}
	projectPeopleAttachments(pending.Body, body.Text, projected)
	files, ok := projected["attachments"].([]map[string]any)
	if !ok || len(files) != len(ids) {
		return false
	}
	for i, id := range ids {
		if files[i]["id"] != id {
			return false
		}
	}
	return true
}

// retainPeopleComment catches up the append-only copy before a pending pin can
// be released. The network is outside h.mu; each local append is admitted
// again under the same lock as subscription removal.
func (h *teamHandler) retainPeopleComment(r *http.Request, id team.CopyID, sub team.Subscription, server team.Entry, instance string, seq uint64) error {
	for {
		h.mu.Lock()
		if !peopleCopyCurrentLocked(r) || !h.peopleWriteCurrentLocked(id, sub, server, instance) {
			h.mu.Unlock()
			return team.ErrPeoplePublicationConflict
		}
		_, last, err := team.ReadCopy(h.dataDir, id, team.CommentsLog, ^uint64(0), 1, 4<<20)
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if last >= seq {
			return nil
		}
		page, err := h.client.ReadComments(r.Context(), server.URL, server.Token, id.Owner, id.Chat, last)
		if err != nil {
			return err
		}
		// ReadComments rejects entries that do not advance beyond last.
		if page.Generation != id.Generation || len(page.Entries) == 0 {
			return team.ErrPeoplePublicationConflict
		}
		h.mu.Lock()
		if !peopleCopyCurrentLocked(r) || !h.peopleWriteCurrentLocked(id, sub, server, instance) {
			h.mu.Unlock()
			return team.ErrPeoplePublicationConflict
		}
		err = team.AppendCopy(h.dataDir, id, team.CommentsLog, page.Entries)
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if page.Entries[len(page.Entries)-1].Seq >= seq {
			return nil
		}
	}
}
