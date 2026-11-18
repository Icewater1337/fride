package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/trash"
)

// The board's delete — and a delete from another computer of the fleet or a
// companion, which comes through the same route — keeps a copy that the
// Recently deleted list offers and restores. When the copy cannot be made the
// delete refuses with noCopy, and only notrash=1 deletes without one.
func TestDeleteRestoreThroughTheAPI(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	if w := do(t, h, "POST", "/api/tasks", `{"name":"foo","agent":"claude","repos":[{"name":"api","baseRef":"main"}]}`); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	work := filepath.Join(s.cfg.RepoDir("foo", "api"), "uncommitted.txt")
	if err := os.WriteFile(work, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "DELETE", "/api/tasks/foo?purge=1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"trash":"foo"`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(work); err == nil {
		t.Fatal("the workspace is still there")
	}

	w = do(t, h, "GET", "/api/trash", "")
	var list []trashView
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("trash: %d %s", w.Code, w.Body)
	}
	if e := list[0]; e.ID != "foo" || e.RestoreAs != "foo" || e.Taken || len(e.Repos) != 1 || !e.Repos[0].Uncommitted {
		t.Errorf("entry = %+v", e)
	}
	if w := do(t, h, "POST", "/api/trash/nope/restore", ""); w.Code != 404 {
		t.Errorf("restore of an unknown id: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/trash/foo/restore", `{}`); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(work); err != nil || string(b) != "keep me\n" {
		t.Fatalf("the uncommitted work did not come back: %q %v", b, err)
	}

	old := trash.MaxFile
	trash.MaxFile = 1 << 10
	t.Cleanup(func() { trash.MaxFile = old })
	if err := os.WriteFile(work, []byte(strings.Repeat("x", 4<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, "DELETE", "/api/tasks/foo?purge=1", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"noCopy":true`) {
		t.Fatalf("delete without a copy possible: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("a refused delete deleted the work")
	}
	if w := do(t, h, "DELETE", "/api/tasks/foo?purge=1&notrash=1", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"trash":""`) {
		t.Fatalf("delete with notrash=1: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/trash", ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("trash after notrash=1: %s", w.Body)
	}
}
