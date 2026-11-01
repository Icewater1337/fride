// Package workspace creates and removes multi-repo task workspaces: a parent
// folder holding one git worktree per repo, so an agent opened on that folder
// sees every repo side by side.
//
// Claude Code cannot do this itself — `claude --worktree` makes a worktree for
// the single repo you are standing in, and `--add-dir` grants a second repo
// access but NOT a worktree (edits there would land on its main checkout).
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
)

type RepoSpec struct {
	Name    string
	BaseRef string // empty = that repo's origin HEAD
}

type Created struct {
	Repo    string
	BaseRef string
	Branch  string
	Copied  []string
	Notes   []string
	// NewBranch: this call created the branch, rather than checking out one
	// that already existed. Only such a branch may be deleted by Undo.
	NewBranch bool
}

// ErrExists is returned by Create when the task's folder is already there. It
// belongs to another task, or to the user: nothing in it may be touched.
var ErrExists = errors.New("already exists")

func git(dir string, args ...string) (string, error) { return gitEnv(dir, nil, args...) }

// gitEnv is git() with extra environment: a task's git identity, for the
// calls that reach its remote.
func gitEnv(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// gitRaw is git() without the leading-whitespace trim.
//
// `git status --porcelain` emits "XY PATH" where X or Y may be a space, so a
// line can legitimately start with one. TrimSpace on the combined output eats
// that space on the first line, shifting the status code and truncating the
// path (" M README.md" was being read as status "M " and path "EADME.md").
func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// DefaultBase resolves a repo's default base: origin's default branch, else
// origin/main or origin/master, else — for a repository with no remote at all —
// the branch its main checkout is on.
func DefaultBase(mainCheckout string) string {
	if ref, err := git(mainCheckout, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return ref
	}
	for _, ref := range []string{"origin/main", "origin/master"} {
		if _, err := git(mainCheckout, "rev-parse", "--verify", "--quiet", "refs/remotes/"+ref); err == nil {
			return ref
		}
	}
	if b, err := git(mainCheckout, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b != "" && b != "HEAD" {
		return b
	}
	return "HEAD"
}

// BranchName is the fallback branch for a task that names none. It is only a
// default: the branch is what gets pushed and reviewed, so the user chooses it.
func BranchName(slug string) string { return "worktree-" + slug }

// branchTypes are the conventional-commit types recognised at the start of a
// task name, so "fix login redirect" suggests fix/... rather than feat/....
var branchTypes = []string{"feat", "fix", "chore", "docs", "refactor", "test",
	"perf", "style", "ci", "build", "revert"}

var ticketRe = regexp.MustCompile(`(?i)\b([a-z]{2,}-\d+)\b`)

// SuggestBranch derives <type>/<TICKET>-<description> from a task name. It is a
// starting point shown in the UI, not a constraint — anything git accepts works.
func SuggestBranch(name string) string {
	rest := strings.TrimSpace(name)
	kind := "feat"
	for _, t := range branchTypes {
		if len(rest) > len(t) && strings.EqualFold(rest[:len(t)], t) &&
			(rest[len(t)] == ' ' || rest[len(t)] == '/' || rest[len(t)] == ':' || rest[len(t)] == '(') {
			kind = t
			rest = strings.TrimLeft(rest[len(t):], " /:")
			break
		}
	}
	ticket := ""
	if m := ticketRe.FindString(rest); m != "" {
		ticket = strings.ToUpper(m)
		rest = strings.Replace(rest, m, "", 1)
	}
	desc := dashify(rest)
	switch {
	case ticket != "" && desc != "":
		return kind + "/" + ticket + "-" + desc
	case ticket != "":
		return kind + "/" + ticket
	case desc != "":
		return kind + "/" + desc
	}
	return ""
}

func dashify(s string) string {
	out := make([]rune, 0, len(s))
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
			dash = false
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
			dash = false
		default:
			if !dash && len(out) > 0 {
				out = append(out, '-')
				dash = true
			}
		}
	}
	return strings.Trim(string(out), "-")
}

