package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCopiedAndRenamedFoldersAdoptedOnce(t *testing.T) {
	ws := t.TempDir()
	s, err := New(t.TempDir(), ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"foo", "moved"} {
		if err := os.Mkdir(filepath.Join(ws, slug), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.Add(&Task{Slug: slug, Name: slug, Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("cp", "-R", filepath.Join(ws, "foo"), filepath.Join(ws, "foo copy")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if out, err := exec.Command("cp", "-R", filepath.Join(ws, "foo"), filepath.Join(ws, "foo-2")).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.Rename(filepath.Join(ws, "moved"), filepath.Join(ws, "bar")); err != nil {
		t.Fatal(err)
	}

	var got []*Task
	for i := 0; i < 3; i++ {
		if got, err = s.List(); err != nil {
			t.Fatal(err)
		}
	}
	slugs := map[string]int{}
	for _, task := range got {
		slugs[task.Slug]++
	}
	// "foo copy" is not a valid slug, so it is left alone.
	want := map[string]int{"foo": 1, "foo-2": 1, "bar": 1}
	if len(slugs) != len(want) {
		t.Fatalf("got %v, want %v", slugs, want)
	}
	for k, n := range want {
		if slugs[k] != n {
			t.Fatalf("got %v, want %v", slugs, want)
		}
	}
	if side := s.readSidecar("bar"); side == nil || side.Slug != "bar" {
		t.Fatalf("bar's sidecar not rewritten: %+v", side)
	}
}
