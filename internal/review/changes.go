package review

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Icewater1337/fride/internal/files"
)

// The diff of one task is read in full on every refresh of the Review tab, and
// a browser tab chokes long before git does. Past these caps a file shows its
// stats and a note instead of its lines; it can still be opened whole.
const (
	maxFileLines  = 3000
	maxTotalLines = 40000
	// maxNewFileBytes caps reading an untracked file to show it as added. A
	// stray build artefact the agent forgot to ignore should not stall a refresh.
	maxNewFileBytes = 1 << 20
)

// File is one changed path of a repository, as the review shows it.
type File struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"` // set for a rename or copy
	// Status is git's name-status letter: A, M, D, R, C or T. An untracked file
	// is reported as A: to a reviewer it is a new file, staged or not.
	Status    string `json:"status"`
	Untracked bool   `json:"untracked,omitempty"`
	Added     int    `json:"added"`
	Deleted   int    `json:"deleted"`
	Binary    bool   `json:"binary,omitempty"`
	// Hash identifies this version of the file's change. A viewed mark is valid
	// only while it still matches; see Viewed.
	Hash string `json:"hash"`
	// Noise says why a file is folded away by default: "generated", "vendored"
	// or "lockfile". It is never hidden, only folded, and NoiseWhy is shown
	// so the reviewer can disagree.
	Noise    string `json:"noise,omitempty"`
	NoiseWhy string `json:"noiseWhy,omitempty"`
	// Layer is where the file sits in the tour: see Layers.
	Layer     string `json:"layer"`
	Hunks     []Hunk `json:"hunks"`
	Truncated bool   `json:"truncated,omitempty"`
	// Withheld marks a file that holds credentials (.env, keys): its lines are
	// not served, for the reason files.Secretish gives.
	Withheld bool `json:"withheld,omitempty"`
	// Viewed is filled in from the reviewer's marks: "" (not yet), "viewed",
	// or "changed" (viewed once, changed since).
	Viewed string `json:"viewed,omitempty"`
}

// Hunk is one @@ block of a file's diff.
type Hunk struct {
	// ID hashes the path and the hunk's lines, not its position: an id of
	// "path#n" shifts whenever an earlier hunk of the file changes, and takes
	// every note and link attached to it along.
	ID       string `json:"id"`
	Header   string `json:"header"` // the text after the closing @@, usually the enclosing function
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	// Lines keep git's one-character prefix: '+', '-' or ' '. A "\ No newline
	// at end of file" marker is dropped; it is not a line of the file.
	Lines []string `json:"lines"`
	// Mechanical marks a hunk that changes nothing a reader needs to check:
	// "whitespace" when every change is whitespace only, "moved" when every
	// changed line is code moved from or to elsewhere in the change.
	Mechanical string `json:"mechanical,omitempty"`
	// Moves are the runs of lines that moved; see detectMoves.
	Moves []Move `json:"moves,omitempty"`
	// Prov has one mark per line: who wrote each added line. See provenance.
	Prov string `json:"prov,omitempty"`
	// Seen marks a hunk of a changed file that the reviewer already read in
	// the version they marked viewed.
	Seen bool `json:"seen,omitempty"`
	// Traces are the agent edits that wrote the hunk's added lines, most
	// lines first; see linkHunks. Unattributed counts the distinctive added
	// lines none of them accounts for, and is only set when Traces is not empty.
	Traces       []Trace `json:"traces,omitempty"`
	Unattributed int     `json:"unattributed,omitempty"`
}

// RepoChanges is everything one repository of the task changed since its base.
type RepoChanges struct {
	Repo      string `json:"repo"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base"`
	MergeBase string `json:"mergeBase,omitempty"`
	Head      string `json:"head,omitempty"`
	Files     []File `json:"files"`
	Err       string `json:"err,omitempty"`
}

// lineBudget is shared by every file of every repo in one request, so the cap
// is on what the browser receives, not on each repo separately.
type lineBudget struct{ left int }

func newBudget() *lineBudget { return &lineBudget{left: maxTotalLines} }

