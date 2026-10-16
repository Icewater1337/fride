package journal

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// The ledger is a git repository collecting every task's rationale document,
// configured in config.json (settings.Ledger). Publishing only uses git: the
// same credential that pushes code is enough, and a server that understands
// push options (GitLab) can be asked to open a merge request and merge it by
// itself — no API token anywhere.
//
// The repository is an Open Knowledge Format bundle: documents by year under
// <dir>/, and the reserved index.md and log.md at the root, rebuilt on every
// publish so the ledger stays navigable.

// ErrNoLedger is returned when config.json configures no ledger.
var ErrNoLedger = fmt.Errorf("no ledger is configured (see `ledger` in docs/CONFIG.md)")

// LedgerEnabled reports whether a ledger is configured.
func LedgerEnabled(cfg *config.Config) bool {
	return cfg.Settings != nil && cfg.Settings.Ledger != nil && strings.TrimSpace(cfg.Settings.Ledger.Remote) != ""
}

// PublishOnDone reports whether closing a task publishes its document.
func PublishOnDone(cfg *config.Config) bool {
	if !LedgerEnabled(cfg) {
		return false
	}
	l := cfg.Settings.Ledger
	return l.OnDone == nil || *l.OnDone
}

type ledgerConf struct {
	remote, branch, dir, push string
	options                   []string
	wait                      time.Duration
	env                       []string
}

func ledgerOf(cfg *config.Config) (*ledgerConf, error) {
	if !LedgerEnabled(cfg) {
		return nil, ErrNoLedger
	}
	l := cfg.Settings.Ledger
	c := &ledgerConf{remote: l.Remote, branch: l.Branch, dir: l.Dir, push: l.Push,
		options: l.PushOptions, wait: time.Duration(l.WaitForMerge) * time.Second}
	if c.branch == "" {
		c.branch = "main"
	}
	if c.dir == "" {
		c.dir = "rationale"
	}
	if c.push == "" {
		c.push = "branch"
	}
	env, err := authorEnv(l.Author, "ledger.author")
	if err != nil {
		return nil, err
	}
	c.env = env
	return c, nil
}

var authorParts = regexp.MustCompile(`^([^<>]+)<([^<>\s]+@[^<>\s]+)>$`)

// authorEnv turns "Name <email>" into the environment that makes it a
// commit's author and committer. Empty means git's own configuration.
func authorEnv(author, field string) ([]string, error) {
	if author == "" {
		return nil, nil
	}
	m := authorParts.FindStringSubmatch(author)
	if m == nil {
		return nil, fmt.Errorf("%s must be \"Name <email>\"", field)
	}
	name, email := strings.TrimSpace(m[1]), m[2]
	return []string{"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email}, nil
}

// The clones are kept with FrIDE's other state, so publishing never touches
// any of the task's own worktrees.
func ledgerClone(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "ledger") }

// credentialsRe finds credentials embedded in URLs, which git repeats in its
// error messages when the remote is written that way.
var credentialsRe = regexp.MustCompile(`(https?://)[^/@\s]+@`)

func redact(s string) string { return credentialsRe.ReplaceAllString(s, "$1") }

func run(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// No prompt: a missing credential must fail, not hang a background publish.
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0"}, env...)...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		safe := make([]string, len(args))
		for i, a := range args {
			safe[i] = redact(a)
		}
		return s, fmt.Errorf("git %s: %s", strings.Join(safe, " "), redact(s))
	}
	return s, nil
}

// ensureClone makes sure the local clone in dir exists, points at the
// configured remote, and is on a current main branch.
func ensureClone(dir, remote, branch string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if _, err := run("", nil, "clone", "--quiet", remote, dir); err != nil {
			return "", fmt.Errorf("could not clone %s: %w", redact(remote), err)
		}
	} else if _, err := run(dir, nil, "remote", "set-url", "origin", remote); err != nil {
		return "", err
	}
	if _, err := run(dir, nil, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return "", err
	}
	// A brand-new, empty ledger has no main branch yet; the first publish
	// creates it.
	if _, err := run(dir, nil, "rev-parse", "--verify", "--quiet", "origin/"+branch); err != nil {
		if _, err := run(dir, nil, "checkout", "--quiet", "--orphan", branch); err != nil {
			if _, err := run(dir, nil, "checkout", "--quiet", branch); err != nil {
				return "", err
			}
		}
		_, _ = run(dir, nil, "rm", "-rf", "--quiet", "--ignore-unmatch", ".")
		return dir, nil
	}
	// Reset rather than pull: this clone is a publishing scratch area, and any
	// local state in it is from a previous publish, not work worth keeping.
	if _, err := run(dir, nil, "checkout", "--quiet", "-B", branch, "origin/"+branch); err != nil {
		return "", err
	}
	if _, err := run(dir, nil, "reset", "--quiet", "--hard", "origin/"+branch); err != nil {
		return "", err
	}
	return dir, nil
}

