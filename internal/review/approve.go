package review

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
	"github.com/Icewater1337/fride/internal/store"
	"github.com/Icewater1337/fride/internal/workspace"
)

// Approval is a person's approval of one version of a task's change.
//
// It holds the version it was given on, as a viewed mark holds its hash: an
// approval of what the agent wrote an hour ago says nothing about what it
// wrote since, so it stops counting the moment the change moves.
type Approval struct {
	By string `json:"by"`
	// Via is how it arrived: "browser" (this computer's Review tab),
	// "fleet: <computer>" (the Review tab of another computer of the fleet),
	// or "api" (no browser: the CLI, a script, an agent).
	Via string    `json:"via"`
	At  time.Time `json:"at"`
	// Version is each repository's head and diff hash as the reviewer had
	// them on screen.
	Version map[string]Anchor `json:"version"`
	// Tier is the task's tier when it was given, for the record.
	Tier string `json:"tier"`
	// Human is false for an approval that came from no person's browser. It
	// is kept, so the Review tab can say an agent tried, and never counted.
	Human bool `json:"human"`

	// Computed when served: whether it is of the change as it is now, and
	// whether it counts toward the tier's approvals; Note says why not.
	Current bool   `json:"current"`
	Counts  bool   `json:"counts"`
	Note    string `json:"note,omitempty"`
}

// Accountable is the person who gave the prompt, recorded when the task was
// created: whoever approves, the change is theirs to answer for.
type Accountable struct {
	Person string `json:"person"`
	// How says how FrIDE knows who it is.
	How string    `json:"how"`
	At  time.Time `json:"at,omitzero"`
	// Recorded is false for a task created before FrIDE recorded this: the
	// person is then worked out from what FrIDE knows today.
	Recorded bool `json:"recorded"`
	// Via names the inbox thread the prompt came through, if any. Filled in
	// when served, by whoever knows the inbox.
	Via string `json:"via,omitempty"`
}

// Gate is what a task's change needs before it may be Done: the approvals
// its tier asks for, of its current version.
type Gate struct {
	Tier  string     `json:"tier"`
	Need  int        `json:"need"`
	Have  int        `json:"have"`
	Ready bool       `json:"ready"`
	Repos []RepoTier `json:"repos"`
	// Why are the files that set the tier (those at the task's tier), up to
	// maxWhy; More counts the rest.
	Why  []FileTier `json:"why"`
	More int        `json:"more,omitempty"`
	// Version is the change as it is now: an approval sends it back, so a
	// click on a page drawn before the change moved approves nothing.
	Version     map[string]Anchor `json:"version"`
	Approvals   []Approval        `json:"approvals"`
	Accountable Accountable       `json:"accountable"`
	// You is who an approval from this computer is recorded as, unless the
	// reviewer names someone else.
	You string `json:"you,omitempty"`
}

const (
	approvalsFile = "approvals.json"
	maxWhy        = 12
)

type approvalState struct {
	Accountable *Accountable `json:"accountable,omitempty"`
	Approvals   []Approval   `json:"approvals"`
}

func loadApprovals(cfg *config.Config, slug string) approvalState {
	var a approvalState
	load(cfg, slug, approvalsFile, &a)
	if a.Approvals == nil {
		a.Approvals = []Approval{}
	}
	return a
}

// PersonOf is who FrIDE knows as the person behind a task on this computer:
// the name and email of the task's git identity, else git's own user.name
// and user.email as the task's first repository sees them, else the
// computer's user. FrIDE has no logins; this is what the commits say.
func PersonOf(cfg *config.Config, t *store.Task) (person, how string) {
	if t.GitIdentity != "" && cfg.Settings != nil {
		if g, ok := cfg.Settings.GitIdentity(t.GitIdentity); ok && (g.Name != "" || g.Email != "") {
			return nameEmail(g.Name, g.Email), "the task's git identity " + g.ID
		}
	}
	dir := ""
	if len(t.Repos) > 0 {
		dir = cfg.RepoDir(t.Slug, t.Repos[0].Name)
	}
	args := []string{"config", "--get"}
	if fi, err := os.Stat(dir); dir == "" || err != nil || !fi.IsDir() {
		// A worktree that is gone: git's global configuration, not that of
		// whatever repository this process happens to run in.
		dir, args = "", []string{"config", "--global", "--get"}
	}
	name, _ := git(dir, append(args, "user.name")...)
	email, _ := git(dir, append(args, "user.email")...)
	if name != "" || email != "" {
		return nameEmail(name, email), "git's user.name and user.email"
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username, "this computer's user"
	}
	return "unknown", "nothing: no git identity, git user or computer user"
}

