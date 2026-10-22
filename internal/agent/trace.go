package agent

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/jsonl"
)

// Edit is one file change an agent made through its edit tools, with what it
// said it was doing at the time.
//
// The review uses these to answer "why is this hunk here" from the session
// that wrote it. A diff records what changed; the transcript is the only
// place that records which request and which stated intent produced it.
type Edit struct {
	Session string    `json:"session"`
	Time    time.Time `json:"time"`
	Tool    string    `json:"tool"` // Edit, MultiEdit, Write, NotebookEdit
	File    string    `json:"file"` // absolute, as the agent passed it
	// Text is the new text the edit put in: new_string, the edits' new
	// strings, or a written file's content.
	Text string `json:"-"`
	// Intent is the agent's last prose before the edit: its own account of
	// what it is about to do.
	Intent string `json:"intent,omitempty"`
	// Prompt is the last thing the user (or, for a subagent, its caller) asked.
	Prompt    string `json:"prompt,omitempty"`
	Sidechain bool   `json:"sidechain,omitempty"`
	// Failed edits are kept out: an edit whose old_string did not match changed
	// nothing, and must not claim a hunk.
	failed bool
}

// Command is one shell command an agent ran, and how it ended.
type Command struct {
	Session string    `json:"session"`
	Time    time.Time `json:"time"`
	Command string    `json:"command"`
	OK      bool      `json:"ok"`
	// Done is false while no result has been recorded: still running, or the
	// session was cut off.
	Done   bool   `json:"done"`
	Output string `json:"output,omitempty"` // the end of it
	Kind   string `json:"kind,omitempty"`   // test, build, lint, typecheck; "" for anything else
}

// Trace is what a workspace's transcripts say about how its code was written.
type Trace struct {
	Edits    []Edit    `json:"edits"`
	Commands []Command `json:"commands"`
	Sessions int       `json:"sessions"`
}

// maxOutput keeps the end of a command's output, which is where a test runner
// reports its verdict.
const maxOutput = 4000

// traceCache holds each transcript's parse, keyed by path and invalidated by
// size and mtime. Transcripts run to megabytes and the Review tab refreshes
// often, but only the file being written changes between refreshes.
var traceCache = struct {
	sync.Mutex
	m map[string]cachedTrace
}{m: map[string]cachedTrace{}}

type cachedTrace struct {
	size  int64
	mtime time.Time
	edits []Edit
	cmds  []Command
}

// ClaudeTrace reads every Claude Code transcript recorded in a workspace and
// returns its edits and commands, oldest first. Other agents' transcripts are
// not parsed: see Tailable.
func ClaudeTrace(home, workspaceDir string) Trace {
	tr := Trace{Edits: []Edit{}, Commands: []Command{}}
	for _, t := range ClaudeTranscripts(home, workspaceDir) {
		fi, err := os.Stat(t.Path)
		if err != nil {
			continue
		}
		traceCache.Lock()
		c, ok := traceCache.m[t.Path]
		traceCache.Unlock()
		if !ok || c.size != fi.Size() || !c.mtime.Equal(fi.ModTime()) {
			e, cmds, err := parseTrace(t.Path)
			if err != nil && len(e) == 0 && len(cmds) == 0 {
				continue
			}
			c = cachedTrace{size: fi.Size(), mtime: fi.ModTime(), edits: e, cmds: cmds}
			traceCache.Lock()
			traceCache.m[t.Path] = c
			traceCache.Unlock()
		}
		tr.Sessions++
		tr.Edits = append(tr.Edits, c.edits...)
		tr.Commands = append(tr.Commands, c.cmds...)
	}
	sort.SliceStable(tr.Edits, func(i, j int) bool { return tr.Edits[i].Time.Before(tr.Edits[j].Time) })
	sort.SliceStable(tr.Commands, func(i, j int) bool { return tr.Commands[i].Time.Before(tr.Commands[j].Time) })
	return tr
}