// ValidateBranch defers to git itself rather than reimplementing its rules,
// which are more intricate than they look (no "..", no trailing ".lock", no
// leading dot in any path component, and so on).
func ValidateBranch(b string) error {
	if b == "" {
		return fmt.Errorf("branch name is empty")
	}
	if strings.HasPrefix(b, "-") {
		return fmt.Errorf("branch %q may not start with a dash", b)
	}
	if _, err := git("", "check-ref-format", "--branch", b); err != nil {
		return fmt.Errorf("%q is not a valid git branch name", b)
	}
	return nil
}

// ValidateBaseRef refuses a base ref that git could read as an option. It is
// not check-ref-format: a base may be any revision (HEAD~2, a commit id), which
// that rule rejects. "origin/-x" is checked too, as the fetch gets only "-x".
func ValidateBaseRef(b string) error {
	if strings.HasPrefix(b, "-") || strings.HasPrefix(strings.TrimPrefix(b, "origin/"), "-") {
		return fmt.Errorf("base ref %q may not start with a dash", b)
	}
	return nil
}

// PruneStale forgets worktree registrations whose directory has gone. Deleting
// a workspace folder from Finder is a supported way to delete a task, but git
// still holds a registration for it, which would later refuse the same slug or
// claim its branch is checked out somewhere.
func PruneStale(cfg *config.Config) {
	for _, repo := range cfg.Repos {
		_, _ = git(repo.Main, "worktree", "prune")
	}
}

// Create builds the workspace. It is all-or-nothing, and it cleans up after
// itself: on failure it removes exactly what it made — never a folder, worktree
// or branch that was there before. A caller must not clean up after it. An
// earlier version left that to the caller, which ran a forced Remove and so
// wiped the existing task whenever the slug was already taken.
func Create(cfg *config.Config, slug string, specs []RepoSpec, agentKind, branch string) ([]Created, error) {
	return CreateWith(cfg, slug, specs, agentKind, branch, nil)
}

