// Package store persists tasks to a JSON file. Both the server and the CLI use
// it, so every mutation takes an OS-level file lock: `fride stack up` run by
// an agent must not race the UI.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
)

func ValidStatus(s Status) bool {
	return s == StatusOpen || s == StatusInProgress || s == StatusDone
}

// TaskRepo is one repository enrolled in a task.
type TaskRepo struct {
	Name    string `json:"name"`
	BaseRef string `json:"baseRef"` // what the worktree was branched from
	Branch  string `json:"branch"`  // the branch created in the worktree
}

type Task struct {
	Slug    string     `json:"slug"`
	Name    string     `json:"name"`
	Status  Status     `json:"status"`
	Repos   []TaskRepo `json:"repos"`
	Created time.Time  `json:"created"`
	// K is the instance index; 0 means "not yet allocated".
	K int `json:"k"`
	// Agent is the CLI this task's sessions run: "claude", "antigravity" or "codex".
	// Absent on tasks created before the choice existed, which were all Claude
	// Code — see agent.Normalize.
	Agent string `json:"agent,omitempty"`
	// Hidden takes a task off the board without touching anything it owns: the
	// worktrees, journal, branches and any running stack all stay exactly as
	// they are. It is a view filter, not a lifecycle state, which is why it is
	// separate from Status rather than another value of it.
	Hidden bool `json:"hidden,omitempty"`
	// Dangerous makes this task's agent sessions run with every confirmation
	// turned off (the flag differs per agent) instead of its normal mode.
	// Opt-in per task; remembered so extra agent tabs match the first one.
	Dangerous bool `json:"dangerous,omitempty"`
	// Profile is which of config.json's profiles the stack boots with. Empty
	// means the default profile. It takes effect at the next stack up: a
	// running stack keeps what it was started with.
	Profile string `json:"profile,omitempty"`
	// Scopes overrides, per container-backed service, how that dependency is
	// provided: "shared" (one container for every task), "task" (this task's
	// own, on its own port), or "off". Absent means the profile's default. A
	// repo checked out into the workspace always wins — it is run from source.
	Scopes map[string]string `json:"scopes,omitempty"`
	// GitIdentity is the git identity (config.json's git.identities, or the
	// companion's) the task's sessions and FrIDE's git calls for it run as:
	// commit author, credentials, forge token. Empty: none, git's own
	// configuration, which is what every task had before identities existed.
	GitIdentity string `json:"gitIdentity,omitempty"`
}

// slugRe mirrors Claude Code's worktree-name rules, which also keeps the slug
// safe as a directory name and as a git branch component.
var slugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func Slugify(name string) string {
	out := make([]rune, 0, len(name))
	prevDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			out = append(out, r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
			prevDash = false
		default:
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	s := string(out)
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func ValidateSlug(s string) error {
	if s == "" || s == "." || s == ".." {
		return fmt.Errorf("invalid slug %q", s)
	}
	if len(s) > 64 {
		return fmt.Errorf("slug %q too long (max 64)", s)
	}
	if !slugRe.MatchString(s) {
		return fmt.Errorf("invalid slug %q: letters, digits, dots, underscores and dashes only", s)
	}
	return nil
}

type Store struct {
	path string
	// wsRoot is the directory holding one folder per task. The folder is the
	// authority: a task whose folder is gone is gone, and a folder carrying a
	// task.json is a task even if this file has never heard of it.
	wsRoot string
	mu     sync.Mutex
}

func New(stateDir, workspaceRoot string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(stateDir, "tasks.json"), wsRoot: workspaceRoot}, nil
}

// dir is a task's workspace folder.
func (s *Store) dir(slug string) string { return filepath.Join(s.wsRoot, slug) }

// sidecar is where a task describes itself, inside its own workspace. Keeping a
// copy there is what lets a folder be moved, restored from a backup, or simply
// found again after tasks.json is lost — and it is why deleting the folder in
// Finder is enough to delete the task.
func (s *Store) sidecar(slug string) string {
	return filepath.Join(s.dir(slug), ".fride", "task.json")
}

func (s *Store) writeSidecar(t *Task) {
	if s.wsRoot == "" {
		return
	}
	// A task whose folder has gone gets no sidecar rather than a resurrected
	// directory containing nothing but metadata.
	if fi, err := os.Stat(s.dir(t.Slug)); err != nil || !fi.IsDir() {
		return
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.sidecar(t.Slug)), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.sidecar(t.Slug), b, 0o644)
}

func (s *Store) readSidecar(slug string) *Task {
	b, err := os.ReadFile(s.sidecar(slug))
	if err != nil {
		return nil
	}
	var t Task
	if json.Unmarshal(b, &t) != nil || t.Slug == "" {
		return nil
	}
	return &t
}