func nameEmail(name, email string) string {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	switch {
	case email == "":
		return name
	case name == "":
		return "<" + email + ">"
	}
	return name + " <" + email + ">"
}

// personKey is what makes two approvals the same person's: the email when
// there is one, else the name, either way without case.
func personKey(p string) string {
	if i, j := strings.LastIndex(p, "<"), strings.LastIndex(p, ">"); i >= 0 && j > i+1 {
		return strings.ToLower(strings.TrimSpace(p[i+1 : j]))
	}
	return strings.ToLower(strings.Join(strings.Fields(p), " "))
}

// RecordAccountable writes down who gave the prompt for a new task. It never
// replaces a record: the person a task was created for stays its person.
func RecordAccountable(cfg *config.Config, t *store.Task) error {
	person, how := PersonOf(cfg, t)
	stateMu.Lock()
	defer stateMu.Unlock()
	a := loadApprovals(cfg, t.Slug)
	if a.Accountable != nil {
		return nil
	}
	a.Accountable = &Accountable{Person: person, How: how, At: time.Now(), Recorded: true}
	return save(cfg, t.Slug, approvalsFile, a)
}

// Approve records an approval. A person approving again replaces their
// earlier approval, so one person is never two approvals. An approval that
// did not come from a person never replaces one that did.
func Approve(cfg *config.Config, slug string, a Approval) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	st := loadApprovals(cfg, slug)
	a.At = time.Now()
	a.Current, a.Counts, a.Note = false, false, ""
	key := personKey(a.By)
	kept := st.Approvals[:0]
	for _, x := range st.Approvals {
		if personKey(x.By) != key || x.Human != a.Human {
			kept = append(kept, x)
		}
	}
	st.Approvals = append(kept, a)
	// Approvals from no browser are only kept to be shown, so the latest few
	// do: a script in a loop must not grow the file without end.
	others := 0
	for i := len(st.Approvals) - 1; i >= 0; i-- {
		if st.Approvals[i].Human {
			continue
		}
		if others++; others > maxNonHuman {
			st.Approvals = append(st.Approvals[:i], st.Approvals[i+1:]...)
		}
	}
	return save(cfg, slug, approvalsFile, st)
}

const maxNonHuman = 5

// ErrMoved refuses an approval of a version that is no longer the change.
var ErrMoved = errors.New("the change moved on since the page was drawn: refresh, read what changed, then approve")

// ApproveCurrent records an approval of a.Version, which must be the change
// as it is now: approving what the agent has since rewritten would vouch for
// code nobody read. An approval that names nobody is recorded as the person
// FrIDE knows on this computer (PersonOf). It returns the gate after it.
func ApproveCurrent(cfg *config.Config, t *store.Task, a Approval) (Gate, error) {
	g := GateFor(cfg, t)
	if !sameVersion(a.Version, g.Version) {
		return g, ErrMoved
	}
	a.By = strings.Join(strings.Fields(a.By), " ")
	if a.By == "" {
		a.By = g.You
	}
	if len(a.By) > 200 {
		return g, fmt.Errorf("a name of at most 200 characters")
	}
	a.Tier = g.Tier
	if err := Approve(cfg, t.Slug, a); err != nil {
		return g, err
	}
	return GateFor(cfg, t), nil
}

// Withdraw takes back a person's approval. It reports whether there was one.
func Withdraw(cfg *config.Config, slug, by string) (bool, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	st := loadApprovals(cfg, slug)
	key, found := personKey(by), false
	kept := st.Approvals[:0]
	for _, x := range st.Approvals {
		if x.Human && personKey(x.By) == key {
			found = true
			continue
		}
		kept = append(kept, x)
	}
	if !found {
		return false, nil
	}
	st.Approvals = kept
	return true, save(cfg, slug, approvalsFile, st)
}

