package review

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/jsonl"
)

// Capture is one agent edit, recorded by a PostToolUse hook as it happened.
//
// The transcripts hold the same edits, but they are the agent's: Claude Code
// prunes them after a while, and they are not where FrIDE keeps a task's
// record. The capture is FrIDE's own, kept with the review and carried by
// export, so the provenance gutter outlives the transcripts.
type Capture struct {
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
	Tool    string    `json:"tool"`
	File    string    `json:"file"`
	// Lines are the new text's lines, whitespace trimmed, blanks left out.
	Lines []string `json:"lines"`
}

const capturesFile = "edits.jsonl"

// RecordEdit handles one PostToolUse hook call: stdin is the event. Any
// failure is silent, as the journal's hook is: a hook must never be the
// reason an edit fails.
func RecordEdit(cfg *config.Config, slugFor func(path string) string) {
	var in struct {
		SessionID string          `json:"session_id"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(b, &in) != nil {
		return
	}
	ed, ok := agent.ParseEdit(in.ToolName, in.ToolInput)
	if !ok {
		return
	}
	// By the file, not the hook's cwd: an agent in the workspace root edits
	// files of every repo, and a file outside the workspaces is no task's.
	slug := slugFor(filepath.Dir(ed.File))
	if slug == "" {
		return
	}
	c := Capture{At: time.Now(), Session: in.SessionID, Tool: ed.Tool, File: ed.File, Lines: textLines(ed.Text)}
	line, err := json.Marshal(c)
	if err != nil {
		return
	}
	if os.MkdirAll(Dir(cfg, slug), 0o755) != nil {
		return
	}
	// One write with O_APPEND: two agents saving at once interleave whole lines.
	f, err := os.OpenFile(filepath.Join(Dir(cfg, slug), capturesFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// loadCaptures reads a task's captured edits. A torn last line is skipped.
func loadCaptures(cfg *config.Config, slug string) []Capture {
	f, err := os.Open(filepath.Join(Dir(cfg, slug), capturesFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Capture
	_ = jsonl.Scan(f, func(line []byte) bool {
		var c Capture
		if json.Unmarshal(line, &c) == nil && c.File != "" {
			out = append(out, c)
		}
		return true
	})
	return out
}

func textLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Provenance marks, one per line of a hunk.
const (
	provAgent = 'a' // an agent edit wrote the line as it stands
	provMixed = 'm' // an agent wrote a line like it, changed since
	provNone  = 'h' // no agent edit accounts for it: by hand, or by a command
)

// maxMixedLines bounds the near-match search per file.
//
// ponytail: a pairwise scan, O(added × agent lines). Past this many agent
// lines a changed line is reported as "no agent edit" instead.
const maxMixedLines = 2000

// provenance gives each added line of every hunk its mark in Hunk.Prov;
// context and removed lines get a space. Both sources count: the hook's
// captures and the transcripts' edits.
func provenance(dir string, files []File, edits []agent.Edit, caps []Capture) {
	if len(edits) == 0 && len(caps) == 0 {
		return // nothing recorded at all: every line would read "by hand", which says nothing
	}
	byFile := map[string][]string{}
	for _, e := range edits {
		p := filepath.Clean(e.File)
		byFile[p] = append(byFile[p], textLines(e.Text)...)
	}
	for _, c := range caps {
		p := filepath.Clean(c.File)
		byFile[p] = append(byFile[p], c.Lines...)
	}
	for fi := range files {
		f := &files[fi]
		lines := byFile[filepath.Join(dir, filepath.FromSlash(f.Path))]
		exact := map[string]bool{}
		for _, l := range lines {
			exact[l] = true
		}
		for hi := range f.Hunks {
			h := &f.Hunks[hi]
			prov := []byte(strings.Repeat(" ", len(h.Lines)))
			for li, l := range h.Lines {
				if l == "" || l[0] != '+' {
					continue
				}
				t := strings.TrimSpace(l[1:])
				switch {
				case t == "":
				case exact[t]:
					prov[li] = provAgent
				case significant(t) && len(lines) <= maxMixedLines && nearAny(t, lines):
					prov[li] = provMixed
				default:
					prov[li] = provNone
				}
			}
			h.Prov = string(prov)
		}
	}
}

// nearAny reports a line that shares most of its text with one of lines: the
// same start and end around a small edit, as when a constant or a name was
// changed by hand after the agent wrote it.
func nearAny(t string, lines []string) bool {
	for _, l := range lines {
		if near(t, l) {
			return true
		}
	}
	return false
}

func near(a, b string) bool {
	n := max(len(a), len(b))
	if n == 0 || min(len(a), len(b))*10 < n*6 {
		return false
	}
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	return (p+s)*10 >= n*6
}
