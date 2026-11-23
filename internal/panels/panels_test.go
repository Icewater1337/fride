package panels

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestUpsertVersionsAndKeepsAFewOld(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Upsert("board", "", "", "", "<p>x</p>", nil); err == nil {
		t.Fatal("a new panel without a title was accepted")
	}
	p, err := s.Upsert("board", "Board", "the tasks", "first", "<p>v1</p>", nil)
	if err != nil || p.Version != 1 {
		t.Fatalf("create: %+v %v", p, err)
	}
	for i := 2; i <= KeepVersions+3; i++ {
		if _, err := s.Upsert("board", "", "", "next", "<p>v"+string(rune('0'+i))+"</p>", nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get("board")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != KeepVersions+3 || got.Title != "Board" || got.Summary != "the tasks" {
		t.Fatalf("got %+v", got)
	}
	if len(got.History) != KeepVersions || got.History[0].Version != KeepVersions+2 {
		t.Fatalf("history %+v", got.History)
	}
	// The source is a plain file the orchestrator can Read.
	if b, _ := os.ReadFile(s.Path("board")); string(b) != got.HTML {
		t.Fatalf("html file %q, Get %q", b, got.HTML)
	}
	r, err := s.Restore("board", KeepVersions+2)
	if err != nil || r.HTML != "<p>v7</p>" || r.Version != KeepVersions+4 {
		t.Fatalf("restore: %+v %v", r, err)
	}
}

func TestEditsMustMatchOnce(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Upsert("p", "P", "", "", "<b>a</b><i>a</i>", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert("p", "", "", "", "", []Edit{{Find: "a", Replace: "b"}}); err == nil ||
		!strings.Contains(err.Error(), "2 times") {
		t.Fatalf("ambiguous edit: %v", err)
	}
	if _, err := s.Upsert("p", "", "", "", "", []Edit{{Find: "zzz", Replace: "b"}}); err == nil {
		t.Fatal("missing find text accepted")
	}
	p, err := s.Upsert("p", "", "", "", "", []Edit{{Find: "<b>a", Replace: "<b>c"}, {Find: "<i>a", Replace: "<i>d"}})
	if err != nil || p.HTML != "<b>c</b><i>d</i>" || p.Version != 2 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestIDsAreFileSafe(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"", "../x", "A", "a/b", "a.b", strings.Repeat("a", 41)} {
		if _, err := s.Upsert(id, "T", "", "", "x", nil); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
}

func TestStateHiddenErrorAndRemove(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Upsert("p", "P", "", "", "x", nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.State("p"); string(st) != "null" {
		t.Fatalf("empty state %s", st)
	}
	if err := s.SetState("p", json.RawMessage(`{"a": 1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState("p", json.RawMessage(`{nope`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	_ = s.SetHidden("p", true)
	_ = s.SetError("p", "boom")
	l := s.List()
	if len(l) != 1 || !l[0].Hidden || l[0].Error != "boom" || !sameJSON(l[0].State, `{"a":1}`) {
		t.Fatalf("%+v", l)
	}
	// A new version shows the panel again and clears the error; state stays.
	p, _ := s.Upsert("p", "", "", "", "y", nil)
	if p.Hidden || p.Error != "" || !sameJSON(p.State, `{"a":1}`) {
		t.Fatalf("%+v", p)
	}
	if err := s.Remove("p"); err != nil || len(s.List()) != 0 {
		t.Fatalf("remove: %v %+v", err, s.List())
	}
	if err := s.Remove("p"); err != nil {
		t.Fatal(err)
	}
}

func sameJSON(a json.RawMessage, b string) bool {
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal([]byte(b), &y)
	return reflect.DeepEqual(x, y)
}
