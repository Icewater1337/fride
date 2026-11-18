// Package journal records the reasoning behind a task while the work is
// happening: the plan, the decisions taken, the assumptions made, the issues
// hit along the way, and the follow-up work found but left for later.
//
// Why this is written live rather than reconstructed afterwards: a session
// transcript reliably records what HAPPENED — files touched, commands run,
// errors hit — but almost never why. An agent weighs a choice internally and
// then acts, so the reason exists only in that moment. Reconstructing it later
// means an LLM inferring a plausible-sounding justification, which is exactly
// the thing this document must not contain. Facts are mined at close-out;
// rationale is only ever recorded, never inferred.
//
// One append-only file per workspace, not one per agent. Entries are
// timestamped and carry their source, so several agents writing concurrently
// merge by ordering rather than needing a merge step at all.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/jsonl"
	"golang.org/x/sys/unix"
)

// Kind classifies an entry. These are the things worth knowing later.
type Kind string

const (
	KindPlan       Kind = "plan"       // what we set out to do
	KindDecision   Kind = "decision"   // a choice taken, with its reason
	KindAssumption Kind = "assumption" // something taken as true without confirming
	KindIssue      Kind = "issue"      // a problem hit during implementation
	KindNote       Kind = "note"       // anything else worth remembering
	// KindFollowup is work found but deliberately not done in this task: a
	// second bug next door, a flaky test, a part of the brief left out. An
	// issue is history; a follow-up is a to-do for someone else, so it is
	// carried out of the task (the rationale document, the companion's done
	// event) in a shape that can become a new ticket. Its Text is the title.
	KindFollowup Kind = "followup"
)

func ValidKind(k Kind) bool {
	switch k {
	case KindPlan, KindDecision, KindAssumption, KindIssue, KindNote, KindFollowup:
		return true
	}
	return false
}

func Kinds() []Kind {
	return []Kind{KindPlan, KindDecision, KindAssumption, KindIssue, KindNote, KindFollowup}
}

// Sources with a meaning beyond a free-form label. A choice the user made in
// conversation and a choice the agent made alone are different kinds of
// evidence — the first settles the question, the second invites review — so the
// finished document has to be able to tell them apart. Any other string is
// still accepted; these two are the ones that are rendered specially.
const (
	SourceAgent = "agent"
	SourceUser  = "user"
)

type Entry struct {
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	Text string    `json:"text"`
	// Why is the rationale. It is the whole point of a decision or assumption
	// entry, and the one thing that cannot be recovered afterwards.
	Why string `json:"why,omitempty"`
	// Repo is the project this concerns, when it concerns only one. Empty means
	// it is about the task as a whole — which is common and fine.
	Repo string `json:"repo,omitempty"`
	// Path is the file or folder this concerns, relative to Repo's root (see
	// ScopePath). Mostly for follow-ups, which outlive the worktree.
	Path string `json:"path,omitempty"`
	// Source records who wrote it, so a human note and an agent note are
	// distinguishable in the finished document.
	Source string `json:"source,omitempty"`
}

func dir(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.StateDir, "journal", slug)
}

// Path is the workspace's journal file. It lives outside the worktrees, like
// logs and pids, so recording a decision never dirties `git status`.
func Path(cfg *config.Config, slug string) string {
	return filepath.Join(dir(cfg, slug), "entries.jsonl")
}