// reconcile makes the list agree with what is on disk: tasks whose folder has
// gone are dropped, and folders describing a task this file does not know about
// are adopted. It reports whether anything changed, so the caller only rewrites
// when it must.
func (s *Store) reconcile(f *file) bool {
	if s.wsRoot == "" {
		return false
	}
	changed := false

	kept := f.Tasks[:0]
	known := map[string]bool{}
	for _, t := range f.Tasks {
		if fi, err := os.Stat(s.dir(t.Slug)); err != nil || !fi.IsDir() {
			changed = true // the folder was deleted outside the app
			continue
		}
		known[t.Slug] = true
		kept = append(kept, t)
	}
	f.Tasks = kept

	entries, err := os.ReadDir(s.wsRoot)
	if err != nil {
		return changed
	}
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		t := s.readSidecar(e.Name())
		if t == nil {
			continue
		}
		// A renamed or copied folder still carries the old slug: the folder's
		// name is the task, otherwise it is re-adopted on every read.
		if t.Slug != e.Name() {
			if ValidateSlug(e.Name()) != nil {
				continue
			}
			t.Slug = e.Name()
			s.writeSidecar(t)
		}
		known[t.Slug] = true
		f.Tasks = append(f.Tasks, t)
		changed = true
	}
	return changed
}

type file struct {
	Tasks []*Task `json:"tasks"`
}

// withLock runs fn while holding an exclusive flock on a sidecar lock file,
// so a CLI invocation and the server cannot interleave read-modify-write.
func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lockPath := s.path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func (s *Store) readUnlocked() (*file, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		f := &file{}
		if s.reconcile(f) {
			_ = s.writeUnlocked(f)
		}
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if len(b) > 0 {
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s is corrupt: %w", s.path, err)
		}
	}
	if s.reconcile(&f) {
		_ = s.writeUnlocked(&f)
	}
	return &f, nil
}

func (s *Store) writeUnlocked(f *file) error {
	sort.Slice(f.Tasks, func(i, j int) bool { return f.Tasks[i].Created.Before(f.Tasks[j].Created) })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) List() ([]*Task, error) {
	var out []*Task
	err := s.withLock(func() error {
		f, err := s.readUnlocked()
		if err != nil {
			return err
		}
		out = f.Tasks
		return nil
	})
	return out, err
}

func (s *Store) Get(slug string) (*Task, error) {
	tasks, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.Slug == slug {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no task %q", slug)
}

// Resolve accepts a unique prefix, which is what makes the CLI pleasant to use.
func (s *Store) Resolve(prefix string) (*Task, error) {
	tasks, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Task
	for _, t := range tasks {
		if t.Slug == prefix {
			return t, nil
		}
		if len(prefix) > 0 && len(t.Slug) >= len(prefix) && t.Slug[:len(prefix)] == prefix {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no task matches %q", prefix)
	case 1:
		return hits[0], nil
	default:
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Slug
		}
		return nil, fmt.Errorf("ambiguous task %q: %v", prefix, names)
	}
}

func (s *Store) Add(t *Task) error {
	return s.withLock(func() error {
		f, err := s.readUnlocked()
		if err != nil {
			return err
		}
		for _, e := range f.Tasks {
			if e.Slug == t.Slug {
				return fmt.Errorf("task %q already exists", t.Slug)
			}
		}
		f.Tasks = append(f.Tasks, t)
		s.writeSidecar(t)
		return s.writeUnlocked(f)
	})
}

// Update applies fn to the stored task under lock and persists the result.
func (s *Store) Update(slug string, fn func(*Task) error) (*Task, error) {
	var updated *Task
	err := s.withLock(func() error {
		f, err := s.readUnlocked()
		if err != nil {
			return err
		}
		for _, e := range f.Tasks {
			if e.Slug == slug {
				if err := fn(e); err != nil {
					return err
				}
				updated = e
				s.writeSidecar(e)
				return s.writeUnlocked(f)
			}
		}
		return fmt.Errorf("no task %q", slug)
	})
	return updated, err
}

func (s *Store) Delete(slug string) error {
	return s.withLock(func() error {
		f, err := s.readUnlocked()
		if err != nil {
			return err
		}
		out := f.Tasks[:0]
		for _, e := range f.Tasks {
			if e.Slug == slug {
				continue
			}
			out = append(out, e)
		}
		// Deleting a task that is already gone succeeds: removing the workspace
		// deletes the folder, and the read above reconciles that away before we
		// get here. Reporting "no task" would fail every successful removal.
		f.Tasks = out
		return s.writeUnlocked(f)
	})
}