// CreateWith is Create with the task's git identity environment (gitid.Env)
// for its fetches, so a private remote is reached with the task's own
// credentials rather than whatever git finds for the user.
func CreateWith(cfg *config.Config, slug string, specs []RepoSpec, agentKind, branch string, env []string) (out []Created, err error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("name at least one repo")
	}
	if branch == "" {
		branch = BranchName(slug)
	}
	if err := ValidateBranch(branch); err != nil {
		return nil, err
	}
	for _, sp := range specs {
		if err := ValidateBaseRef(sp.BaseRef); err != nil {
			return nil, fmt.Errorf("%s: %w", sp.Name, err)
		}
	}
	ws := cfg.WorkspaceDir(slug)
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		return nil, err
	}
	// Mkdir, not MkdirAll: it fails when the folder exists, atomically, so two
	// creates of one slug (a double-clicked Create, the CLI racing the UI)
	// cannot both get past here. The folder is this call's own from now on.
	if err := os.Mkdir(ws, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%s %w", ws, ErrExists)
		}
		return nil, err
	}
	// pending is the repo being added when a step fails: its worktree or new
	// branch may exist although it never made it into out.
	var pending *Created
	defer func() {
		if err != nil {
			made := out
			if pending != nil {
				made = append(made, *pending)
			}
			undo(cfg, slug, made)
			out = nil
		}
	}()
	// A folder deleted outside the app leaves its registration behind, which
	// would refuse this slug or claim the branch is still checked out.
	PruneStale(cfg)
	for _, sp := range specs {
		pending = nil
		repo, ok := cfg.Repo(sp.Name)
		if !ok {
			return out, fmt.Errorf("unknown repo %q", sp.Name)
		}
		if fi, err := os.Stat(repo.Main); err != nil || !fi.IsDir() {
			return out, fmt.Errorf("main checkout missing: %s", repo.Main)
		}
		base := sp.BaseRef
		if base == "" {
			base = DefaultBase(repo.Main)
		}
		c := Created{Repo: repo.Name, BaseRef: base, Branch: branch}

		// Refresh remote-tracking refs so "from origin/main" means today's main.
		if strings.HasPrefix(base, "origin/") {
			if _, err := gitEnv(repo.Main, env, "fetch", "--quiet", "--end-of-options", "origin", strings.TrimPrefix(base, "origin/")); err != nil {
				c.Notes = append(c.Notes, "could not fetch "+base+" — using the local ref")
			}
		}
		if _, err := git(repo.Main, "rev-parse", "--verify", "--quiet", "--end-of-options", base); err != nil {
			return out, fmt.Errorf("%s: base ref %q does not exist", repo.Name, base)
		}
		dest := cfg.RepoDir(slug, repo.Name)
		// Which of these three applies decides whether existing commits survive.
		// The branch name is the user's, so it may well already exist: -B would
		// force it back to base and throw away whatever was on it. Only the
		// first case creates anything.
		var addArgs []string
		_, localErr := git(repo.Main, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
		_, remoteErr := git(repo.Main, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
		switch {
		case localErr == nil:
			// Continue the existing branch where it stands, ignoring base.
			addArgs = []string{"worktree", "add", dest, branch}
			c.Notes = append(c.Notes, "branch "+branch+" already existed — continuing it (base ref ignored)")
			c.BaseRef = branch
		case remoteErr == nil:
			addArgs = []string{"worktree", "add", "--track", "-b", branch, dest, "origin/" + branch}
			c.Notes = append(c.Notes, "branch "+branch+" existed on origin — checked out and tracking it")
			c.BaseRef = "origin/" + branch
		default:
			addArgs = []string{"worktree", "add", "--no-track", "-b", branch, dest, base}
		}
		c.NewBranch = localErr != nil
		// Set before the add: a failed `worktree add -b` can leave the branch
		// behind, and undo has to know it was this call's.
		pending = &c
		if _, err := git(repo.Main, addArgs...); err != nil {
			return out, fmt.Errorf("%s: %w (is branch %q checked out elsewhere?)", repo.Name, err, branch)
		}
		copied, notes := copyIncludes(cfg, repo.Name, repo.Main, dest)
		c.Copied = copied
		c.Notes = append(c.Notes, notes...)
		out = append(out, c)
	}
	pending = nil
	if err := writeGuide(cfg, slug, specs, agentKind, branch); err != nil {
		return out, fmt.Errorf("the workspace's guide file could not be written: %w", err)
	}
	// Best effort: a missing hook costs some rationale, not the workspace, and
	// a missing skill only an idea page.
	if bin, err := os.Executable(); err == nil {
		_ = journal.InstallHook(cfg, slug, bin)
	}
	_ = journal.InstallIdeaSkill(cfg, slug)
	return out, nil
}

// Undo takes back a workspace Create returned, for a caller whose next step
// failed (the task could not be registered). Like Create's own cleanup, it
// removes only what Create made.
func Undo(cfg *config.Config, slug string, created []Created) {
	undo(cfg, slug, created)
}

func undo(cfg *config.Config, slug string, made []Created) {
	for i := len(made) - 1; i >= 0; i-- {
		c := made[i]
		repo, ok := cfg.Repo(c.Repo)
		if !ok {
			continue
		}
		_, _ = git(repo.Main, "worktree", "remove", "--force", cfg.RepoDir(slug, c.Repo))
		_, _ = git(repo.Main, "worktree", "prune")
		if c.NewBranch {
			// Fresh from its base, so nothing is on it. branch -D refuses a
			// branch that is checked out anywhere, which keeps it safe even so.
			_, _ = git(repo.Main, "branch", "-D", c.Branch)
		}
	}
	// Create made this folder with Mkdir, so everything left in it — guide,
	// hook, copied local config — is the same call's.
	_ = os.RemoveAll(cfg.WorkspaceDir(slug))
}

// writeGuide drops the agent's own instructions file at the workspace root,
// which is where the agent is started. The name matters: Claude Code reads
// CLAUDE.md, while Codex and Antigravity read AGENTS.md, so writing the wrong
// one means the agent silently never sees any of this.
//
// It is how the agent learns that FrIDE owns this workspace's stack and that
// the journal exists — the same way it picks up any other project convention.
// It sits outside every repo, so it never appears in a diff.
func writeGuide(cfg *config.Config, slug string, specs []RepoSpec, agentKind, branch string) error {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	body := `# Workspace ` + slug + `

This folder is a FrIDE task workspace. Each subdirectory is a git worktree of
one repository, all on the branch ` + "`" + branch + "`" + `: ` + strings.Join(names, ", ") + `.

` + stackSection(cfg, slug) + `
## Keep the decision log current

This task produces a design-rationale document at the end, assembled from
entries written WHILE the work happened. A diff shows what changed; it does not
show why that option won. So log each decision when you take it — afterwards it
is gone, and anything reconstructed later is guesswork.

` + "```bash" + `
fride note plan --file PLAN.md                  # the plan, yours or the user's
fride note decision "..." --why "..."           # a choice, and why you took it
fride note assumption "..." --why "..."         # something you assumed, unconfirmed
fride note issue "..."                          # a problem hit along the way
fride note ls                                   # what has been recorded so far
` + "```" + `

Add ` + "`--repo <name>`" + ` when an entry concerns one repository only.

Record an entry when you:

- settle on a plan, or change it
- pick between real alternatives (a library, a layer, a data shape, a workaround)
- assume something you did not verify (an API's behaviour, an owner's intent, a default)
- hit a problem that cost time, or that the next person would hit too

Do not narrate routine steps. Four good decision entries beat forty notes.

## Decisions the user makes

The conversation is not recorded anywhere. When the user settles a question —
picks one of the options you offered, overrules an approach you proposed, rules
something out of scope, or answers a question you had to ask before you could
continue — write it down straight away with ` + "`--source user`" + `:

` + "```bash" + `
fride note decision "Attach to the shared database rather than a per-task one" \
  --why "User: the seed data takes 20 minutes to rebuild and every task needs it" \
  --source user
` + "```" + `

Two things make such an entry worth having, so put both in:

- **what was being decided**, including the alternative that lost. "Chose X"
  is half a record; "chose X over Y" is the whole one.
- **the reason the user gave**, in their terms, not your reconstruction of it.
  If they gave none, say so — an unexplained instruction is still a fact, and
  pretending to a reason is worse than recording its absence.

Use ` + "`--source user`" + ` only for what the user actually decided. Your own
choices stay unmarked, so that a reader can tell which calls were theirs.
` + ideaSection(cfg)
	return os.WriteFile(filepath.Join(cfg.WorkspaceDir(slug), agent.GuideFile(agent.Normalize(agentKind))), []byte(body), 0o644)
}

// ideaSection points the agent at the document-idea skill, when the team has
// an ideas repository to publish to. Agents other than Claude Code do not load
// skills, so it names the file.
func ideaSection(cfg *config.Config) string {
	if !journal.IdeasEnabled(cfg) {
		return ""
	}
	return `
## Exploring an idea, not a ticket?

When this task is about trying something NEW — a proof of concept, a spike,
"what if we…" — rather than implementing a defined ticket, follow the
` + "`document-idea`" + ` skill (` + "`.claude/skills/document-idea/SKILL.md`" + `; read it if your
agent does not load skills). It keeps a living idea page under ` + "`" + journal.IdeaFolder + "/`" + `, in the
layout of the team's ideas repository, and the person publishes it with
*Publish idea* in FrIDE. Ticket work stays in the decision log above.
`
}

const stackHeading = "## Running the stack"

// stackSection tells the agent which tool owns this workspace's stack. Without
// it an agent falls back on whatever else it knows — the services' own start
// commands — and starts a copy on the default ports that the board cannot see,
// collides with the main checkout, and is never stopped.
func stackSection(cfg *config.Config, slug string) string {
	return stackHeading + `

Start, stop and inspect this task's stack with FrIDE, never by running the
services' own start commands by hand: only a stack started this way gets this
task's own ports, shows up on the task board, and is stopped cleanly.

` + "```bash" + `
fride stack up ` + slug + ` --wait     # start it, and wait until every service is ready
fride stack status ` + slug + `        # what runs, how, and on which port
fride stack logs ` + slug + ` <svc>    # the end of one service's log
fride stack down ` + slug + `          # stop it
` + "```" + `

A repo folder in this workspace runs from source; a service whose repo is not
here comes from a container, or is left out, as the task's profile says.
Open the app on the entry URL ` + "`stack status`" + ` prints. Logs live in
` + "`" + filepath.Join(cfg.RunDir(slug), "logs") + "`" + `, outside the workspace.
`
}

// EnsureGuide adds the stack section to a guide written before the section
// existed, so older workspaces learn it too the next time an agent starts in
// them. It only appends: the rest of the file may have been edited by hand.
func EnsureGuide(cfg *config.Config, slug, agentKind string) error {
	path := filepath.Join(cfg.WorkspaceDir(slug), agent.GuideFile(agent.Normalize(agentKind)))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && strings.Contains(string(b), stackHeading)) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + stackSection(cfg, slug))
	return err
}