// Diff reads one repository's change since base: the working tree against
// merge-base(base, HEAD), which takes in committed, staged and unstaged work,
// plus every untracked file.
//
// Against the merge-base rather than the base tip, as workspace.Stats counts:
// the base may have moved on since the worktree was made, and its new commits
// would otherwise show up as the agent deleting them.
func Diff(dir, repo, base string, budget *lineBudget) RepoChanges {
	rc := RepoChanges{Repo: repo, Base: base, Files: []File{}}
	if b, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		rc.Branch = b
	}
	if h, err := git(dir, "rev-parse", "HEAD"); err == nil {
		rc.Head = h
	}
	mb, err := git(dir, "merge-base", base, "HEAD")
	if err != nil || mb == "" {
		rc.Err = "cannot resolve the base " + base
		return rc
	}
	rc.MergeBase = mb

	// The file list comes from --name-status -z, whose paths are exact, and the
	// lines from the patch, whose "diff --git a/x b/y" header is ambiguous
	// once a path contains a space. Both list files in the same order, so the
	// two are zipped. -M on both, or they would disagree about renames.
	ns, err := gitRaw(dir, "-c", "core.quotepath=off", "diff", "--no-color", "--no-ext-diff",
		"-M", "--name-status", "-z", mb)
	if err != nil {
		rc.Err = err.Error()
		return rc
	}
	entries := parseNameStatus(ns)
	patch, err := gitRaw(dir, "-c", "core.quotepath=off", "diff", "--no-color", "--no-ext-diff",
		"-M", "--src-prefix=a/", "--dst-prefix=b/", mb)
	if err != nil {
		rc.Err = err.Error()
		return rc
	}
	sections := splitPatch(patch)
	if len(sections) != len(entries) {
		rc.Err = fmt.Sprintf("git listed %d changed files but its patch has %d", len(entries), len(sections))
		return rc
	}

	attrs := noiseAttrs(dir, entries)
	for i, e := range entries {
		f := File{Path: e.path, OldPath: e.oldPath, Status: e.status}
		parseSection(&f, sections[i], budget)
		f.Hash = hashOf(sections[i])
		withhold(&f)
		classifyNoise(&f, attrs[f.Path])
		rc.Files = append(rc.Files, f)
	}

	// Untracked files are invisible to git diff, and a new file is often the
	// heart of what an agent wrote.
	if others, err := gitRaw(dir, "-c", "core.quotepath=off", "ls-files", "--others",
		"--exclude-standard", "-z"); err == nil {
		var news []nameStatus
		for _, p := range strings.Split(others, "\x00") {
			if p != "" {
				news = append(news, nameStatus{status: "A", path: p})
			}
		}
		attrs := noiseAttrs(dir, news)
		for _, e := range news {
			f := untrackedFile(dir, e.path, budget)
			withhold(&f)
			classifyNoise(&f, attrs[f.Path])
			rc.Files = append(rc.Files, f)
		}
	}
	return rc
}

type nameStatus struct{ status, path, oldPath string }

// withhold drops the lines of a file that holds credentials, keeping its
// counts. The API is unauthenticated on loopback, and a diff of a new .env is
// the whole file: the file browser refuses to serve it, so the review must
// not serve it either.
func withhold(f *File) {
	if !files.Secretish(f.Path) {
		return
	}
	f.Withheld = true
	f.Hunks = []Hunk{}
}

// parseNameStatus reads `git diff --name-status -z`: a status field, then one
// path, or two for a rename or copy (old, then new). R and C carry a score
// ("R087") that is not needed here.
func parseNameStatus(s string) []nameStatus {
	f := strings.Split(s, "\x00")
	var out []nameStatus
	for i := 0; i < len(f); {
		st := f[i]
		if st == "" {
			i++
			continue
		}
		code := st[:1]
		if (code == "R" || code == "C") && i+2 < len(f) {
			out = append(out, nameStatus{status: code, oldPath: f[i+1], path: f[i+2]})
			i += 3
			continue
		}
		if i+1 >= len(f) {
			break
		}
		out = append(out, nameStatus{status: code, path: f[i+1]})
		i += 2
	}
	return out
}

