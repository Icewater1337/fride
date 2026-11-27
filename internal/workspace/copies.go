package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/Icewater1337/fride/internal/atomicfile"
	"github.com/Icewater1337/fride/internal/config"
)

// The files of a repository's copy list (secrets, local settings) are copied
// into a workspace when it is created. Copied once, they went stale: a secret
// rotated in the main checkout never reached the workspaces made before, and
// their tests failed on a login long after it was fixed at home. So what was
// copied is recorded, per file, as a fingerprint of its content, and
// RefreshCopies brings a workspace's copies up to date where that loses nothing.

// copiedPath is that record, in the workspace's sidecar folder: outside every
// repository, so it is never committed, and gone with the workspace.
func copiedPath(ws string) string { return filepath.Join(ws, ".fride", "copied.json") }

// readCopied returns the record, keyed "<repo>/<path>". A record that is
// missing or unreadable is empty, which RefreshCopies takes as "unknown".
func readCopied(ws string) map[string]string {
	rec := map[string]string{}
	if b, err := os.ReadFile(copiedPath(ws)); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	return rec
}

func writeCopied(ws string, rec map[string]string) error {
	if len(rec) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(copiedPath(ws)), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(copiedPath(ws), b, 0o600)
}

// fingerprint is a file's content as a hash, which is all the record keeps:
// these files hold secrets.
func fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RefreshCopies brings the copied files of a workspace's repositories up to
// date with their main checkouts, and returns the ones it copied or merged, as
// "<repo>/<path>", and notes on what it could not merge. A shared file is
// merged key by key (shared.go); any other file is copied when:
//   - the workspace has none (also how a file added to the copy list later
//     reaches older workspaces), or
//   - the main checkout's changed, and the workspace's is still what was copied
//     there (unchanged since, by the record).
//
// Never over a file edited in the workspace, nor over one copied before the
// record existed, which cannot be told apart from an edit: overwriting is the
// decision that loses something, so anything unread or unknown keeps the file.
func RefreshCopies(cfg *config.Config, slug string) (refreshed, notes []string) {
	ws := cfg.WorkspaceDir(slug)
	if cfg.Settings == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return nil, nil
	}
	refreshed, notes = refreshShared(cfg, slug)
	rec := readCopied(ws)
	for _, e := range entries {
		repo, ok := cfg.Repo(e.Name())
		if !ok || !e.IsDir() {
			continue
		}
		dest := filepath.Join(ws, e.Name())
		lines, _ := copyCandidates(cfg, repo.Name, repo.Main)
		for _, line := range lines {
			if slices.Contains(cfg.Settings.Repo(repo.Name).Share, line) {
				continue // merged by refreshShared
			}
			key := repo.Name + "/" + line
			src, dst := filepath.Join(repo.Main, line), filepath.Join(dest, line)
			main, err := fingerprint(src)
			if err != nil {
				continue
			}
			here, err := fingerprint(dst)
			switch {
			case err == nil && here == main:
				rec[key] = main // in step; known from now on
				continue
			case errors.Is(err, fs.ErrNotExist):
			case err == nil && rec[key] != "" && here == rec[key]:
			default:
				continue // edited here, unknown, or unreadable
			}
			fi, err := os.Lstat(src)
			if err != nil || copyFile(src, dst, fi) != nil {
				continue
			}
			rec[key] = main
			refreshed = append(refreshed, key)
		}
	}
	_ = writeCopied(ws, rec)
	return refreshed, notes
}
