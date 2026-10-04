package app

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// claudeTrustedFolder reads Claude's undocumented trust file read-only. Any
// error degrades to ""; callers must retain their neutral-directory fallback.
func claudeTrustedFolder(home string) string {
	const max = 32 << 20
	f, err := os.Open(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || len(b) > max {
		return ""
	}
	var doc struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	var dirs []string
	for path, project := range doc.Projects {
		if !project.Trusted || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			continue
		}
		st, err := os.Stat(path)
		if err == nil && st.IsDir() {
			dirs = append(dirs, path)
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return ""
	}
	return dirs[0]
}