// copyIncludes replicates Claude Code's .worktreeinclude behaviour, which only
// runs when Claude Code itself creates the worktree — `git worktree add` does not
// do it. Without this a fresh worktree has no .env / .env.properties and the
// stack cannot start.
//
// Deliberately literal-path only: the real matcher is gitignore syntax applied
// to `git ls-files --others --ignored`. Rather than half-implement that, a glob
// is reported as a note instead of being silently skipped.
func copyIncludes(cfg *config.Config, name, mainCheckout, dest string) (copied []string, notes []string) {
	entries, overridden := cfg.Settings.EffectiveCopy(name, mainCheckout)
	if overridden {
		notes = append(notes, "copy list from config.json (repos."+name+".copy)")
	}
	for _, line := range entries {
		if strings.ContainsAny(line, "*?[") {
			notes = append(notes, fmt.Sprintf("%q uses glob syntax — copy it by hand", line))
			continue
		}
		src := filepath.Join(mainCheckout, line)
		fi, err := os.Lstat(src)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%q not present — skipped", line))
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			notes = append(notes, fmt.Sprintf("%q is a symlink — skipped", line))
			continue
		}
		if fi.IsDir() {
			notes = append(notes, fmt.Sprintf("%q is a directory — list files, not directories", line))
			continue
		}
		// Tracked files arrive via git; copying them would shadow the checkout.
		if _, err := git(mainCheckout, "ls-files", "--error-unmatch", line); err == nil {
			notes = append(notes, fmt.Sprintf("%q is tracked — git already provides it", line))
			continue
		}
		if err := copyFile(src, filepath.Join(dest, line), fi); err != nil {
			notes = append(notes, fmt.Sprintf("failed to copy %q: %v", line, err))
			continue
		}
		copied = append(copied, line)
	}
	return copied, notes
}

