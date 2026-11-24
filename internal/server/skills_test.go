package server

import (
	"strings"
	"testing"
)

func TestSkillsFromTheSettingsPage(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())
	put := `{"content":"---\nname: e2e\ndescription: Test it.\n---\n","create":true}`
	if w := do(t, h, "PUT", "/api/skills/e2e", put); w.Code != 200 || !strings.Contains(w.Body.String(), `"description":"Test it."`) {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/skills/e2e", put); w.Code != 409 {
		t.Fatalf("adding a taken name = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/skills/..%2Fescape", put); w.Code == 200 {
		t.Fatalf("a path for a name = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "GET", "/api/skills/e2e", ""); !strings.Contains(w.Body.String(), `description: Test it.`) {
		t.Fatalf("read = %d %s", w.Code, w.Body)
	}
	// Never over the fleet: every agent on this computer follows a skill.
	if w := do(t, s.localAPI, "GET", "/api/skills", ""); w.Code != 404 {
		t.Fatalf("skills over the fleet API = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/skills/e2e", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"movedTo"`) ||
		!strings.Contains(w.Body.String(), `"skills":[]`) {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
}
