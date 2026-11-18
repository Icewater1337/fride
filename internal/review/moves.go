package review

import (
	"sort"
	"strings"
	"unicode"
)

// Move is a run of a hunk's lines that the same change also has on the other
// side, somewhere else: code that moved rather than changed.
type Move struct {
	Line int `json:"line"` // index into Hunk.Lines of the run's first line
	N    int `json:"n"`
	// Path and At say where the other side is: for added lines where they
	// came from (an old line number), for removed lines where they went (a
	// new line number).
	Path string `json:"path"`
	At   int    `json:"at"`
}

// A run has to be this long to count as moved. Below it, a matching
// "return nil" and a brace are a coincidence. Git's --color-moved uses the
// same alphanumeric floor.
const (
	minMoveLines   = 3
	minMoveAlnum   = 20
	maxMoveSources = 64
)

type lineAt struct {
	file, hunk, idx int
	no              int // the line's number on its own side
}

// detectMoves finds blocks that one file of a repository lost and another
// place gained, and folds a hunk that only moves code. Lines compare as in
// whitespaceOnly: indentation that is syntax, spaces inside a string and the
// space between two words count, other whitespace does not. Each side keys by
// its own file, so a block moved between an indent-sensitive file and another
// one is not a move.
//
// A match inside one hunk is not a move: that is a block re-indented where it
// stands, and the reader has to see what wraps it now.
//
// ponytail: line runs, not syntax. A function moved and edited in the middle
// shows as two moved runs around a change; tree-sitter would see one function.
func detectMoves(files []File) {
	removed := map[string][]lineAt{}
	for fi := range files {
		sens := wsRulesFor(files[fi].Path)
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			old, _ := lineNumbers(h)
			for li, l := range h.Lines {
				if l != "" && l[0] == '-' {
					if k := squashLine(l[1:], sens); k != "" {
						removed[k] = append(removed[k], lineAt{fi, hi, li, old[li]})
					}
				}
			}
		}
	}
	if len(removed) == 0 {
		return
	}
	// Every removed line is the source of at most one move, and every added
	// line the end of at most one: take the longest candidates first and mark
	// what they use. A further copy of the same block is new code.
	type cand struct {
		fi, hi, li, n int
		src           lineAt
	}
	var cands []cand
	for fi := range files {
		sens := wsRulesFor(files[fi].Path)
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			for li, l := range h.Lines {
				// A run starts only at a distinctive line: a brace removed in
				// a thousand places would make every pair a candidate.
				if l == "" || l[0] != '+' || !significant(l[1:]) {
					continue
				}
				sources := removed[squashLine(l[1:], sens)]
				if len(sources) > maxMoveSources {
					continue
				}
				for _, src := range sources {
					if src.file == fi && src.hunk == hi {
						continue
					}
					if n := runLength(files, fi, hi, li, src); n >= minMoveLines && alnum(h.Lines[li:li+n]) >= minMoveAlnum {
						cands = append(cands, cand{fi, hi, li, n, src})
					}
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
	used := map[[3]int]bool{} // {file, hunk, line}, on either side
	free := func(f, h, l, n int) bool {
		for i := l; i < l+n; i++ {
			if used[[3]int{f, h, i}] {
				return false
			}
		}
		return true
	}
	for _, c := range cands {
		if !free(c.fi, c.hi, c.li, c.n) || !free(c.src.file, c.src.hunk, c.src.idx, c.n) {
			continue
		}
		for i := 0; i < c.n; i++ {
			used[[3]int{c.fi, c.hi, c.li + i}] = true
			used[[3]int{c.src.file, c.src.hunk, c.src.idx + i}] = true
		}
		h, srcHunk := &files[c.fi].Hunks[c.hi], &files[c.src.file].Hunks[c.src.hunk]
		_, nu := lineNumbers(h)
		h.Moves = append(h.Moves, Move{Line: c.li, N: c.n, Path: files[c.src.file].Path, At: c.src.no})
		srcHunk.Moves = append(srcHunk.Moves, Move{Line: c.src.idx, N: c.n, Path: files[c.fi].Path, At: nu[c.li]})
	}
	for fi := range files {
		for hi := range files[fi].Hunks {
			m := files[fi].Hunks[hi].Moves
			sort.Slice(m, func(i, j int) bool { return m[i].Line < m[j].Line })
		}
	}
	for fi := range files {
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			if h.Mechanical == "" && len(h.Moves) > 0 && allMoved(h) {
				h.Mechanical = "moved"
			}
		}
	}
}

// runLength counts how many added lines from li on match removed lines from
// src on, one for one.
func runLength(files []File, fi, hi, li int, src lineAt) int {
	add := files[fi].Hunks[hi].Lines
	del := files[src.file].Hunks[src.hunk].Lines
	addSens, delSens := wsRulesFor(files[fi].Path), wsRulesFor(files[src.file].Path)
	n := 0
	for li+n < len(add) && src.idx+n < len(del) {
		a, d := add[li+n], del[src.idx+n]
		if a == "" || d == "" || a[0] != '+' || d[0] != '-' || squashLine(a[1:], addSens) != squashLine(d[1:], delSens) {
			break
		}
		n++
	}
	return n
}

func alnum(lines []string) int {
	n := 0
	for _, l := range lines {
		for _, r := range l {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				n++
			}
		}
	}
	return n
}

// allMoved reports a hunk whose every changed line belongs to a move, or is
// blank.
func allMoved(h *Hunk) bool {
	in := make([]bool, len(h.Lines))
	for _, m := range h.Moves {
		for i := m.Line; i < m.Line+m.N && i < len(in); i++ {
			in[i] = true
		}
	}
	for i, l := range h.Lines {
		if l != "" && l[0] != ' ' && !in[i] && strings.TrimSpace(l[1:]) != "" {
			return false
		}
	}
	return true
}

// lineNumbers gives each line of a hunk its old and new line number. A line
// that does not exist on a side gets the number of the next one there.
func lineNumbers(h *Hunk) (old, nu []int) {
	old, nu = make([]int, len(h.Lines)), make([]int, len(h.Lines))
	o, n := h.OldStart, h.NewStart
	for i, l := range h.Lines {
		old[i], nu[i] = o, n
		if l == "" || l[0] != '+' {
			o++
		}
		if l == "" || l[0] != '-' {
			n++
		}
	}
	return old, nu
}
