package settings

import (
	"encoding/json"
	"testing"
)

func TestPRReviewIsOnUnlessTurnedOff(t *testing.T) {
	for in, want := range map[string]bool{
		`{}`:                        true,
		`{"pr": {}}`:                true,
		`{"pr": {"review": true}}`:  true,
		`{"pr": {"review": false}}`: false,
		`{"repos": {"api": {"build": "go build ./..."}}}`: true,
	} {
		var s Settings
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatal(err)
		}
		if got := s.PRReview(); got != want {
			t.Errorf("%s: PRReview() = %v, want %v", in, got, want)
		}
	}
	var s Settings
	_ = json.Unmarshal([]byte(`{"repos": {"api": {"build": "go build ./...", "test": "go test ./..."}}}`), &s)
	if r := s.Repo("api"); r.Build != "go build ./..." || r.Test != "go test ./..." {
		t.Errorf("repos.api = %+v", r)
	}
	if (*Settings)(nil).PRReview() != true {
		t.Error("no settings at all should keep the review on")
	}
}
