package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	turns := readTurns(path)
	if len(turns) == 0 {
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
// Read from the transcript rather than the screen because the screen is a
// repainted TUI and this decision is too consequential to take from a guess.
func Pending(k Kind, home, workspaceDir, sessionID string) string {
	if !Tailable(k) || sessionID == "" {
		return ""
	}
	path := transcriptPath(home, workspaceDir, sessionID)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	names := map[string]string{} // tool_use id -> tool name
	var order []string
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Content []struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Name      string `json:"name"`
					ToolUseID string `json:"tool_use_id"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.IsSidechain {
			// A subagent's tool calls are answered inside its own loop and never
			// reach a dialog, so counting them would report a block that is not
			// there.
			continue
		}
		for _, b := range e.Message.Content {
			switch b.Type {
			case "tool_use":
				if b.ID != "" {
					names[b.ID] = b.Name
					order = append(order, b.ID)
				}
			case "tool_result":
				delete(names, b.ToolUseID)
			}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if n, ok := names[order[i]]; ok {
			return n
		}
	}
	return ""
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
	prefix := claudeEncode(filepath.Clean(workspaceDir))
	des, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		if n := de.Name(); n != prefix && !strings.HasPrefix(n, prefix+"-") {
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
func readTurns(path string) []Turn {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// A single line can carry a whole tool result and dwarfs the default limit.
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	var out []Turn
	seen := map[string]bool{}
	for sc.Scan() {
		var e struct {
			Type        string          `json:"type"`
			UUID        string          `json:"uuid"`
			IsSidechain bool            `json:"isSidechain"`
			IsMeta      bool            `json:"isMeta"`
			Timestamp   time.Time       `json:"timestamp"`
			Message     json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.UUID == "" || e.IsSidechain || e.IsMeta || seen[e.UUID] {
			continue // resumed and forked transcripts repeat earlier lines
		}
		if e.Type != "assistant" && e.Type != "user" {
			continue
		}
		t, ok := flatten(e.Type, e.Message)
		if !ok {
			continue
		}
		seen[e.UUID] = true
		t.UUID, t.Time = e.UUID, e.Timestamp
		out = append(out, t)
	}
	return out
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