// Published is what a publish did.
type Published struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
	// Link is the merge or pull request the server reported, if any.
	Link    string `json:"link,omitempty"`
	Skipped string `json:"skipped,omitempty"`
	Waiting string `json:"waiting,omitempty"`
}

// title is the document's human name, falling back to the slug.
func title(opt Options, slug string) string {
	if opt.Name != "" {
		return opt.Name
	}
	return slug
}

// writeLedgerIndex rebuilds the bundle root index, grouped by year. This is the
// one index the spec lets carry frontmatter, and only to declare the version.
func writeLedgerIndex(dir, sub string) error {
	entries, err := ScanBundle(dir, sub)
	if err != nil {
		return err
	}
	byYear := map[string][]IndexEntry{}
	var years []string
	for _, e := range entries {
		parts := strings.Split(e.Path, "/")
		y := "undated"
		if len(parts) >= 2 {
			y = parts[len(parts)-2]
		}
		if _, seen := byYear[y]; !seen {
			years = append(years, y)
		}
		byYear[y] = append(byYear[y], e)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(years)))
	sections := make([]IndexSection, 0, len(years))
	for _, y := range years {
		sections = append(sections, IndexSection{Heading: y, Entries: byYear[y]})
	}
	body := RenderIndex(
		"Design rationale recorded while the work happened, one document per task.",
		OKFVersion, sections)
	return os.WriteFile(filepath.Join(dir, "index.md"), []byte(body), 0o644)
}

func appendLedgerLog(dir, rel, name string, existed bool) error {
	path := filepath.Join(dir, "log.md")
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	verb, what := "Create", "Recorded the"
	if existed {
		verb, what = "Update", "Updated the"
	}
	line := fmt.Sprintf("**%s**: %s rationale for [%s](/%s).", verb, what, name, filepath.ToSlash(rel))
	body := MergeLog(string(old), time.Now().Format("2006-01-02"), []string{line})
	return os.WriteFile(path, []byte(body), 0o644)
}

// generatedRe matches the lines of a document that only say when it was made.
var generatedRe = regexp.MustCompile(`(?m)^(timestamp:.*|> Generated by \S+ on .*)$`)

// sameDocument compares two renderings of a document, ignoring when each was
// generated.
func sameDocument(a, b string) bool {
	norm := func(s string) string { return strings.TrimSpace(generatedRe.ReplaceAllString(s, "")) }
	return norm(a) == norm(b)
}

// linkRe finds the merge or pull request URL a server prints after a push.
var linkRe = regexp.MustCompile(`https?://\S+/(?:-/)?(?:merge_requests|pull)/(?:new/)?\S*`)

// publishMu serialises publishes in this process. Closing several tasks in a
// row starts one publish each; based on the same main, every merge request
// after the first would conflict on the shared index.md and log.md — a state
// a server never auto-merges out of. With WaitForMerge each publish also holds
// the lock until its own merge has reached main.
var publishMu sync.Mutex

