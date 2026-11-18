// Package atomicfile replaces a file whole: it writes a temp file beside it
// and renames that into place, so a reader sees the old file or the new one,
// never half of one.
package atomicfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// Write puts b at path, whose folder must exist. The temp file has a name of
// its own, so two processes writing path at once (the CLI and `fride serve`)
// never share one, and it is removed when the write fails. perm goes through
// the umask, as with os.WriteFile. Nothing is synced to disk: a rename is
// what the callers need, so that no reader sees a partial file.
func Write(path string, b []byte, perm os.FileMode) error {
	f, err := create(path, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// WriteJSON writes v as indented JSON with a trailing newline, creating
// path's folder (0755) when it is missing.
func WriteJSON(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return Write(path, append(b, '\n'), perm)
}

// create opens a new temp file beside path. Not os.CreateTemp, which makes
// every file 0600: a file meant to be 0644 would need a chmod, which ignores
// the umask. The name ends in ".tmp", which a task move leaves out of what it
// copies.
func create(path string, perm os.FileMode) (*os.File, error) {
	dir, base := filepath.Split(path)
	for range 100 {
		name := filepath.Join(dir, "."+base+"-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if !errors.Is(err, fs.ErrExist) {
			return f, err
		}
	}
	return nil, fmt.Errorf("no free temp file name beside %s", path)
}
