package review

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// Trace is one agent edit that wrote some of a hunk's lines.
type Trace struct {
	Session   string    `json:"session"`
	Time      time.Time `json:"time"`
	Tool      string    `json:"tool"`
	Intent    string    `json:"intent,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Sidechain bool      `json:"sidechain,omitempty"`
	// Lines counts the hunk's distinctive added lines credited to this edit.
	Lines int `json:"lines"`
}

// significant drops lines too generic to tell one edit from another: a lone
// brace or "return nil" appears in almost every edit of a file.
func significant(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return false
	}
	return strings.ContainsFunc(s, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
	})
}

// linkHunks credits each hunk's added lines to the agent edits that wrote them.
//
// This is a text match after the fact, not recorded provenance. Each
// distinctive added line (whitespace trimmed) goes to the latest edit of the
// same file whose new text contains it: that is the edit that left the line as
// it is now. A hunk then lists every edit that got at least one of its lines,
// most lines first, and how many of its lines no edit accounts for. One hunk
// is often the work of several edits (a change, then a fix of it, or a
// subagent's addition next to it), so crediting only one would misreport it.
// A line written through a shell command (sed, a generator) matches nothing
// and stays unaccounted, rather than being credited to a guess.
func linkHunks(dir string, files []File, edits []agent.Edit) {
	if len(edits) == 0 {
		return
	}
	byFile := map[string][]int{}
	for i, e := range edits {
		p := filepath.Clean(e.File)
		byFile[p] = append(byFile[p], i)
	}
	// Each edit's lines, as a set, built once per edit that is looked at.
	sets := map[int]map[string]bool{}
	lineSet := func(i int) map[string]bool {
		if s, ok := sets[i]; ok {
			return s
		}
		s := map[string]bool{}
		for _, l := range strings.Split(edits[i].Text, "\n") {
			if significant(l) {
				s[strings.TrimSpace(l)] = true
			}
		}
		sets[i] = s
		return s
	}

	for fi := range files {
		f := &files[fi]
		cands := byFile[filepath.Join(dir, filepath.FromSlash(f.Path))]
		if len(cands) == 0 {
			continue
		}
		for hi := range f.Hunks {
			h := &f.Hunks[hi]
			credit := map[int]int{}
			wanted := 0
			for _, l := range h.Lines {
				if !strings.HasPrefix(l, "+") || !significant(l[1:]) {
					continue
				}
				wanted++
				w := strings.TrimSpace(l[1:])
				best := -1
				for _, ci := range cands {
					if lineSet(ci)[w] && (best < 0 || !edits[ci].Time.Before(edits[best].Time)) {
						best = ci
					}
				}
				if best >= 0 {
					credit[best]++
				}
			}
			got := 0
			for ei, n := range credit {
				e := edits[ei]
				h.Traces = append(h.Traces, Trace{Session: e.Session, Time: e.Time, Tool: e.Tool,
					Intent: e.Intent, Prompt: e.Prompt, Sidechain: e.Sidechain, Lines: n})
				got += n
			}
			sort.Slice(h.Traces, func(i, j int) bool {
				a, b := h.Traces[i], h.Traces[j]
				if a.Lines != b.Lines {
					return a.Lines > b.Lines
				}
				return a.Time.Before(b.Time)
			})
			if len(h.Traces) > 0 {
				h.Unattributed = wanted - got
			}
		}
	}
}

// editsPerFile counts the agent's edits to each file of a repo, by path
// relative to the repo.
func editsPerFile(dir string, edits []agent.Edit) map[string]int {
	out := map[string]int{}
	for _, e := range edits {
		rel, err := filepath.Rel(dir, filepath.Clean(e.File))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out[filepath.ToSlash(rel)]++
	}
	return out
}