// Append adds one entry. Concurrent writers are serialised with a lock, since
// several agents in one workspace may record at the same moment.
func Append(cfg *config.Config, slug string, e Entry) error {
	if !ValidKind(e.Kind) {
		return fmt.Errorf("unknown kind %q (want one of %v)", e.Kind, Kinds())
	}
	if strings.TrimSpace(e.Text) == "" {
		return fmt.Errorf("an entry needs some text")
	}
	// A follow-up is read by someone who was not there, whoever files it as a
	// new ticket, possibly a program: a bare title is not enough to act on.
	if e.Kind == KindFollowup && strings.TrimSpace(e.Why) == "" {
		return fmt.Errorf("a follow-up needs a why (--why): what was found, and why it was left out")
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	// Refuse an entry Read could not return before anything is created on
	// disk. jsonl.Scan counts the newline toward MaxLine, so reserve space
	// for it.
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) >= jsonl.MaxLine {
		return fmt.Errorf("entry is %d MiB, over the %d MiB limit", len(b)>>20, jsonl.MaxLine>>20)
	}
	if err := os.MkdirAll(dir(cfg, slug), 0o755); err != nil {
		return err
	}

	lock, err := os.OpenFile(Path(cfg, slug)+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	f, err := os.OpenFile(Path(cfg, slug), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns every entry, oldest first.
func Read(cfg *config.Config, slug string) ([]Entry, error) {
	f, err := os.Open(Path(cfg, slug))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Entry
	// A plan can be a whole document pasted in one entry; jsonl.Scan skips a
	// line too long to keep instead of ending the read there.
	err = jsonl.Scan(f, func(line []byte) bool {
		var e Entry
		if json.Unmarshal(line, &e) == nil { // a corrupt line must not lose the rest of the record
			out = append(out, e)
		}
		return true
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, err
}

// Followups returns the follow-ups recorded for a task, oldest first.
func Followups(cfg *config.Config, slug string) ([]Entry, error) {
	entries, err := Read(cfg, slug)
	var out []Entry
	for _, e := range entries {
		if e.Kind == KindFollowup {
			out = append(out, e)
		}
	}
	return out, err
}

// counted is a journal's counts, for the size and time it had then.
type counted struct {
	size  int64
	mod   time.Time
	kinds map[Kind]int
}

// countCache spares Count a read of a journal that has not changed. The board
// asks for every task's counts on each refresh, every few seconds, and one
// entry can be a whole pasted plan. The journal is only ever appended to, so
// its size and modification time say whether it changed.
var countCache = struct {
	sync.Mutex
	m map[string]counted
}{m: map[string]counted{}}

// Count summarises the record for the UI. The map is the caller's own.
func Count(cfg *config.Config, slug string) map[Kind]int {
	p := Path(cfg, slug)
	fi, err := os.Stat(p)
	if err != nil {
		return map[Kind]int{}
	}
	countCache.Lock()
	c, ok := countCache.m[p]
	countCache.Unlock()
	if !ok || c.size != fi.Size() || !c.mod.Equal(fi.ModTime()) {
		entries, _ := Read(cfg, slug)
		c = counted{size: fi.Size(), mod: fi.ModTime(), kinds: map[Kind]int{}}
		for _, e := range entries {
			c.kinds[e.Kind]++
		}
		countCache.Lock()
		countCache.m[p] = c
		countCache.Unlock()
	}
	out := make(map[Kind]int, len(c.kinds))
	for k, n := range c.kinds {
		out[k] = n
	}
	return out
}

// ScopePath turns the path an agent gives with an entry into a repository and
// a path inside it. roots maps each of the task's repositories to its
// worktree; wd is the directory the agent ran from.
//
// Agents pass whatever path they hold, usually an absolute one into their
// worktree. Whoever acts on a follow-up later needs the repository and the
// path within it instead: by then the worktree is gone, and it may have been
// on another computer. So:
//
//   - with repo given, a relative path is relative to that repository;
//   - otherwise a relative path is taken from wd, like any command-line path;
//   - a path inside one of the worktrees becomes relative to it, and names
//     its repository;
//   - anything else is kept as given: a file that does not exist yet, or one
//     outside the task, is still worth naming.
func ScopePath(roots map[string]string, wd, repo, path string) (string, string, error) {
	if strings.TrimSpace(path) == "" {
		return repo, "", nil
	}
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		if repo != "" {
			if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
				return "", "", fmt.Errorf("path %q leaves the repository %s", path, repo)
			}
			return repo, filepath.ToSlash(p), nil
		}
		p = filepath.Join(wd, p)
	}
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names) // the same answer every time, whatever the map's order
	for _, name := range names {
		rel, ok := within(roots[name], p)
		if !ok {
			continue
		}
		if repo != "" && repo != name {
			return "", "", fmt.Errorf("path %q is in %s, not in %s", path, name, repo)
		}
		if rel == "." {
			rel = ""
		}
		return name, filepath.ToSlash(rel), nil
	}
	return repo, filepath.ToSlash(filepath.Clean(path)), nil
}

// within is p relative to root, when p is root or below it. Both are also
// tried with symlinks resolved: the working directory can come back resolved
// (macOS's /var is /private/var) while the configured workspace root is not,
// or the other way round.
func within(root, p string) (string, bool) {
	roots, ps := []string{root}, []string{p}
	if r := resolved(root); r != root {
		roots = append(roots, r)
	}
	if r := resolved(p); r != p {
		ps = append(ps, r)
	}
	for _, r := range roots {
		for _, q := range ps {
			rel, err := filepath.Rel(r, q)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return rel, true
			}
		}
	}
	return "", false
}

// resolved is p with the symlinks of its deepest existing ancestor resolved:
// a follow-up may name a file that does not exist yet.
func resolved(p string) string {
	rest := ""
	for q := p; ; {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(q)
		if parent == q {
			return p
		}
		rest = filepath.Join(filepath.Base(q), rest)
		q = parent
	}
}

// Dir is where a task's journal lives — outside the workspace, so it has to be
// carried along when a task moves to another computer.
func Dir(cfg *config.Config, slug string) string { return dir(cfg, slug) }
