package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scimux/scimux/internal/asset"
	"github.com/scimux/scimux/internal/referencemedia"
	"github.com/scimux/scimux/internal/sessionlog"
	"github.com/scimux/scimux/internal/storagebudget"
)

var referenceAssetMarkerRE = regexp.MustCompile(`(!?)\[[^]]*\]\(scimux-asset:([A-Za-z0-9_]+)\)`)

type referenceCaptureRequest struct {
	Source struct {
		UID     string `json:"uid"`
		Segment *int   `json:"segment"`
		Record  *int   `json:"record"`
	} `json:"source"`
}

func (a *app) handleReferenceMediaCapture(w http.ResponseWriter, r *http.Request) {
	var request referenceCaptureRequest
	if err := decodeJSON(w, r, &request); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if request.Source.UID == "" || request.Source.Segment == nil || request.Source.Record == nil || *request.Source.Segment < 0 || *request.Source.Record < 0 {
		http.Error(w, "source uid, segment and record are required", http.StatusBadRequest)
		return
	}
	path, _, nodeID, _, ok := a.resolvePreview(request.Source.UID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	window, anchor, _, _, ok := sessionlog.ReadTurnWindow(path, *request.Source.Segment, *request.Source.Record, 0, 0)
	if !ok || anchor != 0 || len(window) != 1 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	turn := window[0]
	// ReadTurnWindow resolved the exact ordinal without Preview's timestamp or
	// first-turn fallback. The path itself was selected by the requested UID.
	turn.UID, turn.Segment, turn.Record = request.Source.UID, *request.Source.Segment, *request.Source.Record
	if nodeID != "" {
		projected, _ := a.projectTurns(nodeID, window)
		turn = projected[0]
	}
	captureItems := a.referenceCaptureItems
	if a.referenceCaptureItemsHook != nil {
		captureItems = a.referenceCaptureItemsHook
	}
	inputs, err := captureItems(path, nodeID, turn.Text)
	if err != nil {
		referenceMediaError(w, err)
		return
	}
	if nodeID != "" {
		currentPath, _, currentNode, _, current := a.resolvePreview(request.Source.UID)
		if !current || currentPath != path || currentNode != nodeID {
			http.Error(w, "reference media source changed during capture", http.StatusInternalServerError)
			return
		}
	}
	media, err := a.referenceMedia.Capture(referencemedia.Source{
		UID: request.Source.UID, Segment: *request.Source.Segment, Record: *request.Source.Record,
	}, turn.Text, inputs)
	if err != nil {
		referenceMediaError(w, err)
		return
	}
	writeJSON(w, map[string]any{"text": turn.Text, "media": media})
}

func (a *app) referenceCaptureItems(logPath, nodeID, text string) ([]referencemedia.CaptureItem, error) {
	markers := referenceAssetMarkerRE.FindAllStringSubmatch(text, -1)
	if len(markers) == 0 {
		return nil, nil
	}
	idx := sessionlog.ReadAssets(logPath)
	type candidate struct {
		id, name, mime string
		content        sessionlog.AssetEvent
		available      bool
	}
	candidates := make([]candidate, 0, len(markers))
	seen := make(map[string]bool)
	for _, marker := range markers {
		id := marker[2]
		if seen[id] {
			continue
		}
		seen[id] = true
		rec, ok := idx[id]
		if !ok {
			// A missing explicit image marker remains visible as unavailable. A
			// plain file link is not an image candidate without server metadata.
			if marker[1] == "!" {
				candidates = append(candidates, candidate{id: id, name: "image"})
			}
			continue
		}
		name := rec.Name
		if name == "" {
			name = "image"
		}
		mime := inlineImageTypes[strings.ToLower(filepath.Ext(name))]
		if mime == "" {
			continue
		}
		if rec.Mime != "" && rec.Mime != mime {
			return nil, referencemedia.ErrInvalid
		}
		content, ok := resolveAssetBacking(idx, rec)
		if !ok {
			return nil, referencemedia.ErrCorrupt
		}
		candidates = append(candidates, candidate{id: id, name: name, mime: mime, content: content, available: true})
	}
	if len(candidates) > a.referenceMedia.Limits.MaxItems {
		return nil, referencemedia.ErrTooLarge
	}
	items := make([]referencemedia.CaptureItem, 0, len(candidates))
	var total int64
	for _, candidate := range candidates {
		if !candidate.available {
			items = append(items, referencemedia.CaptureItem{Key: "asset:" + candidate.id, Name: candidate.name, State: referencemedia.StateUnavailable})
			continue
		}
		remaining := a.referenceMedia.Limits.MaxTotalBytes - total
		if remaining < 0 {
			return nil, referencemedia.ErrTooLarge
		}
		readLimit := a.referenceMedia.Limits.MaxImageBytes
		if remaining < readLimit {
			readLimit = remaining
		}
		var data []byte
		var err error
		switch candidate.content.Storage {
		case "inline":
			data, err = referencemedia.ReadBounded(base64.NewDecoder(base64.StdEncoding, strings.NewReader(candidate.content.Bytes)), readLimit)
		case "blob":
			if nodeID == "" {
				items = append(items, referencemedia.CaptureItem{Key: "asset:" + candidate.id, Name: candidate.name, State: referencemedia.StateUnavailable})
				continue
			}
			var full string
			full, err = asset.ResolveBlobPath(a.assetsDir, nodeID, candidate.content.BlobPath)
			if err == nil {
				var f *os.File
				f, err = os.Open(full)
				if err == nil {
					data, err = referencemedia.ReadBounded(f, readLimit)
					closeErr := f.Close()
					if err == nil {
						err = closeErr
					}
				}
			}
		default:
			err = referencemedia.ErrCorrupt
		}
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		items = append(items, referencemedia.CaptureItem{Key: "asset:" + candidate.id, Name: candidate.name, MIME: candidate.mime, State: referencemedia.StateReady, Data: data})
	}
	return items, nil
}

func (a *app) handleReferenceMediaAsset(w http.ResponseWriter, r *http.Request) {
	data, mime, err := a.referenceMedia.ReadAsset(r.PathValue("captureID"), r.PathValue("itemID"))
	if err != nil {
		if errors.Is(err, referencemedia.ErrInvalid) {
			http.Error(w, "invalid media id", http.StatusBadRequest)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "image", zeroTime, bytes.NewReader(data))
}

var zeroTime = time.Time{}

func referenceMediaError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, referencemedia.ErrTooLarge):
		http.Error(w, "reference media exceeds capture limit", http.StatusRequestEntityTooLarge)
	case errors.Is(err, referencemedia.ErrInvalid):
		http.Error(w, "invalid reference media", http.StatusBadRequest)
	case errors.Is(err, storagebudget.ErrBudget):
		http.Error(w, "storage limit reached", http.StatusInsufficientStorage)
	default:
		http.Error(w, "reference media unavailable", http.StatusInternalServerError)
	}
}
