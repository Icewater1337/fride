package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/jsonl"
)

// Turn is one message of a live agent conversation, flattened to the little the
// orchestrator needs to read: who spoke, what they said in prose, and which
// tools they reached for.
//
// Reading the transcript rather than the terminal is what makes supervision
// reliable. A PTY carries a fullscreen TUI — cursor addressing, spinners, a
// redrawn box — so recovering "what did the agent just ask me" from those bytes
// is guesswork. The transcript is the same conversation already in structured
// form.
type Turn struct {
	UUID  string    `json:"uuid"`
	Role  string    `json:"role"` // "assistant" | "user"
	Text  string    `json:"text,omitempty"`
	Tools []string  `json:"tools,omitempty"`
	Time  time.Time `json:"time"`
}

// Tailable reports whether this agent's conversations can be read as they
// happen. Only Claude Code writes a transcript in a shape that is stable enough
// to follow turn by turn; the others are watched through their terminal
// instead — see term.Session.Screen.
func Tailable(k Kind) bool { return Normalize(string(k)) == Claude }

// Tail returns the turns of one conversation that follow afterUUID, oldest
// first, capped at max. The second result is the cursor to pass next time; it
// is unchanged when nothing new was written, so a caller can tell "quiet" from
// "said something".
//
// An unknown afterUUID (a cursor from a transcript that has since been replaced
// by /clear, or the very first read) yields the last max turns rather than the
// whole history: the point is to catch up on what just happened, not to replay
// a conversation that may be thousands of turns long.
func Tail(k Kind, home, workspaceDir, sessionID, afterUUID string, max int) ([]Turn, string) {
	if !Tailable(k) || sessionID == "" {
		return nil, afterUUID
	}
	path := transcriptPath(home, workspaceDir, sessionID)
	if path == "" {
		return nil, afterUUID
	}
	turns, err := readTurns(path)
	if err != nil || len(turns) == 0 {
		// An unreadable transcript reads as quiet, with the cursor left where it
		// was: reporting the part read before the failure would move the cursor
		// to it, and the next look would replay what came after as news.
		return nil, afterUUID
	}
	from := 0
	if afterUUID != "" {
		for i, t := range turns {
			if t.UUID == afterUUID {
				from = i + 1
				break
			}
		}
	}
	fresh := turns[from:]
	if len(fresh) == 0 {
		return nil, afterUUID
	}
	if len(fresh) > max {
		fresh = fresh[len(fresh)-max:]
	}
	return fresh, fresh[len(fresh)-1].UUID
}

// Pending reports the tool an agent has asked to use and not had answered, or
// "" when it is simply at its prompt.
//
// This is how "waiting for you to decide something" is told apart from "waiting
// for you to say something", and the two must not be confused. An agent stopped
// on an unanswered tool call is showing a confirmation dialog, not a text box:
// its keystrokes select an option and Enter confirms whatever is selected, so
// typing a sentence at it does not answer the question — it answers some
// question, unpredictably, and says the user approved it.
//
// Only the latest turn can be showing a dialog, so only its calls count. A
// call left dangling further back — the process was killed mid-tool and the
// conversation resumed — is history, and counting it would mark the task
// blocked for good and refuse every instruction to it. Two things end a turn:
// a new assistant message (the API answers every call before the next one, so
// whatever was still open was abandoned), and a prompt typed at the agent. And
// since is when the process now running started: a call recorded before it
// cannot be a dialog that process is showing.
//
// Read from the transcript rather than the screen because the screen is a
// repainted TUI and this decision is too consequential to take from a guess.
// For the same reason a transcript that cannot be read is an error, not a "".
func Pending(k Kind, home, workspaceDir, sessionID string, since time.Time) (string, error) {
	c, err := PendingCall(k, home, workspaceDir, sessionID, since)
	return c.Name, err
}

// Call is a tool call an agent has made and not had answered.
type Call struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
	Time  time.Time       `json:"time"`
}

