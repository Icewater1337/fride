package review

import "unicode"

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
// place gained, whitespace aside, and folds a hunk that only moves code.
//
// A match inside one hunk is not a move: that is a block re-indented where it
// stands, and the reader has to see what wraps it now.
//
// ponytail: line runs, not syntax. A function moved and edited in the middle
// shows as two moved runs around a change; tree-sitter would see one function.
func detectMoves(files []File) {
	removed := map[string][]lineAt{}
	for fi := range files {
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			old, _ := lineNumbers(h)
			for li, l := range h.Lines {
				if l != "" && l[0] == '-' {
					if k := squash(l[1:]); k != "" {
						removed[k] = append(removed[k], lineAt{fi, hi, li, old[li]})
					}
				}
			}
		}
	}
	if len(removed) == 0 {
		return
	}
	for fi := range files {
		for hi := range files[fi].Hunks {
			h := &files[fi].Hunks[hi]
			_, nu := lineNumbers(h)
			for li := 0; li < len(h.Lines); {
				best, bestN := lineAt{}, 0
				// A run starts only at a distinctive line: a brace removed in
				// a thousand places would make every pair a candidate.
				if l := h.Lines[li]; l != "" && l[0] == '+' && significant(l[1:]) && len(removed[squash(l[1:])]) <= maxMoveSources {
					for _, src := range removed[squash(l[1:])] {
						if src.file == fi && src.hunk == hi {
							continue
						}
						if n := runLength(files, fi, hi, li, src); n > bestN {
							best, bestN = src, n
						}
					}
				}
				if bestN < minMoveLines || alnum(h.Lines[li:li+bestN]) < minMoveAlnum {
					li++
					continue
				}
				srcHunk := &files[best.file].Hunks[best.hunk]
				h.Moves = append(h.Moves, Move{Line: li, N: bestN, Path: files[best.file].Path, At: best.no})
				srcHunk.Moves = append(srcHunk.Moves, Move{Line: best.idx, N: bestN, Path: files[fi].Path, At: nu[li]})
				li += bestN
			}
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
	n := 0
	for li+n < len(add) && src.idx+n < len(del) {
		a, d := add[li+n], del[src.idx+n]
		if a == "" || d == "" || a[0] != '+' || d[0] != '-' || squash(a[1:]) != squash(d[1:]) {
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
		if l != "" && l[0] != ' ' && !in[i] && squash(l[1:]) != "" {
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