// gate weighs a task's approvals against the tier of its change.
func gate(repos []RepoChanges, tiers []RepoTier, kept Kept) Gate {
	g := Gate{Tier: settings.TierAuto, Repos: tiers, Why: []FileTier{}, Approvals: []Approval{},
		Version: map[string]Anchor{}, Accountable: kept.Accountable}
	if g.Repos == nil {
		g.Repos = []RepoTier{}
	}
	for _, rt := range tiers {
		if tierRank(rt.Tier) > tierRank(g.Tier) {
			g.Tier = rt.Tier
		}
	}
	g.Need = Need(g.Tier)
	for _, rc := range repos {
		g.Version[rc.Repo] = Anchor{Head: rc.Head, Hash: repoHash(rc)}
		for _, f := range rc.Files {
			if f.Tier != g.Tier {
				continue
			}
			if len(g.Why) == maxWhy {
				g.More++
				continue
			}
			g.Why = append(g.Why, FileTier{Repo: rc.Repo, Path: f.Path, Tier: f.Tier, Why: f.TierWhy, Rule: f.TierRule})
		}
	}
	seen := map[string]bool{}
	for _, a := range kept.Approvals {
		a.Current = sameVersion(a.Version, g.Version)
		switch {
		case !a.Human:
			a.Note = "not from a person's browser (an agent, a script or the CLI): only an approval given in the Review tab counts"
		case !a.Current:
			a.Note = "of an earlier version: the change moved on since"
		case seen[personKey(a.By)]:
			a.Note = "the same person approved already"
		default:
			seen[personKey(a.By)] = true
			a.Counts = true
			g.Have++
		}
		g.Approvals = append(g.Approvals, a)
	}
	g.Ready = g.Have >= g.Need
	return g
}

// sameVersion: every repository at the same head and diff, and no more or
// fewer of them.
func sameVersion(a, b map[string]Anchor) bool {
	if len(a) != len(b) {
		return false
	}
	for repo, x := range b {
		if y, ok := a[repo]; !ok || y != x {
			return false
		}
	}
	return true
}

// Specs are a task's repositories that have a worktree, with their base and
// the review rules config.json gives them.
func Specs(cfg *config.Config, t *store.Task) []RepoSpec {
	present := map[string]bool{}
	for _, r := range workspace.Repos(cfg, t.Slug) {
		present[r.Name] = true
	}
	var repos []RepoSpec
	for _, tr := range t.Repos {
		// A repo the task names but whose worktree is gone has nothing to diff.
		if !present[tr.Name] {
			continue
		}
		dir := cfg.RepoDir(t.Slug, tr.Name)
		base := tr.BaseRef
		if base == "" {
			base = workspace.DefaultBase(dir)
		}
		var rv *settings.Review
		if cfg.Settings != nil {
			rv = cfg.Settings.Repo(tr.Name).Review
		}
		repos = append(repos, RepoSpec{Name: tr.Name, Dir: dir, Base: base, Review: rv})
	}
	return repos
}

// KeptFor is LoadKept, with the accountable person worked out for a task
// created before FrIDE recorded one.
func KeptFor(cfg *config.Config, t *store.Task) Kept {
	k := LoadKept(cfg, t.Slug)
	if k.Accountable.Person == "" {
		k.Accountable.Person, k.Accountable.How = PersonOf(cfg, t)
	}
	return k
}

// GateFor reads a task's change and weighs its approvals: the gate without
// the rest of the review (transcripts, focus, AI pass), for closing a task
// and for whatever merges its work.
func GateFor(cfg *config.Config, t *store.Task) Gate {
	var repos []RepoChanges
	var tiers []RepoTier
	for _, sp := range Specs(cfg, t) {
		rc := Diff(sp.Dir, sp.Name, sp.Base, newBudget())
		tiers = append(tiers, placeRepo(sp, &rc))
		repos = append(repos, rc)
	}
	g := gate(repos, tiers, KeptFor(cfg, t))
	g.You, _ = PersonOf(cfg, t)
	return g
}

// checkApprovals refuses Done while the change lacks the approvals its tier
// asks for. It reads the task itself, so CheckDone stays the one call every
// path to Done makes.
func checkApprovals(cfg *config.Config, slug string) error {
	st, err := store.New(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	// A store that cannot be read refuses; a task it does not have has no
	// change to approve, and the move to Done fails on its own.
	tasks, err := st.List()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if t.Slug != slug {
			continue
		}
		if g := GateFor(cfg, t); !g.Ready {
			return fmt.Errorf("%s: a person approves it in the Review tab first", g.Missing())
		}
	}
	return nil
}

// Missing says what the gate still lacks, in one sentence; "" when ready.
func (g Gate) Missing() string {
	if g.Ready {
		return ""
	}
	why := ""
	switch {
	case len(g.Why) > 0:
		w := g.Why[0]
		why = fmt.Sprintf(" (%s/%s: %s", w.Repo, w.Path, w.Why)
		if n := len(g.Why) - 1 + g.More; n > 0 {
			why += fmt.Sprintf(", and %d more file%s", n, plural(n))
		}
		why += ")"
	default:
		for _, r := range g.Repos {
			if r.Tier == g.Tier && r.Err != "" {
				why = fmt.Sprintf(" (%s: %s)", r.Repo, r.Err)
				break
			}
		}
	}
	return fmt.Sprintf("the change is %s tier%s and needs %d human approval%s of its current version, it has %d",
		g.Tier, why, g.Need, plural(g.Need), g.Have)
}
