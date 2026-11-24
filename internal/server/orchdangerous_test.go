package server

import (
	"testing"

	"github.com/Icewater1337/fride/internal/orchestrator"
	"github.com/Icewater1337/fride/internal/settings"
)

// A task the orchestrator creates without saying skips its agent's permission
// checks only when config.json opts in; an explicit choice wins either way.
func TestOrchestratorTasksFollowTheDangerousSetting(t *testing.T) {
	s, _, _ := testServer(t)
	h := orchHost{s}
	no := false
	create := func(name string, dangerous *bool) bool {
		t.Helper()
		v, err := h.CreateTask(orchestrator.NewTask{Name: name, Dangerous: dangerous,
			Repos: []orchestrator.NewRepo{{Name: "api", BaseRef: "main"}}})
		if err != nil {
			t.Fatal(err)
		}
		task, err := s.st.Get(v.Slug)
		if err != nil {
			t.Fatal(err)
		}
		return task.Dangerous
	}
	if create("off-by-default", nil) {
		t.Fatal("without the setting, an orchestrator task skipped permission checks")
	}
	s.cfg.Settings.Orchestrator = &settings.Orchestrator{Dangerous: true}
	if !create("on-by-setting", nil) {
		t.Fatal("with orchestrator.dangerous, an orchestrator task kept its permission checks")
	}
	if create("asked-for-checks", &no) {
		t.Fatal("an explicit dangerous=false was overridden by the setting")
	}
}
