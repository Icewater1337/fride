// Package journal records the reasoning behind a task while the work is
// happening: the plan, the decisions taken, the assumptions made, and the
// issues hit along the way.
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
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"golang.org/x/sys/unix"
)

// Kind classifies an entry. These are the four things worth knowing later.
type Kind string

const (
	KindPlan       Kind = "plan"       // what we set out to do
	KindDecision   Kind = "decision"   // a choice taken, with its reason
	KindAssumption Kind = "assumption" // something taken as true without confirming
	KindIssue      Kind = "issue"      // a problem hit during implementation
	KindNote       Kind = "note"       // anything else worth remembering
)

func ValidKind(k Kind) bool {
	switch k {
	case KindPlan, KindDecision, KindAssumption, KindIssue, KindNote:
		return true
	}
	return false
}

func Kinds() []Kind { return []Kind{KindPlan, KindDecision, KindAssumption, KindIssue, KindNote} }

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
	if e.Time.IsZero() {
		e.Time = time.Now()
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
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
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
	sc := bufio.NewScanner(f)
	// A plan can be a whole document pasted in one entry.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // a corrupt line must not lose the rest of the record
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Count summarises the record for the UI.
func Count(cfg *config.Config, slug string) map[Kind]int {
	entries, _ := Read(cfg, slug)
	out := map[Kind]int{}
	for _, e := range entries {
		out[e.Kind]++
	}
	return out
}

// Dir is where a task's journal lives — outside the workspace, so it has to be
// carried along when a task moves to another computer.
func Dir(cfg *config.Config, slug string) string { return dir(cfg, slug) }
