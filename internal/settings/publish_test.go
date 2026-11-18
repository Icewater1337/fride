package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// A config.json written before PublishRepo existed: its ledger and ideas keys
// must load with the same meaning, and be saved back as they were.
const oldPublishConfig = `{
  "version": 1,
  "ledger": {
    "remote": "https://git.example.com/team/ledger.git",
    "branch": "trunk",
    "dir": "docs/rationale",
    "push": "direct",
    "pushOptions": ["ci.skip", "merge_request.title={{title}}"],
    "waitForMerge": 30,
    "author": "Jane Doe <jane@example.com>",
    "onDone": false
  },
  "ideas": {
    "remote": "https://git.example.com/team/specs.git",
    "branch": "develop",
    "dir": "proposals",
    "pushOptions": ["merge_request.create"],
    "author": "Jane Doe <jane@example.com>"
  }
}
`

func TestPublishRepoKeysLoadAsBefore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(oldPublishConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, i := s.Ledger, s.Ideas
	if l == nil || i == nil {
		t.Fatalf("ledger %v, ideas %v", l, i)
	}
	if l.Remote != "https://git.example.com/team/ledger.git" || l.Branch != "trunk" || l.Dir != "docs/rationale" ||
		l.Push != "direct" || !slices.Equal(l.PushOptions, []string{"ci.skip", "merge_request.title={{title}}"}) ||
		l.WaitForMerge != 30 || l.Author != "Jane Doe <jane@example.com>" || l.OnDone == nil || *l.OnDone {
		t.Fatalf("ledger loaded as %+v", *l)
	}
	if i.Remote != "https://git.example.com/team/specs.git" || i.Branch != "develop" || i.Dir != "proposals" ||
		!slices.Equal(i.PushOptions, []string{"merge_request.create"}) || i.Author != "Jane Doe <jane@example.com>" {
		t.Fatalf("ideas loaded as %+v", *i)
	}

	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved, old map[string]any
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(oldPublishConfig), &old); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ledger", "ideas"} {
		if !reflect.DeepEqual(saved[key], old[key]) {
			t.Errorf("%s saved as %v, was %v", key, saved[key], old[key])
		}
	}
}

func TestPublishRepoChecks(t *testing.T) {
	bad := PublishRepo{Dir: "../outside", Author: "Jane Doe"}
	s := &Settings{Ledger: &Ledger{PublishRepo: bad}, Ideas: &Ideas{PublishRepo: bad}}
	var got []string
	for _, p := range s.Check() {
		if p.Error {
			got = append(got, p.Where)
		}
	}
	for _, want := range []string{"ledger.remote", "ledger.dir", "ledger.author", "ideas.remote", "ideas.dir", "ideas.author"} {
		if !slices.Contains(got, want) {
			t.Errorf("no error at %s; got %v", want, got)
		}
	}
}