// Publish writes the task's document into the ledger and pushes it.
func Publish(cfg *config.Config, slug string, opt Options) (*Published, error) {
	c, err := ledgerOf(cfg)
	if err != nil {
		return nil, err
	}
	publishMu.Lock()
	defer publishMu.Unlock()

	doc, err := Compose(cfg, slug, opt)
	if err != nil {
		return nil, err
	}
	dir, err := ensureClone(ledgerClone(cfg), c.remote, c.branch)
	if err != nil {
		return nil, err
	}
	topic := "rationale/" + slug
	if c.push == "branch" {
		if _, err := run(dir, nil, "checkout", "--quiet", "-B", topic); err != nil {
			return nil, err
		}
	}

	// By year: a flat folder of every task ever is unreadable after the first
	// hundred.
	rel := filepath.Join(c.dir, time.Now().Format("2006"), slug+".md")
	full := filepath.Join(dir, rel)
	_, statErr := os.Stat(full)
	existed := statErr == nil

	// Nothing to say is a normal outcome when a task is re-closed unchanged.
	// What counts is the last version sent — the task's open branch if its
	// merge is still pending, else main — and the generation time does not.
	prev := ""
	if c.push == "branch" {
		if out, err := run(dir, nil, "show", "origin/"+topic+":"+filepath.ToSlash(rel)); err == nil {
			prev = out
		}
	}
	if prev == "" {
		if b, err := os.ReadFile(full); err == nil {
			prev = string(b)
		}
	}
	if prev != "" && sameDocument(prev, doc) {
		return &Published{Path: filepath.ToSlash(rel), Skipped: "the document is unchanged since the last publish"}, nil
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, []byte(doc), 0o644); err != nil {
		return nil, err
	}
	if err := writeLedgerIndex(dir, c.dir); err != nil {
		return nil, err
	}
	if err := appendLedgerLog(dir, rel, title(opt, slug), existed); err != nil {
		return nil, err
	}
	if _, err := run(dir, nil, "add", rel, "index.md", "log.md"); err != nil {
		return nil, err
	}
	p := &Published{Path: filepath.ToSlash(rel)}
	if _, err := run(dir, nil, "diff", "--cached", "--quiet"); err == nil {
		p.Skipped = "the document is unchanged since the last publish"
		return p, nil
	}
	if _, err := run(dir, c.env, "commit", "--quiet", "-m", "docs: record design rationale for "+slug); err != nil {
		if strings.Contains(err.Error(), "identity unknown") {
			return nil, fmt.Errorf("git does not know who is committing to the ledger — set ledger.author in config.json (\"Name <email>\")")
		}
		return nil, err
	}

	if c.push == "direct" {
		p.Branch = c.branch
		// Someone else may have published meanwhile: rebase and retry once.
		if _, err := run(dir, nil, "push", "--quiet", "origin", "HEAD:"+c.branch); err != nil {
			if _, rerr := run(dir, c.env, "pull", "--quiet", "--rebase", "origin", c.branch); rerr != nil {
				return nil, err
			}
			if _, err := run(dir, nil, "push", "--quiet", "origin", "HEAD:"+c.branch); err != nil {
				return nil, err
			}
		}
		return p, nil
	}

	p.Branch = topic
	args := []string{"push", "--force-with-lease", "-u", "origin", "HEAD:" + topic}
	fill := strings.NewReplacer("{{task}}", slug, "{{title}}", title(opt, slug), "{{branch}}", topic, "{{target}}", c.branch)
	for _, o := range c.options {
		args = append(args, "-o", fill.Replace(o))
	}
	out, err := run(dir, nil, args...)
	if err != nil {
		return nil, err
	}
	if m := linkRe.FindString(out); m != "" {
		p.Link = m
	}
	if c.wait <= 0 {
		return p, nil
	}
	// Hold the lock until the merge has reached main, so the next publish
	// bases on a main that contains this one.
	head, err := run(dir, nil, "rev-parse", "HEAD")
	if err != nil {
		return p, nil
	}
	for deadline := time.Now().Add(c.wait); time.Now().Before(deadline); {
		time.Sleep(3 * time.Second)
		if _, err := run(dir, nil, "fetch", "--quiet", "origin", c.branch); err != nil {
			break
		}
		if _, err := run(dir, nil, "merge-base", "--is-ancestor", head, "origin/"+c.branch); err == nil {
			return p, nil
		}
	}
	p.Waiting = fmt.Sprintf("pushed, but not merged into %s after %s", c.branch, c.wait)
	log.Printf("ledger: %s %s — check %s", topic, p.Waiting, p.Link)
	return p, nil
}

// CheckRemote reports whether a publishing repository (the ledger, the ideas
// repository) can be reached with git's own credentials, for `fride doctor`.
func CheckRemote(remote string) error {
	cmd := exec.Command("git", "ls-remote", "--quiet", "--exit-code", remote, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		return nil // reachable but empty: the first publish creates its main branch
	}
	msg := strings.TrimSpace(redact(string(out)))
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s", msg)
}