// PendingCall is Pending with the whole call rather than its tool's name: for
// a multiple-choice question its input is the question itself (see
// Call.Questions), which the agent's turns carry only as "used
// AskUserQuestion". The zero Call means nothing is pending.
func PendingCall(k Kind, home, workspaceDir, sessionID string, since time.Time) (Call, error) {
	if !Tailable(k) || sessionID == "" {
		return Call{}, nil
	}
	path := transcriptPath(home, workspaceDir, sessionID)
	if path == "" {
		return Call{}, nil // nothing written yet, so nothing asked
	}
	f, err := os.Open(path)
	if err != nil {
		return Call{}, err
	}
	defer f.Close()

	calls := map[string]Call{} // tool_use id -> the call
	var order []string
	message := "" // the assistant message the open calls belong to
	endTurn := func() { calls, order = map[string]Call{}, nil }
	err = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			Type        string    `json:"type"`
			UUID        string    `json:"uuid"`
			IsSidechain bool      `json:"isSidechain"`
			IsMeta      bool      `json:"isMeta"`
			Timestamp   time.Time `json:"timestamp"`
			Message     struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			// A subagent's tool calls are answered inside its own loop and never
			// reach a dialog, so counting them would report a block that is not
			// there.
			return true
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		}
		var prompt string
		if json.Unmarshal(e.Message.Content, &prompt) != nil {
			_ = json.Unmarshal(e.Message.Content, &blocks)
		}
		switch e.Type {
		case "assistant":
			// One message is written as one line per content block, all sharing
			// its id; a line without one is taken as a message of its own.
			id := e.Message.ID
			if id == "" {
				id = e.UUID
			}
			if id != message {
				endTurn()
				message = id
			}
			if !e.Timestamp.IsZero() && e.Timestamp.Before(since) {
				return true
			}
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					calls[b.ID] = Call{ID: b.ID, Name: b.Name, Input: b.Input, Time: e.Timestamp}
					order = append(order, b.ID)
				}
			}
		case "user":
			typed, results := strings.TrimSpace(prompt) != "", false
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					delete(calls, b.ToolUseID)
					results = true
				case "text":
					typed = true
				}
			}
			// Tool results come back as user lines too; only something said is
			// a new turn. Meta lines are the CLI talking to itself.
			if typed && !results && !e.IsMeta {
				endTurn()
			}
		}
		return true
	})
	if err != nil {
		return Call{}, err
	}
	for i := len(order) - 1; i >= 0; i-- {
		if c, ok := calls[order[i]]; ok {
			return c, nil
		}
	}
	return Call{}, nil
}

