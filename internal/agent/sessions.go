package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/Icewater1337/fride/internal/jsonl"
)

// Recorded is one past conversation that can be reopened.
//
// Every agent records its sessions somewhere, but in three different shapes, so
// enumerating them is per-agent knowledge and belongs in this table alongside
// the argv. What they have in common is only what the UI needs: an id to resume
// by, a time to order and label by, and a rough size.
type Recorded struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	Messages     int       `json:"messages"`
	LastActivity time.Time `json:"lastActivity"`
	Dir          string    `json:"dir,omitempty"`
	// Started is the earliest timestamp in the transcript. It is how a
	// conversation can be dated against the processes that are running: /clear
	// abandons a conversation and begins a new one mid-session, so a transcript
	// that starts after a live agent did is one of that agent's own.
	Started time.Time `json:"started,omitempty"`
	// InUse marks a conversation that something is still writing. It is offered
	// for reading, not for resuming: a second process on one transcript is how
	// two agents end up interleaving turns into the same file.
	InUse bool `json:"inUse,omitempty"`
}

// Sessions lists the conversations recorded for a workspace, newest first.
// workspaceDir is matched against the session's own working directory, so an
// agent started in one repository of the workspace still belongs to it.
func Sessions(k Kind, home, workspaceDir string) []Recorded {
	var out []Recorded
	switch Normalize(string(k)) {
	case Claude:
		out = claudeSessions(home, workspaceDir)
	case Codex:
		out = codexSessions(home, workspaceDir)
	case Antigravity:
		out = antigravitySessions(home, workspaceDir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivity.After(out[j].LastActivity) })
	return out
}

