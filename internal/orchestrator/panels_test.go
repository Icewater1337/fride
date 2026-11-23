package orchestrator

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/panels"
)

func TestPanelActionsWriteEditAndRemove(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	o.apply(Reply{Actions: []Action{{Kind: ActPanelUpsert, Panel: "board", Title: "Board",
		Summary: "the tasks by status", Note: "first", HTML: "<h1>Board</h1><p>old</p>"}}}, user)
	p, err := o.Panels().Get("board")
	if err != nil || p.Version != 1 || p.Title != "Board" {
		t.Fatalf("%+v %v", p, err)
	}
	o.apply(Reply{Actions: []Action{{Kind: ActPanelUpsert, Panel: "board",
		Edits: []panels.Edit{{Find: "<p>old</p>", Replace: "<p>new</p>"}}}}}, user)
	if p, _ = o.Panels().Get("board"); p.HTML != "<h1>Board</h1><p>new</p>" || p.Version != 2 {
		t.Fatalf("after edits: %+v", p)
	}
	// An edit that misses is reported in the chat and changes nothing.
	o.apply(Reply{Actions: []Action{{Kind: ActPanelUpsert, Panel: "board",
		Edits: []panels.Edit{{Find: "<p>gone</p>", Replace: "x"}}}}}, user)
	if !strings.Contains(lastText(o), "not in the panel's source") {
		t.Fatalf("a missed edit reads %q", lastText(o))
	}

	// The prompt lists it with the path its source can be read from.
	in := o.prompt("THE USER SAYS:\nhi\n\n", nil)
	if !strings.Contains(in, "PANELS") || !strings.Contains(in, o.Panels().Path("board")) ||
		!strings.Contains(in, "the tasks by status") {
		t.Fatalf("prompt has no panel list:\n%s", in)
	}

	o.apply(Reply{Actions: []Action{{Kind: ActPanelRemove, Panel: "board"}}}, user)
	if len(o.Panels().List()) != 0 {
		t.Fatal("not removed")
	}
}

func TestInlineWidgetIsAChatMessage(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	o.apply(Reply{Actions: []Action{{Kind: ActInline, Title: "Costs", HTML: "<canvas></canvas>"}}}, user)
	msgs := o.Snapshot().Messages
	var found bool
	for _, m := range msgs {
		found = found || (m.HTML == "<canvas></canvas>" && m.Text == "Costs")
	}
	if !found {
		t.Fatalf("no widget message: %+v", msgs)
	}
}

// A UI event is queued and read as its own turn, the user's, with the data
// it came with; a turn about a panel runs on the panel model.
func TestUIEventIsReadAsAUserTurn(t *testing.T) {
	st := stubClaude(t, Reply{Say: "fixed it"})
	o := New(t.TempDir(), t.TempDir(), st.bin, newFakeHost())
	t.Cleanup(func() { o.Close(); o.Wait() })
	if _, err := o.Panels().Upsert("board", "Board", "", "", "<p>x</p>", nil); err != nil {
		t.Fatal(err)
	}
	if err := o.UIEvent("nope", "x", nil); err == nil {
		t.Fatal("an event for no panel was queued")
	}
	if err := o.UIEvent("board", "the chart is empty", json.RawMessage(`{"slug":"proj-1"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(lastText(o), "fixed it") })
	in, _ := os.ReadFile(st.stdin)
	if !strings.Contains(string(in), "UI EVENT (panel board): the chart is empty") ||
		!strings.Contains(string(in), `DATA: {"slug":"proj-1"}`) {
		t.Fatalf("prompt:\n%s", in)
	}
	argv, _ := os.ReadFile(st.argv)
	if !strings.Contains(string(argv), "--model\nopus\n") || !strings.Contains(string(argv), "--add-dir\n"+o.Panels().Dir()) {
		t.Fatalf("argv:\n%s", argv)
	}
}

func TestUIEventsAreBounded(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	if _, err := o.Panels().Upsert("p", "P", "", "", "x", nil); err != nil {
		t.Fatal(err)
	}
	// Held: a turn is running, so nothing is read.
	o.mu.Lock()
	o.thinking = true
	o.mu.Unlock()
	for i := 0; i < maxUIQueue; i++ {
		if err := o.UIEvent("p", "click", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.UIEvent("p", "click", nil); err == nil {
		t.Fatal("the queue grew past its bound")
	}
	o.mu.Lock()
	o.thinking = false
	o.mu.Unlock()
}

func TestModelForPanelTurns(t *testing.T) {
	o := newTestOrch(t, newFakeHost())
	for text, want := range map[string]string{
		"build me a dashboard of CI":  "opus",
		"add a tab for reviews":       "opus",
		"start a task with backend":   "",
		"the panel throws an error":   "opus",
		"tablespace migration please": "",
	} {
		if got := o.modelFor(text); got != want {
			t.Errorf("modelFor(%q) = %q, want %q", text, got, want)
		}
	}
}
