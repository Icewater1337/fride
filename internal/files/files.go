// Package files backs the workspace file browser. Every path is resolved and
// then checked to be inside the workspace root, so a symlink in a checkout
// cannot be used to read arbitrary files off the machine through the UI.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxFileBytes caps what the editor pane will load; build outputs and lockfiles
// in these repos are large enough to lock up a browser tab.
const maxFileBytes = 1 << 20 // 1 MiB

type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // relative to the workspace root
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// skipDirs are never listed: they are huge, uninteresting, and would make the
// tree unusable in these repos.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "build": true, ".gradle": true,
	"__pycache__": true, ".venv": true, "dist": true, ".idea": true,
	".pytest_cache": true, "target": true,
}

// resolve makes rel safe against traversal and symlink escapes.
func resolve(root, rel string) (string, error) {
	if rel == "" || rel == "." {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	full := filepath.Join(root, filepath.Clean("/"+rel))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	// The target may not exist yet; walk up to the nearest existing ancestor.
	probe := full
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("not found")
		}
		probe = parent
	}
	realProbe, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	if realProbe != realRoot && !strings.HasPrefix(realProbe, realRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the workspace")
	}
	return full, nil
}

// List returns one directory level, directories first.
func List(root, rel string) ([]Entry, error) {
	full, err := resolve(root, rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := []Entry{} // non-nil: serialised to JSON and iterated by the UI
	for _, de := range des {
		name := de.Name()
		if de.IsDir() && skipDirs[name] {
			continue
		}
		if name == ".DS_Store" {
			continue
		}
		e := Entry{Name: name, IsDir: de.IsDir(), Path: filepath.Join(strings.TrimPrefix(rel, "."), name)}
		e.Path = strings.TrimPrefix(e.Path, "/")
		if info, err := de.Info(); err == nil {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

type FileContent struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
	Binary   bool   `json:"binary"`
	Truncate bool   `json:"truncated"`
	Withheld bool   `json:"withheld,omitempty"`
}

// secretish matches the local config files repositories keep secrets in:
// .env and .env.properties hold API keys and OIDC client secrets, and .npmrc
// carries the registry token.
func secretish(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	switch {
	case base == ".npmrc", base == ".netrc", base == ".pypirc":
		return true
	case strings.HasPrefix(base, ".env"):
		// .env.template / .env.example are committed placeholders, not secrets
		return !strings.HasSuffix(base, ".template") &&
			!strings.HasSuffix(base, ".example") &&
			!strings.HasSuffix(base, ".sample")
	}
	return strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key")
}

// Read loads a file for display.
//
// Secret-bearing local config is withheld. The HTTP API is unauthenticated (it
// is loopback-only, but so is every other process on the machine), so serving
// a repo's .env would put its API keys behind a plain GET.
// A terminal tab is one click away when the real file is genuinely needed —
// that path at least requires driving a shell rather than a bare URL.
func Read(root, rel string) (*FileContent, error) {
	full, err := resolve(root, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory", rel)
	}
	if secretish(rel) {
		return &FileContent{Path: rel, Size: fi.Size(), Withheld: true}, nil
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	limit := fi.Size()
	truncated := false
	if limit > maxFileBytes {
		limit = maxFileBytes
		truncated = true
	}
	b := make([]byte, limit)
	n, _ := f.Read(b)
	b = b[:n]

	out := &FileContent{Path: rel, Size: fi.Size(), Truncate: truncated}
	// A NUL byte or invalid UTF-8 means we should not try to show it as text.
	if !utf8.Valid(b) || len(b) > 0 && strings.IndexByte(string(b), 0) >= 0 {
		out.Binary = true
		return out, nil
	}
	out.Content = string(b)
	return out, nil
}

// SafeJoin resolves rel under root with the same containment guarantee as the
// browser endpoints, for callers that need a directory (e.g. opening a terminal
// tab in a subfolder).
func SafeJoin(root, rel string) (string, error) {
	return resolve(root, rel)
}