// Under reports whether p is root or nested inside it.
func Under(p, root string) bool {
	p, root = filepath.Clean(p), filepath.Clean(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// --- claude -------------------------------------------------------------------

// Claude Code writes one JSONL per session under
// ~/.claude/projects/<encoded-cwd>/. The directory name is lossy, so each
// candidate is confirmed against the cwd recorded inside it.
//
// claudeEncode is Claude Code's own naming: every UTF-16 unit that is not an
// ASCII letter or digit becomes "-" (so a space, "@" or "é" does too), and a
// name over 200 units is cut there and suffixed with the base-36 absolute value
// of a 32-bit (h<<5)-h+c hash of the original path.
func claudeEncode(p string) string {
	units := utf16.Encode([]rune(p))
	b := make([]byte, len(units))
	var h int32
	for i, u := range units {
		h = h*31 + int32(u) // wraps like JS's |0
		if u < 128 && ('a' <= u && u <= 'z' || 'A' <= u && u <= 'Z' || '0' <= u && u <= '9') {
			b[i] = byte(u)
		} else {
			b[i] = '-'
		}
	}
	if len(b) <= claudeMaxName {
		return string(b)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return string(b[:claudeMaxName]) + "-" + strconv.FormatInt(abs, 36)
}

const claudeMaxName = 200

// ClaudeHolds reports whether the project directory called name can hold
// conversations started in workspaceDir or a folder inside it. A cut name
// keeps only its first 200 units comparable, since the hash differs per folder;
// either way the cwd recorded inside each transcript has the final say.
func ClaudeHolds(name, workspaceDir string) bool {
	prefix := claudeEncode(filepath.Clean(workspaceDir))
	if len(prefix) > claudeMaxName {
		return strings.HasPrefix(name, prefix[:claudeMaxName]+"-")
	}
	return name == prefix || strings.HasPrefix(name, prefix+"-")
}

func claudeSessions(home, workspaceDir string) []Recorded {
	projects := filepath.Join(home, ".claude", "projects")
	des, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	byID := map[string]*Recorded{}
	seen := map[string]bool{}

	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		if !ClaudeHolds(de.Name(), workspaceDir) {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(projects, de.Name(), "*.jsonl"))
		for _, f := range files {
			claudeAccumulate(f, workspaceDir, byID, seen)
		}
	}
	out := make([]Recorded, 0, len(byID))
	for _, r := range byID {
		out = append(out, *r)
	}
	return out
}

func claudeAccumulate(path, workspaceDir string, byID map[string]*Recorded, seen map[string]bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	confirmed, foreign := false, false
	// The transcript's own start, taken from any line: the first entries of a
	// conversation begun by /clear are the command and its echo, not an
	// assistant turn, and those are exactly the ones being dated here.
	var first time.Time
	// title is what Claude Code called this conversation. It is worth having
	// because one terminal tab routinely produces several conversations without
	// the user opening anything: /clear starts one, and so does changing the
	// model or updating the CLI. Offered back as a row of timestamps they look
	// like duplicates of the session you are sitting in; titled, "Switch to
	// Fable 5" next to "Implementation status" needs no explanation.
	//
	// Taken from the LAST ai-title entry: the title is rewritten as the
	// conversation develops, and the newest one describes it best.
	var title string
	// A read error leaves the conversation counted as far as it could be read,
	// which only understates its size; the list is for choosing what to reopen.
	_ = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			Type      string    `json:"type"`
			UUID      string    `json:"uuid"`
			SessionID string    `json:"sessionId"`
			CWD       string    `json:"cwd"`
			AITitle   string    `json:"aiTitle"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			return true
		}
		// Title entries carry no cwd, so they are read before the confirmation
		// below would skip them — and only kept once the file has been
		// confirmed as this workspace's, which the assignment at the end does.
		if e.Type == "ai-title" && e.AITitle != "" {
			title = e.AITitle
		}
		if !confirmed {
			if e.CWD == "" {
				return true
			}
			if !Under(e.CWD, workspaceDir) {
				foreign = true // encoded-name collision: a different workspace
				return false
			}
			confirmed = true
		}
		if !e.Timestamp.IsZero() && (first.IsZero() || e.Timestamp.Before(first)) {
			first = e.Timestamp
		}
		// Resumed and forked transcripts repeat earlier lines.
		if e.Type != "assistant" || e.UUID == "" || seen[e.UUID] {
			return true
		}
		if e.Message.Usage.InputTokens == 0 && e.Message.Usage.OutputTokens == 0 {
			return true
		}
		seen[e.UUID] = true
		id := e.SessionID
		if id == "" {
			id = filepath.Base(path)
		}
		r := byID[id]
		if r == nil {
			r = &Recorded{ID: id, Dir: e.CWD}
			byID[id] = r
		}
		r.Messages++
		if e.Timestamp.After(r.LastActivity) {
			r.LastActivity = e.Timestamp
		}
		if r.Started.IsZero() || first.Before(r.Started) {
			r.Started = first
		}
		if title != "" {
			r.Title = title
		}
		return true
	})
	// A title written after the last assistant turn still belongs to it.
	if title != "" && !foreign {
		if id := sessionIDFor(byID, path); id != "" {
			byID[id].Title = title
		}
	}
}

// sessionIDFor names the conversation a transcript file holds, for the last
// pass above. Claude Code names the file after the session, so the filename is
// the answer whenever it is one this scan already recorded.
func sessionIDFor(byID map[string]*Recorded, path string) string {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if _, ok := byID[id]; ok {
		return id
	}
	return ""
}

// --- codex ---------------------------------------------------------------------

// Codex writes ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl, whose
// first line carries the session id and the cwd. Only that line is parsed; the
// rest is counted, as a rough size, and never decoded.
func codexSessions(home, workspaceDir string) []Recorded {
	root := filepath.Join(home, ".codex", "sessions")
	var out []Recorded
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		var head struct {
			Payload struct {
				ID        string    `json:"id"`
				CWD       string    `json:"cwd"`
				Timestamp time.Time `json:"timestamp"`
			} `json:"payload"`
		}
		lines, ours := 0, false
		// A read error keeps the count so far: it is a size for a label.
		_ = jsonl.Scan(f, func(line []byte) bool {
			lines++
			if lines > 1 {
				return true
			}
			ours = json.Unmarshal(line, &head) == nil && head.Payload.ID != "" &&
				Under(head.Payload.CWD, workspaceDir)
			return ours
		})
		if !ours {
			return nil
		}
		last := head.Payload.Timestamp
		if fi, err := d.Info(); err == nil && fi.ModTime().After(last) {
			// The header timestamp is when the session STARTED; the file's mtime
			// is when it was last written, which is what "most recent" should mean.
			last = fi.ModTime()
		}
		out = append(out, Recorded{ID: head.Payload.ID, Messages: lines - 1, LastActivity: last, Dir: head.Payload.CWD})
		return nil
	})
	return out
}

// --- antigravity ---------------------------------------------------------------

// Antigravity keeps its conversations in a SQLite database. It is read through
// the sqlite3 CLI in read-only mode rather than by linking a driver: this is one
// small query, and a cgo dependency for it would be out of proportion — and a
// read-only URI cannot disturb a running agy.
func antigravitySessions(home, workspaceDir string) []Recorded {
	db := filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db")
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		return nil
	}
	const q = `SELECT conversation_id, title, step_count, last_modified_time, workspace_uris
	           FROM conversation_summaries WHERE killed = 0 ORDER BY last_modified_time DESC LIMIT 200;`
	cmd := exec.Command(bin, "-readonly", "-separator", "\x1f", db, q)
	raw, err := cmd.Output()
	if err != nil {
		return nil
	}
	var out []Recorded
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 5 {
			continue
		}
		// workspace_uris holds one or more paths, sometimes as file:// URIs.
		if !workspaceMatches(f[4], workspaceDir) {
			continue
		}
		r := Recorded{ID: f[0], Title: f[1]}
		r.Messages, _ = strconv.Atoi(f[2])
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, f[3]); err == nil {
				r.LastActivity = t
				break
			}
		}
		out = append(out, r)
	}
	return out
}

func workspaceMatches(uris, workspaceDir string) bool {
	for _, part := range strings.FieldsFunc(uris, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '"' || r == '[' || r == ']'
	}) {
		p := strings.TrimSpace(strings.TrimPrefix(part, "file://"))
		if p != "" && Under(p, workspaceDir) {
			return true
		}
	}
	return false
}

// GuideFile is the filename an agent reads for project instructions at the
// workspace root. Claude Code looks for CLAUDE.md; Codex and Antigravity both
// read AGENTS.md, which is what their binaries reference.
func GuideFile(k Kind) string {
	if Normalize(string(k)) == Claude {
		return "CLAUDE.md"
	}
	return "AGENTS.md"
}

// GuideFiles is every name this tool might have written, so removing a
// workspace cleans up whichever one it created.
func GuideFiles() []string { return []string{"CLAUDE.md", "AGENTS.md"} }

// Transcript is one Claude Code transcript file and the working directory it
// was recorded in.
type Transcript struct {
	Path string
	Cwd  string
}

// ClaudeTranscripts lists the transcript files recorded in a workspace or any
// folder inside it — what has to travel with the workspace for a conversation
// to be resumable somewhere else.
func ClaudeTranscripts(home, workspaceDir string) []Transcript {
	projects := filepath.Join(home, ".claude", "projects")
	des, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	var out []Transcript
	for _, de := range des {
		if !de.IsDir() || !ClaudeHolds(de.Name(), workspaceDir) {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(projects, de.Name(), "*.jsonl"))
		for _, f := range files {
			if cwd := transcriptCwd(f); cwd != "" && Under(cwd, workspaceDir) {
				out = append(out, Transcript{Path: f, Cwd: cwd})
			}
		}
	}
	return out
}

// transcriptCwd is the first working directory a transcript records.
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// A transcript unreadable before its first cwd is left out, which leaves
	// it where it is rather than taking it anywhere.
	cwd := ""
	_ = jsonl.Scan(f, func(line []byte) bool {
		var e struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(line, &e) == nil && e.CWD != "" {
			cwd = e.CWD
		}
		return cwd == ""
	})
	return cwd
}

// ClaudeProjectDir is where Claude Code keeps the transcripts of conversations
// started in cwd.
func ClaudeProjectDir(home, cwd string) string {
	return filepath.Join(home, ".claude", "projects", claudeEncode(filepath.Clean(cwd)))
}