// transcriptPath locates the JSONL a conversation is being written to. Claude
// Code names the file after the session, so the usual case is one Stat; the
// scan behind it covers a transcript that was resumed under a directory encoded
// from a different cwd.
func transcriptPath(home, workspaceDir, sessionID string) string {
	if strings.ContainsAny(sessionID, `/\`) {
		return "" // a session id is a uuid; anything else is not ours to open
	}
	projects := filepath.Join(home, ".claude", "projects")
	des, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		if !ClaudeHolds(de.Name(), workspaceDir) {
			continue
		}
		p := filepath.Join(projects, de.Name(), sessionID+".jsonl")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// readTurns flattens a transcript into the conversation a reader would see.
//
// Three kinds of line are dropped on purpose: sidechain entries, which are a
// subagent's own conversation and not what this agent is telling the user; meta
// entries, which are the CLI talking to itself; and user entries whose content
// is a block list, which are tool results being fed back in rather than
// anything a person said.
func readTurns(path string) ([]Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Turn
	seen := map[string]bool{}
	err = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			Type        string          `json:"type"`
			UUID        string          `json:"uuid"`
			IsSidechain bool            `json:"isSidechain"`
			IsMeta      bool            `json:"isMeta"`
			Timestamp   time.Time       `json:"timestamp"`
			Message     json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			return true
		}
		if e.UUID == "" || e.IsSidechain || e.IsMeta || seen[e.UUID] {
			return true // resumed and forked transcripts repeat earlier lines
		}
		if e.Type != "assistant" && e.Type != "user" {
			return true
		}
		t, ok := flatten(e.Type, e.Message)
		if !ok {
			return true
		}
		seen[e.UUID] = true
		t.UUID, t.Time = e.UUID, e.Timestamp
		out = append(out, t)
		return true
	})
	return out, err
}

// flatten turns one message into a Turn, reporting false for the ones that
// carry nothing a reader would want.
func flatten(kind string, raw json.RawMessage) (Turn, bool) {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return Turn{}, false
	}
	t := Turn{Role: m.Role}
	if t.Role == "" {
		t.Role = kind
	}

	// A user message is either a plain string (something was actually said) or a
	// block list (tool results coming back). Only the first is a turn.
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		s = strings.TrimSpace(s)
		if s == "" || kind != "user" {
			return Turn{}, false
		}
		t.Text = s
		return t, true
	}
	if kind != "assistant" {
		return Turn{}, false
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return Turn{}, false
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if s := strings.TrimSpace(b.Text); s != "" {
				texts = append(texts, s)
			}
		case "tool_use":
			if b.Name != "" {
				t.Tools = append(t.Tools, b.Name)
			}
		}
		// "thinking" is deliberately skipped: it is the agent reasoning to
		// itself, not what it is telling anyone.
	}
	t.Text = strings.Join(texts, "\n\n")
	if t.Text == "" && len(t.Tools) == 0 {
		return Turn{}, false
	}
	return t, true
}

// QuestionTool is Claude Code's multiple-choice question. Unlike a permission
// dialog it asks for an answer, not an approval, and Esc closes it ("User
// declined to answer questions") and leaves the agent at its prompt, where the
// answer can be typed as a message. Typed straight at the dialog, text is lost
// and its Enter picks whichever option is highlighted — the first.
const QuestionTool = "AskUserQuestion"

// Question is one question of a QuestionTool call, with the options it offers.
// The person answering may also pick none of them and say something else.
type Question struct {
	Header  string   `json:"header,omitempty"`
	Text    string   `json:"question"`
	Options []Option `json:"options,omitempty"`
	Multi   bool     `json:"multiSelect,omitempty"`
}

// Option is one answer a Question offers.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Questions is what a QuestionTool call asks: nil for any other call, and for
// an input in a shape this version does not know, which leaves the caller
// with the screen and the agent's own words, as before.
func (c Call) Questions() []Question {
	if c.Name != QuestionTool {
		return nil
	}
	var in struct {
		Questions []Question `json:"questions"`
	}
	if json.Unmarshal(c.Input, &in) != nil {
		return nil
	}
	var out []Question
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Text) != "" {
			out = append(out, q)
		}
	}
	return out
}

// Failure is a Claude Code turn that ended on an API error instead of an
// answer: the CLI writes the error as a synthetic assistant message and goes
// back to its prompt, where it waits for someone to say "continue". Nothing
// else will — to everything watching, it looks like an agent that finished.
type Failure struct {
	UUID string    `json:"uuid"`
	Text string    `json:"text"`
	Time time.Time `json:"time"`
}

// Failed reports the API error the conversation's latest turn ended on, or
// nil when it ended any other way. Only errors written since the process now
// running started count, like Pending: an old one is history, not the state
// of what is on the screen.
func Failed(k Kind, home, workspaceDir, sessionID string, since time.Time) (*Failure, error) {
	if !Tailable(k) || sessionID == "" {
		return nil, nil
	}
	path := transcriptPath(home, workspaceDir, sessionID)
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last *Failure
	err = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			Type              string          `json:"type"`
			UUID              string          `json:"uuid"`
			IsSidechain       bool            `json:"isSidechain"`
			IsMeta            bool            `json:"isMeta"`
			IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
			Timestamp         time.Time       `json:"timestamp"`
			Message           json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsSidechain || e.IsMeta {
			return true
		}
		if e.Type != "assistant" && e.Type != "user" {
			return true // system lines (turn durations), attachments, bookkeeping
		}
		last = nil
		if e.Type == "assistant" && e.IsAPIErrorMessage &&
			(e.Timestamp.IsZero() || !e.Timestamp.Before(since)) {
			t, _ := flatten(e.Type, e.Message)
			text := t.Text
			if text == "" {
				text = "API Error"
			}
			last = &Failure{UUID: e.UUID, Text: text, Time: e.Timestamp}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return last, nil
}
