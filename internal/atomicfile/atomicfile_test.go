package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteReplacesTheFileWithItsMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state")
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	for _, c := range []struct {
		content string
		perm    os.FileMode
	}{{"one\n", 0o644}, {"two\n", 0o600}} {
		if err := Write(p, []byte(c.content), c.perm); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || string(b) != c.content {
			t.Fatalf("read %q, %v; want %q", b, err, c.content)
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != c.perm {
			t.Fatalf("mode %v, %v; want %v", fi.Mode().Perm(), err, c.perm)
		}
	}
	onlyFile(t, dir, "state")
}

func TestWriteGoesThroughTheUmask(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state")
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	if err := Write(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v; want 0600, as os.WriteFile gives", fi.Mode().Perm(), err)
	}
}

func TestWriteLeavesNoTempFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	// A folder in the way: the rename over it fails.
	if err := os.Mkdir(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(filepath.Join(dir, "state"), []byte("x"), 0o644); err == nil {
		t.Fatal("wrote over a folder")
	}
	onlyFile(t, dir, "state")
}

func TestWriteJSONMakesTheFolderAndEndsWithANewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "state.json")
	if err := WriteJSON(p, map[string]int{"k": 1}, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"k\": 1\n}\n"; string(b) != want {
		t.Fatalf("wrote %q, want %q", b, want)
	}
}

func TestWriteJSONWritesNothingItCannotMarshal(t *testing.T) {
	dir := t.TempDir()
	if err := WriteJSON(filepath.Join(dir, "state.json"), func() {}, 0o644); err == nil {
		t.Fatal("marshalled a func")
	}
	onlyFile(t, dir)
}

// onlyFile fails unless dir holds exactly these names: no temp file is left.
func onlyFile(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(names) {
		t.Fatalf("%s holds %v, want %v", dir, got, names)
	}
	for i := range names {
		if got[i] != names[i] {
			t.Fatalf("%s holds %v, want %v", dir, got, names)
		}
	}
}