func copyFile(src, dst string, fi os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

type RemoveResult struct {
	Repo    string
	Removed bool
	Detail  string
}

// Purge is Remove followed by deleting the directory outright, for when the
// point is to get rid of the folder rather than to retire the task tidily. It
// still goes through git first so the main repositories do not keep stale
// worktree registrations pointing at a path that no longer exists.
//
// It refuses when Remove kept a repo even with force: what force cannot
// discard (commits on a detached HEAD, a worktree git cannot read) would be
// lost for good with the folder.
func Purge(cfg *config.Config, slug string) ([]RemoveResult, error) {
	res, err := Remove(cfg, slug, true)
	if err != nil {
		return res, err
	}
	var kept []string
	for _, r := range res {
		if !r.Removed {
			kept = append(kept, r.Repo+" ("+r.Detail+")")
		}
	}
	if len(kept) > 0 {
		return res, fmt.Errorf("workspace kept: %s", strings.Join(kept, "; "))
	}
	if err := os.RemoveAll(cfg.WorkspaceDir(slug)); err != nil {
		return res, err
	}
	for _, repo := range cfg.Repos {
		// A worktree whose directory vanished is only forgotten on prune.
		_, _ = git(repo.Main, "worktree", "prune")
	}
	return res, nil
}

// Remove takes the workspace down. A repo with uncommitted work is kept and
// reported rather than force-removed, and the task branch is deleted only where
// it holds no unique commits.
//
// Every decision here destroys something when it goes the wrong way, so an
// error never counts as the harmless answer: a branch whose unpushed commits
// cannot be counted is kept, and so is a worktree whose HEAD cannot be read.
// force discards uncommitted work only — never commits.
func Remove(cfg *config.Config, slug string, force bool) ([]RemoveResult, error) {
	ws := cfg.WorkspaceDir(slug)
	if _, err := os.Stat(ws); err != nil {
		return nil, fmt.Errorf("no workspace at %s", ws)
	}
	var res []RemoveResult
	allGone := true
	for _, repo := range cfg.Repos {
		dir := cfg.RepoDir(slug, repo.Name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		keep := func(detail string) {
			allGone = false
			res = append(res, RemoveResult{Repo: repo.Name, Removed: false, Detail: detail})
		}
		// Ask the worktree what it is on. The branch is the user's choice and is
		// recorded per repo, so deriving it from the slug would delete the wrong
		// branch — or, more often, silently leave the right one behind. The
		// full ref, not --abbrev-ref: a short name can also be a tag or a path.
		head, err := git(dir, "rev-parse", "--verify", "--quiet", "HEAD")
		if err != nil {
			keep("kept — cannot read its HEAD (" + err.Error() + "); inspect with: git -C " + dir + " status")
			continue
		}
		ref, err := git(dir, "symbolic-ref", "--quiet", "HEAD")
		if err != nil {
			// Detached. Commits made here are reachable from this worktree's
			// HEAD alone, and removing the worktree drops that — and its
			// reflog — for good.
			n, err := countCommits(repo.Main, head, "--branches", "--remotes", "--tags")
			switch {
			case err != nil:
				keep("kept — detached HEAD, and whether its commits are on a branch could not be checked (" + err.Error() + ")")
				continue
			case n > 0:
				keep(fmt.Sprintf("kept — detached HEAD with %d commit(s) on no branch; keep them with: git -C %s switch -c <branch>", n, dir))
				continue
			}
			ref = ""
		}
		// A headless `claude -p` run leaves a stale lock behind even though its
		// pid is gone; clear it so removal is not blocked by a dead session.
		_, _ = git(repo.Main, "worktree", "unlock", dir)
		args := []string{"worktree", "remove", dir}
		if force {
			args = []string{"worktree", "remove", "--force", dir}
		}
		if _, err := git(repo.Main, args...); err != nil {
			keep("kept — uncommitted work or locked; inspect with: git -C " + dir + " status")
			continue
		}
		detail := "removed"
		if branch := strings.TrimPrefix(ref, "refs/heads/"); branch != ref {
			n, err := countCommits(repo.Main, ref, "--remotes")
			switch {
			case err != nil:
				detail = fmt.Sprintf("removed — KEPT branch %s (its unpushed commits could not be counted: %v)", branch, err)
			case n == 0:
				_, _ = git(repo.Main, "branch", "-D", branch)
				detail = "removed (branch had no unique commits — deleted)"
			default:
				detail = fmt.Sprintf("removed — KEPT branch %s (%d unpushed commit(s))", branch, n)
			}
		}
		res = append(res, RemoveResult{Repo: repo.Name, Removed: true, Detail: detail})
	}
	// A worktree whose repository is no longer discovered (its base folder left
	// the settings, its main checkout moved) is not in cfg.Repos. Skipping it
	// silently let Purge delete its uncommitted work with the folder.
	known := map[string]bool{}
	for _, repo := range cfg.Repos {
		known[repo.Name] = true
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(ws, e.Name(), ".git")); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		allGone = false
		res = append(res, RemoveResult{Repo: e.Name(), Removed: false,
			Detail: "kept — repository no longer configured; inspect with: git -C " + filepath.Join(ws, e.Name()) + " status"})
	}
	if allGone {
		// The workspace root also holds the files this tool put there itself
		// (CLAUDE.md, .claude/settings.json for the PreCompact hook). Remove
		// exactly those, then the directory — os.Remove still guards the rest, so
		// anything the user left behind keeps the workspace alive rather than
		// being deleted silently.
		for _, g := range agent.GuideFiles() {
			_ = os.Remove(filepath.Join(ws, g))
		}
		_ = os.RemoveAll(filepath.Join(ws, ".claude"))
		// .fride holds the sidecar describing this task. It has to go with the
		// rest: while it survives, the workspace is never empty, so the os.Remove
		// below fails and the next read adopts the task straight back from the
		// sidecar — a removal that reports success and does nothing.
		_ = os.RemoveAll(filepath.Join(ws, ".fride"))
		_ = os.Remove(ws) // only succeeds when empty, which is what we want
	}
	return res, nil
}

// countCommits counts the commits reachable from rev and from none of the refs
// the exclude options name (--remotes, --branches, …). The trailing "--" ends
// the revisions: without it a branch named like a folder of the checkout
// (docs, api) is an "ambiguous argument", and rev-list fails.
func countCommits(repo, rev string, exclude ...string) (int, error) {
	args := append([]string{"rev-list", "--count", rev, "--not"}, exclude...)
	s, err := git(repo, append(args, "--")...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(s))
}

// Repos lists which of the known repos actually exist in a workspace.
func Repos(cfg *config.Config, slug string) []config.Repo {
	var out []config.Repo
	for _, r := range cfg.Repos {
		if fi, err := os.Stat(cfg.RepoDir(slug, r.Name)); err == nil && fi.IsDir() {
			out = append(out, r)
		}
	}
	return out
}

// Branches reports the current branch of each repo in the workspace, for the UI.
func Branches(cfg *config.Config, slug string) map[string]string {
	out := map[string]string{}
	for _, r := range Repos(cfg, slug) {
		if b, err := git(cfg.RepoDir(slug, r.Name), "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			out[r.Name] = b
		}
	}
	return out
}

// Dirty reports how many changed files each repo has, so the UI can warn before
// a workspace is discarded.
func Dirty(cfg *config.Config, slug string) map[string]int {
	out := map[string]int{}
	for _, r := range Repos(cfg, slug) {
		s, err := gitRaw(cfg.RepoDir(slug, r.Name), "status", "--porcelain")
		if err != nil {
			continue
		}
		n := 0
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		out[r.Name] = n
	}
	return out
}

// RecentBranches lists branches by most recent commit, for the "base branch"
// picker in the new-task dialog. Remote-tracking refs come first because a task
// almost always starts from origin/<something>.
func RecentBranches(mainCheckout string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasSuffix(l, "/HEAD") || strings.Contains(l, "->") || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	if s, err := git(mainCheckout, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)", fmt.Sprintf("--count=%d", limit), "refs/remotes/origin"); err == nil {
		add(s)
	}
	if s, err := git(mainCheckout, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)", fmt.Sprintf("--count=%d", limit), "refs/heads"); err == nil {
		add(s)
	}
	return out
}