// splitPatch cuts a multi-file patch at each "diff --git " header.
func splitPatch(p string) []string {
	if p == "" {
		return nil
	}
	var out []string
	lines := strings.SplitAfter(p, "\n")
	var cur strings.Builder
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git ") && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(l)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseSection fills in a file's hunks and counts from its part of the patch.
func parseSection(f *File, sec string, budget *lineBudget) {
	var cur *Hunk
	inHunks := false
	for _, l := range strings.Split(strings.TrimRight(sec, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunks = true
			h, ok := parseHunkHeader(l)
			if !ok {
				cur = nil
				continue
			}
			f.Hunks = append(f.Hunks, h)
			cur = &f.Hunks[len(f.Hunks)-1]
		case !inHunks:
			if strings.HasPrefix(l, "Binary files ") || l == "GIT binary patch" {
				f.Binary = true
			}
		case cur == nil:
		case strings.HasPrefix(l, `\`):
			// "\ No newline at end of file"
		case len(l) > 0 && (l[0] == '+' || l[0] == '-' || l[0] == ' '):
			if l[0] == '+' {
				f.Added++
			} else if l[0] == '-' {
				f.Deleted++
			}
			cur.Lines = append(cur.Lines, l)
		case l == "":
			// A context line of an empty line, whose leading space an editor or
			// a transport stripped.
			cur.Lines = append(cur.Lines, " ")
		}
	}
	finishHunks(f, budget)
}

// finishHunks gives each hunk its id and class, and applies the line caps.
func finishHunks(f *File, budget *lineBudget) {
	n := 0
	for i := range f.Hunks {
		n += len(f.Hunks[i].Lines)
	}
	if n > maxFileLines || n > budget.left {
		f.Truncated = true
		f.Hunks = []Hunk{}
		return
	}
	budget.left -= n
	for i := range f.Hunks {
		h := &f.Hunks[i]
		h.ID = hunkID(f.Path, h.Lines)
		if whitespaceOnly(h.Lines) {
			h.Mechanical = "whitespace"
		}
	}
	if f.Hunks == nil {
		f.Hunks = []Hunk{}
	}
}

// parseHunkHeader reads "@@ -a,b +c,d @@ heading". A count left out means 1.
func parseHunkHeader(l string) (Hunk, bool) {
	rest := strings.TrimPrefix(l, "@@ ")
	end := strings.Index(rest, " @@")
	if end < 0 {
		return Hunk{}, false
	}
	ranges := strings.Fields(rest[:end])
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "-") || !strings.HasPrefix(ranges[1], "+") {
		return Hunk{}, false
	}
	h := Hunk{Header: strings.TrimSpace(rest[end+3:]), Lines: []string{}}
	var ok1, ok2 bool
	h.OldStart, h.OldLines, ok1 = parseRange(ranges[0][1:])
	h.NewStart, h.NewLines, ok2 = parseRange(ranges[1][1:])
	return h, ok1 && ok2
}

func parseRange(s string) (start, count int, ok bool) {
	a, b, has := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if has {
		if count, err = strconv.Atoi(b); err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

func hunkID(p string, lines []string) string {
	h := sha1.New()
	h.Write([]byte(p))
	for _, l := range lines {
		h.Write([]byte{0})
		h.Write([]byte(l))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func hashOf(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// whitespaceOnly reports a hunk whose removed and added lines are the same
// once whitespace is ignored, taken in order. A hunk that only adds or only
// removes blank lines counts too. Anything else, even a single moved
// character, is left for the reader.
func whitespaceOnly(lines []string) bool {
	var minus, plus strings.Builder
	changed := false
	for _, l := range lines {
		if l == "" {
			continue
		}
		switch l[0] {
		case '-':
			changed = true
			minus.WriteString(squash(l[1:]))
		case '+':
			changed = true
			plus.WriteString(squash(l[1:]))
		}
	}
	return changed && minus.String() == plus.String()
}

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// untrackedFile builds the review of a file git does not know yet: all added.
func untrackedFile(dir, rel string, budget *lineBudget) File {
	f := File{Path: rel, Status: "A", Untracked: true, Hunks: []Hunk{}}
	full := filepath.Join(dir, rel)
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		// A symlink or a vanished file: report it without reading through it.
		f.Hash = hashOf("untracked\x00" + rel)
		return f
	}
	if fi.Size() > maxNewFileBytes {
		f.Truncated = true
		f.Hash = hashOf(fmt.Sprintf("untracked\x00%s\x00%d\x00%d", rel, fi.Size(), fi.ModTime().UnixNano()))
		return f
	}
	b, err := os.ReadFile(full)
	if err != nil {
		f.Hash = hashOf("untracked\x00" + rel)
		return f
	}
	f.Hash = hashOf("untracked\x00" + rel + "\x00" + string(b))
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		f.Binary = true
		return f
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" && len(b) == 0 {
		return f // an empty new file has no lines to show
	}
	lines := strings.Split(text, "\n")
	h := Hunk{OldStart: 0, OldLines: 0, NewStart: 1, NewLines: len(lines), Lines: make([]string, len(lines))}
	for i, l := range lines {
		h.Lines[i] = "+" + l
	}
	f.Added = len(lines)
	f.Hunks = []Hunk{h}
	finishHunks(&f, budget)
	return f
}

// lockfiles are regenerated by tools, never written by hand, and their diff
// is unreadable. A dependency change is surfaced by the manifest beside them.
var lockfiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true,
	"go.sum": true, "Cargo.lock": true, "poetry.lock": true, "Pipfile.lock": true,
	"composer.lock": true, "Gemfile.lock": true, "uv.lock": true, "gradle.lockfile": true,
	"npm-shrinkwrap.json": true, "flake.lock": true, "mix.lock": true, "pubspec.lock": true,
}

// vendorDirs are third-party code checked in as-is.
var vendorDirs = []string{"vendor/", "node_modules/", "third_party/", "third-party/"}

// classifyNoise folds files nobody reads line by line. The attribute an
// owner set in .gitattributes wins over the guesses by name.
func classifyNoise(f *File, attr string) {
	switch attr {
	case "generated":
		f.Noise, f.NoiseWhy = "generated", "marked linguist-generated in .gitattributes"
		return
	case "vendored":
		f.Noise, f.NoiseWhy = "vendored", "marked linguist-vendored in .gitattributes"
		return
	}
	base := path.Base(f.Path)
	if lockfiles[base] {
		f.Noise, f.NoiseWhy = "lockfile", base+" is written by a tool"
		return
	}
	for _, d := range vendorDirs {
		if strings.HasPrefix(f.Path, d) || strings.Contains(f.Path, "/"+d) {
			f.Noise, f.NoiseWhy = "vendored", "inside "+strings.TrimSuffix(d, "/")+"/"
			return
		}
	}
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") ||
		strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, "_pb2.py") ||
		strings.Contains(base, ".generated.") || strings.HasSuffix(base, "_generated.go") {
		f.Noise, f.NoiseWhy = "generated", "named like generated code"
	}
}

// noiseAttrs asks git which files the repository itself marks as generated or
// vendored. One check-attr call covers every file.
func noiseAttrs(dir string, entries []nameStatus) map[string]string {
	out := map[string]string{}
	if len(entries) == 0 {
		return out
	}
	var in strings.Builder
	for _, e := range entries {
		in.WriteString(e.path)
		in.WriteByte(0)
	}
	cmd := exec.Command("git", "check-attr", "-z", "--stdin",
		"linguist-generated", "gitlab-generated", "linguist-vendored")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(in.String())
	b, err := cmd.Output()
	if err != nil {
		return out // no attributes is the safe answer: nothing gets folded
	}
	// -z output: path NUL attribute NUL value NUL, repeated.
	f := strings.Split(string(b), "\x00")
	for i := 0; i+2 < len(f); i += 3 {
		p, attr, val := f[i], f[i+1], f[i+2]
		if val != "set" && val != "true" {
			continue
		}
		if attr == "linguist-vendored" {
			if out[p] == "" {
				out[p] = "vendored"
			}
		} else {
			out[p] = "generated"
		}
	}
	return out
}

func git(dir string, args ...string) (string, error) {
	s, err := gitRaw(dir, args...)
	return strings.TrimSpace(s), err
}

// gitRaw keeps leading whitespace: a patch's context line starts with a space.
func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}