// parseTrace reads one transcript.
//
// Unlike readTurns, sidechain entries are kept: a subagent's edits land in the
// same worktree, and a hunk it wrote must still be explained. Its intent and
// prompt are tracked apart from the main conversation's, so neither borrows
// the other's words.
func parseTrace(path string) ([]Edit, []Command, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	type voice struct{ intent, prompt string }
	var main, side voice
	var edits []Edit
	var cmds []Command
	editAt := map[string]int{} // tool_use id -> index in edits
	cmdAt := map[string]int{}  // tool_use id -> index in cmds
	seen := map[string]bool{}

	err = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			Type        string          `json:"type"`
			UUID        string          `json:"uuid"`
			SessionID   string          `json:"sessionId"`
			IsSidechain bool            `json:"isSidechain"`
			IsMeta      bool            `json:"isMeta"`
			Timestamp   time.Time       `json:"timestamp"`
			Message     json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsMeta {
			return true
		}
		if e.UUID != "" {
			if seen[e.UUID] {
				return true // resumed and forked transcripts repeat earlier lines
			}
			seen[e.UUID] = true
		}
		if e.Type != "assistant" && e.Type != "user" {
			return true
		}
		v := &main
		if e.IsSidechain {
			v = &side
		}
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if len(e.Message) == 0 || json.Unmarshal(e.Message, &m) != nil {
			return true
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			if e.Type == "user" && strings.TrimSpace(s) != "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
				// A string starting with "<" is the CLI's own markup (a slash
				// command's echo, a local command's output), not a request.
				v.prompt = clip(strings.TrimSpace(s), 600)
			}
			return true
		}
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			return true
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					if e.Type == "assistant" {
						v.intent = clip(t, 600)
					} else if !strings.HasPrefix(t, "<") {
						v.prompt = clip(t, 600)
					}
				}
			case "tool_use":
				if ed, ok := editOf(b.Name, b.Input); ok {
					ed.Session, ed.Time, ed.Sidechain = e.SessionID, e.Timestamp, e.IsSidechain
					ed.Intent, ed.Prompt = v.intent, v.prompt
					editAt[b.ID] = len(edits)
					edits = append(edits, ed)
				} else if b.Name == "Bash" {
					var in struct {
						Command string `json:"command"`
					}
					if json.Unmarshal(b.Input, &in) == nil && in.Command != "" {
						cmdAt[b.ID] = len(cmds)
						cmds = append(cmds, Command{Session: e.SessionID, Time: e.Timestamp,
							Command: in.Command, Kind: CommandKind(in.Command)})
					}
				}
			case "tool_result":
				if i, ok := cmdAt[b.ToolUseID]; ok {
					cmds[i].Done, cmds[i].OK = true, !b.IsError
					cmds[i].Output = tail(resultText(b.Content), maxOutput)
				}
				if i, ok := editAt[b.ToolUseID]; ok && b.IsError {
					edits[i].failed = true
				}
			}
		}
		return true
	})
	kept := edits[:0]
	for _, ed := range edits {
		if !ed.failed {
			kept = append(kept, ed)
		}
	}
	return kept, cmds, err
}

// editOf reads the input of an edit tool. The shapes are Claude Code's.
func editOf(name string, input json.RawMessage) (Edit, bool) {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		NewString    string `json:"new_string"`
		Content      string `json:"content"`
		NewSource    string `json:"new_source"`
		Edits        []struct {
			NewString string `json:"new_string"`
		} `json:"edits"`
	}
	switch name {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
	default:
		return Edit{}, false
	}
	if json.Unmarshal(input, &in) != nil {
		return Edit{}, false
	}
	ed := Edit{Tool: name, File: in.FilePath}
	switch name {
	case "Edit":
		ed.Text = in.NewString
	case "MultiEdit":
		parts := make([]string, 0, len(in.Edits))
		for _, x := range in.Edits {
			parts = append(parts, x.NewString)
		}
		ed.Text = strings.Join(parts, "\n")
	case "Write":
		ed.Text = in.Content
	case "NotebookEdit":
		ed.File, ed.Text = in.NotebookPath, in.NewSource
	}
	return ed, ed.File != ""
}

// resultText flattens a tool result: a plain string, or a list of blocks of
// which the text ones count.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Commands that prove something about the code, by kind. A command is
// matched anywhere in a shell line, so "cd api && go test ./..." counts. The
// tool name has to stand as a word of its own, so a "grep test" does not.
var commandKinds = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"test", regexp.MustCompile(`(^|[\s;&|(])(go test|(npm|pnpm|yarn|bun)( run)? test|npx (jest|vitest|playwright)|jest|vitest|pytest|python3? -m (pytest|unittest)|cargo test|mvn( \S+)* (test|verify)|(\./)?gradlew?( \S+)* (\S+:)?test|make (test|check)|rspec|bundle exec rspec|phpunit|dotnet test|mix test|swift test|ctest|tox|nox)\b`)},
	{"typecheck", regexp.MustCompile(`(^|[\s;&|(])(tsc|npx tsc|mypy|pyright|go vet|(npm|pnpm|yarn) (run )?(typecheck|type-check|check-types))\b`)},
	{"lint", regexp.MustCompile(`(^|[\s;&|(])(eslint|npx eslint|(npm|pnpm|yarn) (run )?lint|golangci-lint|ruff|flake8|pylint|rubocop|clippy|cargo clippy|biome (check|lint)|gofmt -l|staticcheck|shellcheck)\b`)},
	{"build", regexp.MustCompile(`(^|[\s;&|(])(go build|(npm|pnpm|yarn|bun) (run )?build|cargo build|mvn( \S+)* (package|compile|install)|(\./)?gradlew?( \S+)* (build|assemble)|make( build)?$|make build|dotnet build|swift build|docker build|vite build|next build)\b`)},
}

// CommandKind classifies a shell command by what it proves, or "" if it
// proves nothing about the code.
func CommandKind(cmd string) string {
	for _, k := range commandKinds {
		if k.re.MatchString(cmd) {
			return k.kind
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}

// utf8RuneStart reports whether b begins a character, so a cut never splits one.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