// FileChange is one uncommitted path in a repo, with its porcelain status code.
type FileChange struct {
	Status string `json:"status"` // two-char porcelain code, e.g. " M", "??", "A "
	Path   string `json:"path"`
}

// RepoStat is what the UI shows per repo: where the branch is, how much has
// changed since the task started, and what is still uncommitted.
type RepoStat struct {
	Repo      string       `json:"repo"`
	Branch    string       `json:"branch"`
	BaseRef   string       `json:"baseRef,omitempty"`
	Added     int          `json:"added"`
	Deleted   int          `json:"deleted"`
	Ahead     int          `json:"ahead"`
	Changed   []FileChange `json:"changed,omitempty"`
	DirtyFile int          `json:"dirtyFiles"`
	Err       string       `json:"err,omitempty"`
}

// Stats reports, per repo in the workspace, the line churn since the task's base
// and the list of uncommitted files.
//
// "Since the task started" means against the merge-base with the recorded base
// ref, not against HEAD: that way committed work counts too, and the number does
// not silently shrink each time you commit. The base may have moved on since the
// worktree was made, so the merge-base — not the base tip — is the comparison
// point, or unrelated upstream commits would show up as your deletions.
func Stats(cfg *config.Config, slug string, baseRefs map[string]string) []RepoStat {
	var out []RepoStat
	for _, r := range Repos(cfg, slug) {
		dir := cfg.RepoDir(slug, r.Name)
		st := RepoStat{Repo: r.Name, BaseRef: baseRefs[r.Name]}
		if b, err := git(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			st.Branch = b
		}

		base := st.BaseRef
		if base == "" {
			base = DefaultBase(cfg.RepoDir(slug, r.Name))
		}
		if mb, err := git(dir, "merge-base", base, "HEAD"); err == nil && mb != "" {
			// Working tree vs merge-base covers committed AND uncommitted tracked
			// changes in one diff.
			if ns, err := git(dir, "diff", "--numstat", mb); err == nil {
				a, d := parseNumstat(ns)
				st.Added, st.Deleted = a, d
			}
			if n, err := git(dir, "rev-list", "--count", mb+"..HEAD"); err == nil {
				st.Ahead, _ = strconv.Atoi(strings.TrimSpace(n))
			}
		} else {
			st.Err = "cannot resolve base " + base
		}

		// Untracked files are invisible to `git diff`, but they are the whole
		// point of a new file — count every line as added.
		if others, err := git(dir, "ls-files", "--others", "--exclude-standard"); err == nil {
			for _, f := range strings.Split(others, "\n") {
				f = strings.TrimSpace(f)
				if f == "" {
					continue
				}
				st.Added += countLines(filepath.Join(dir, f))
			}
		}

		if porcelain, err := gitRaw(dir, "status", "--porcelain"); err == nil {
			for _, l := range strings.Split(porcelain, "\n") {
				if len(l) < 4 {
					continue
				}
				st.Changed = append(st.Changed, FileChange{
					Status: l[:2], Path: strings.TrimSpace(l[3:]),
				})
			}
			st.DirtyFile = len(st.Changed)
		}
		out = append(out, st)
	}
	return out
}

