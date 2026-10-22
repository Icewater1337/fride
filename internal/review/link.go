package review

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
)

// Trace is the agent edit a hunk is attributed to.
type Trace struct {
	Session   string    `json:"session"`
	Time      time.Time `json:"time"`
	Tool      string    `json:"tool"`
	Intent    string    `json:"intent,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Sidechain bool      `json:"sidechain,omitempty"`
	// Edits counts every agent edit that wrote some of the hunk's lines. More
	// than one means the agent came back to it.
	Edits int `json:"edits"`
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

// linkHunks attributes each hunk with added lines to the agent edit that
// wrote most of them.
//
// This is a text match after the fact, not recorded provenance. An edit
// claims a hunk only if at least half of the hunk's distinctive added lines
// appear verbatim (whitespace trimmed) in the edit's new text. Among edits
// that qualify, the one with the most matching lines wins, and the later one
// on a tie, because it is the edit that left the text as it is now. A line
// the agent wrote through a shell command (sed, a generator) matches nothing,
// so its hunk stays unattributed rather than being credited to a guess.
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
			var want []string
			for _, l := range h.Lines {
				if strings.HasPrefix(l, "+") && significant(l[1:]) {
					want = append(want, strings.TrimSpace(l[1:]))
				}
			}
			if len(want) == 0 {
				continue
			}
			best, bestN, touching := -1, 0, 0
			for _, ci := range cands {
				set := lineSet(ci)
				n := 0
				for _, w := range want {
					if set[w] {
						n++
					}
				}
				if n == 0 {
					continue
				}
				touching++
				if n*2 >= len(want) && n >= bestN {
					best, bestN = ci, n
				}
			}
			if best < 0 {
				continue
			}
			e := edits[best]
			h.Trace = &Trace{Session: e.Session, Time: e.Time, Tool: e.Tool, Intent: e.Intent,
				Prompt: e.Prompt, Sidechain: e.Sidechain, Edits: touching}
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
