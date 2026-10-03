package app

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// vibeInstalledVersion reads the distribution metadata next to a Python or
// bundled vibe-acp launcher. Unlike vibe-acp --version, this does not start
// Vibe or let its startup create files in the user's configuration directory.
// An ambiguous or unsupported installation leaves the version unknown.
func vibeInstalledVersion(bin string) string {
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	prefix := filepath.Dir(filepath.Dir(resolved))
	patterns := []string{
		filepath.Join(prefix, "lib", "python*", "site-packages", "mistral_vibe-*.dist-info", "METADATA"),
		filepath.Join(prefix, "Lib", "site-packages", "mistral_vibe-*.dist-info", "METADATA"),
		filepath.Join(filepath.Dir(resolved), "_internal", "mistral_vibe-*.dist-info", "METADATA"),
	}
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return ""
		}
		files = append(files, matches...)
	}
	if len(files) != 1 {
		return ""
	}
	return vibeVersionFromMetadata(files[0])
}

func vibeVersionFromMetadata(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scan := bufio.NewScanner(io.LimitReader(f, 64<<10))
	var name, version string
	ended := false
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			ended = true
			break
		}
		if strings.HasPrefix(line, "Name: ") {
			if name != "" {
				return ""
			}
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name: "))
		}
		if strings.HasPrefix(line, "Version: ") {
			if version != "" {
				return ""
			}
			version = strings.TrimSpace(strings.TrimPrefix(line, "Version: "))
		}
	}
	if scan.Err() != nil || !ended || name != "mistral-vibe" || version == "" || parseHarnessVersion(version) != version {
		return ""
	}
	dir := filepath.Base(filepath.Dir(path))
	if dir != "mistral_vibe-"+version+".dist-info" {
		return ""
	}
	return version
}