// Head is where a worktree stands: the commit it has checked out, and how far
// it is from the branch it tracks.
type Head struct {
	Commit  string `json:"commit,omitempty"`
	Subject string `json:"subject,omitempty"`
	// Upstream is the branch it tracks, "" when it tracks none — a branch that
	// was never pushed, or a detached HEAD.
	Upstream string `json:"upstream,omitempty"`
	// Ahead counts the commits not on the upstream yet, and Behind those on it
	// that are not here: both as of the last fetch, which this does not do.
	Ahead  int `json:"ahead,omitempty"`
	Behind int `json:"behind,omitempty"`
}

// HeadOf reads a worktree's Head. Whatever cannot be read is left empty: a
// repository with no commits has no subject, a local branch no upstream.
func HeadOf(dir string) Head {
	var h Head
	if out, err := git(dir, "log", "-1", "--format=%h%x09%s"); err == nil {
		h.Commit, h.Subject, _ = strings.Cut(out, "\t")
	}
	up, err := git(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil || up == "" {
		return h
	}
	h.Upstream = up
	if out, err := git(dir, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); err == nil {
		if f := strings.Fields(out); len(f) == 2 {
			h.Ahead, _ = strconv.Atoi(f[0])
			h.Behind, _ = strconv.Atoi(f[1])
		}
	}
	return h
}

func parseNumstat(s string) (added, deleted int) {
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		// binary files report "-" instead of a count
		if a, err := strconv.Atoi(f[0]); err == nil {
			added += a
		}
		if d, err := strconv.Atoi(f[1]); err == nil {
			deleted += d
		}
	}
	return added, deleted
}

// countLines is capped: a stray build artifact or lockfile should not turn the
// churn number into noise, nor cost a big read on every poll.
func countLines(path string) int {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() > 2<<20 {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// WriteGuide (re)writes the agent's guide file at the workspace root — for a
// workspace that arrived from another computer, whose guide named that
// computer's paths.
func WriteGuide(cfg *config.Config, slug string, repos []string, agentKind, branch string) error {
	specs := make([]RepoSpec, 0, len(repos))
	for _, r := range repos {
		specs = append(specs, RepoSpec{Name: r})
	}
	return writeGuide(cfg, slug, specs, agentKind, branch)
}

// CopyLocalConfig copies a repository's git-ignored local config from its main
// checkout into a worktree, as a new workspace does.
func CopyLocalConfig(cfg *config.Config, repo, dest string) (copied []string, notes []string) {
	r, ok := cfg.Repo(repo)
	if !ok {
		return nil, []string{"unknown repository " + repo}
	}
	return copyIncludes(cfg, repo, r.Main, dest)
}
