package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/panels"
)

// Panels are tabs the orchestrator builds in the web UI for itself: free
// HTML/JS apps, stored by internal/panels and run by the UI in a sandboxed
// frame. This file is the orchestrator's side of them: the actions that write
// them, what it is shown of them every turn, and the UI events a panel sends
// back through fride.ask.

const (
	// maxUIQueue bounds the UI events waiting for a turn. A panel that calls
	// fride.ask in a loop must be refused, not given an unbounded bill.
	maxUIQueue = 20
	// maxUIData bounds the data a UI event carries into the prompt.
	maxUIData = 16 << 10
)

// UIEvent is something the user did in a panel that needs the orchestrator.
type UIEvent struct {
	Panel string          `json:"panel"`
	Text  string          `json:"text"`
	Data  json.RawMessage `json:"data,omitempty"`
	Time  time.Time       `json:"time"`
}

// Panels is the panel store, which the server serves to the UI.
func (o *Orchestrator) Panels() *panels.Store { return o.panels }

// panelWords spot a turn that is likely to build or change a panel, which is
// worth the stronger model. Which turn writes a panel is only known after it
// ran, so this is a guess, and a wrong one costs money or quality, never a
// wrong action.
var panelWords = regexp.MustCompile(`(?i)\b(panels?|dashboards?|widgets?|charts?|tabs?|ui)\b`)

// modelFor is the model for a turn the user started with text: the panel
// model when the text is about panels, else "" for the routine one.
func (o *Orchestrator) modelFor(text string) string {
	if !panelWords.MatchString(text) {
		return ""
	}
	return o.host.PanelModel()
}

// UIEvent queues an event from a panel. It is a queue rather than Say's 409
// because a panel's button has nobody to retry it: the click is read as soon
// as the turn in progress is over.
func (o *Orchestrator) UIEvent(panel, text string, data json.RawMessage) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("say something")
	}
	if len(text) > maxInboxText {
		return fmt.Errorf("the event text is %d bytes; the limit is %d", len(text), maxInboxText)
	}
	if len(data) > maxUIData {
		return fmt.Errorf("the event data is %d bytes; the limit is %d", len(data), maxUIData)
	}
	if len(data) > 0 && !json.Valid(data) {
		return fmt.Errorf("the event data is not JSON")
	}
	if !strings.HasPrefix(panel, "inline-") {
		if _, err := o.panels.Get(panel); err != nil {
			return err
		}
	}
	o.mu.Lock()
	if o.closed() {
		o.mu.Unlock()
		return fmt.Errorf("the orchestrator has stopped")
	}
	if len(o.st.UIQueue) >= maxUIQueue {
		o.mu.Unlock()
		return fmt.Errorf("%d panel events are already waiting; try again once they are read", maxUIQueue)
	}
	o.st.UIQueue = append(o.st.UIQueue, UIEvent{Panel: panel, Text: text, Data: data, Time: time.Now()})
	o.save()
	o.mu.Unlock()
	o.kick()
	return nil
}

// drainUI starts a turn for the next UI event. Called with the lock held, when
// a turn may start; returns with it released.
func (o *Orchestrator) drainUI() {
	ev := o.st.UIQueue[0]
	o.st.UIQueue = o.st.UIQueue[1:]
	o.thinking = true
	gen := o.gen
	o.lastErr = ""
	o.add(RoleUser, "", "["+ev.Panel+"] "+ev.Text)
	// The user acting in a panel is the user speaking: what the orchestrator
	// was blocked on is answered, exactly as in Say.
	o.mem.awaiting = false
	for slug, ts := range o.mem.tasks {
		ts.unanswered = 0
		o.mem.tasks[slug] = ts
	}
	o.turns.Add(1)
	o.save()
	o.mu.Unlock()

	lead := "UI EVENT (panel " + ev.Panel + "): " + ev.Text + "\n"
	if len(ev.Data) > 0 {
		lead += "DATA: " + string(ev.Data) + "\n"
	}
	lead += "\nThe user did this in your panel in the web UI. Act on it as on the user " +
		"speaking in the chat, and say in a line what you did.\n\n"
	input := o.prompt(lead, nil)
	tr := turn{origin: fromUser, model: o.modelFor(ev.Text)}
	go func() {
		defer o.turns.Done()
		_ = o.think(context.Background(), gen, input, tr, nil)
	}()
}

// panelAct carries out a panel action.
func (o *Orchestrator) panelAct(a Action) error {
	switch a.Kind {
	case ActPanelUpsert:
		_, err := o.panels.Upsert(a.Panel, a.Title, a.Summary, a.Note, a.HTML, a.Edits)
		return err
	case ActPanelRemove:
		return o.panels.Remove(a.Panel)
	case ActInline:
		if strings.TrimSpace(a.HTML) == "" {
			return fmt.Errorf("an inline widget needs html")
		}
		if len(a.HTML) > panels.MaxHTML {
			return fmt.Errorf("the inline widget is %d bytes; the limit is %d", len(a.HTML), panels.MaxHTML)
		}
		o.mu.Lock()
		text := strings.TrimSpace(a.Title)
		if text == "" {
			text = "(widget)"
		}
		o.st.Seq++
		o.st.Messages = append(o.st.Messages, Message{ID: o.st.Seq, Role: RoleOrchestrator,
			Task: a.Task, Text: text, HTML: a.HTML, Time: time.Now()})
		o.save()
		o.mu.Unlock()
		return nil
	}
	return fmt.Errorf("unknown action %q", a.Kind)
}

func isPanelAct(kind string) bool {
	return kind == ActPanelUpsert || kind == ActPanelRemove || kind == ActInline
}

func describePanel(a Action) string {
	switch a.Kind {
	case ActPanelUpsert:
		s := "panel " + a.Panel
		if a.Title != "" {
			s += " (" + a.Title + ")"
		}
		if len(a.Edits) > 0 && a.HTML == "" {
			s += fmt.Sprintf(": %d edit(s)", len(a.Edits))
		} else {
			s += " written"
		}
		if a.Note != "" {
			s += " — " + a.Note
		}
		return s
	case ActPanelRemove:
		return "removed panel " + a.Panel
	}
	return "widget in the chat"
}

// renderPanels is the PANELS section of the prompt: what exists, where its
// source is, and what the user and the panel itself have done with it.
func (o *Orchestrator) renderPanels() string {
	ps := o.panels.List()
	var b strings.Builder
	b.WriteString("\nPANELS (your tabs in the web UI; Read a source before editing it):\n")
	if len(ps) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, p := range ps {
		fmt.Fprintf(&b, "  %s — %s [v%d", p.ID, p.Title, p.Version)
		if p.Hidden {
			b.WriteString(", HIDDEN by the user")
		}
		b.WriteString("] " + o.panels.Path(p.ID) + "\n")
		if p.Summary != "" {
			b.WriteString("    " + clip(p.Summary, 200) + "\n")
		}
		if len(p.State) > 0 && string(p.State) != "null" {
			b.WriteString("    state: " + clip(string(p.State), 400) + "\n")
		}
		if p.Error != "" {
			b.WriteString("    ERROR in the UI: " + clip(p.Error, 400) + "\n")
		}
	}
	return b.String()
}
