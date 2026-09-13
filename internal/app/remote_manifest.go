package app

// S8 (remote bootstrap) — the computer half of FR-40.
//
// web/js/bootstrap.js consumes a manifest of
// {source, entry, entries:[{url,kind,size,integrity}]}. This file is the
// producer. Entries are derived from the embedded FS the handlers actually
// serve (the same rule servedAssetInventory encodes), never from a
// hand-maintained list. Integrity is sha256- + standard-base64 of the
// served bytes, computed once per process: the embed is immutable for the
// life of the binary, and a disk cache would verify a rebuild against the
// previous binary's hashes.

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const (
	remoteBootstrapSource    = "computer"
	remoteBootstrapEntryPath = "/js/app.js"
)

type remoteBootstrapManifest struct {
	Source  string                 `json:"source"`
	Entry   string                 `json:"entry"`
	Entries []remoteBootstrapEntry `json:"entries"`
}

type remoteBootstrapEntry struct {
	URL       string `json:"url"`
	Kind      string `json:"kind"`
	Size      int    `json:"size"`
	Integrity string `json:"integrity"`
}

var (
	remoteBootstrapOnce sync.Once
	remoteBootstrapVal  remoteBootstrapManifest
	remoteBootstrapErr  error
)

func computerBootstrapManifest() (remoteBootstrapManifest, error) {
	remoteBootstrapOnce.Do(func() {
		remoteBootstrapVal, remoteBootstrapErr = buildRemoteBootstrapManifest()
	})
	return remoteBootstrapVal, remoteBootstrapErr
}

func (a *app) handleRemoteBootstrapManifest(w http.ResponseWriter, r *http.Request) {
	serveRemoteBootstrapManifest(w, r)
}

func serveRemoteBootstrapManifest(w http.ResponseWriter, r *http.Request) {
	m, err := computerBootstrapManifest()
	if err != nil {
		http.Error(w, "bootstrap manifest unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

func buildRemoteBootstrapManifest() (remoteBootstrapManifest, error) {
	entries, err := deriveRemoteBootstrapEntries()
	if err != nil {
		return remoteBootstrapManifest{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].URL < entries[j].URL })
	return remoteBootstrapManifest{
		Source:  remoteBootstrapSource,
		Entry:   remoteBootstrapEntryPath,
		Entries: entries,
	}, nil
}

// deriveRemoteBootstrapEntries mirrors newWebHandlers: index at "/", flat
// *.js under /js/, flat *.css under /css/, recursive tree under /assets/.
// Index bytes are the CSRF-substituted page GET / actually serves, so the
// digest matches the channel fetch bootstrap.js will verify.
func deriveRemoteBootstrapEntries() ([]remoteBootstrapEntry, error) {
	indexBytes, err := csrfIndex(webFS)
	if err != nil {
		return nil, err
	}
	out := []remoteBootstrapEntry{remoteBootstrapEntryFor("/", "index", indexBytes)}

	for _, r := range []struct{ dir, prefix, ext, kind string }{
		{"web/js", "/js/", ".js", "js"},
		{"web/css", "/css/", ".css", "css"},
	} {
		entries, err := fs.ReadDir(webFS, r.dir)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", r.dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				return nil, fmt.Errorf("embedded %s/%s is a directory; %s serves flat files only",
					r.dir, e.Name(), r.prefix)
			}
			if !strings.HasSuffix(e.Name(), r.ext) {
				return nil, fmt.Errorf("embedded %s/%s does not end in %s", r.dir, e.Name(), r.ext)
			}
			p := r.dir + "/" + e.Name()
			b, err := fs.ReadFile(webFS, p)
			if err != nil {
				return nil, fmt.Errorf("read embedded %s: %w", p, err)
			}
			out = append(out, remoteBootstrapEntryFor(r.prefix+e.Name(), r.kind, b))
		}
	}

	err = fs.WalkDir(webFS, "web/assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(webFS, p)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", p, err)
		}
		out = append(out, remoteBootstrapEntryFor("/assets/"+strings.TrimPrefix(p, "web/assets/"), "asset", b))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk embedded web/assets: %w", err)
	}
	return out, nil
}

func remoteBootstrapEntryFor(url, kind string, b []byte) remoteBootstrapEntry {
	return remoteBootstrapEntry{
		URL:       url,
		Kind:      kind,
		Size:      len(b),
		Integrity: sha256Integrity(b),
	}
}

func sha256Integrity(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}
