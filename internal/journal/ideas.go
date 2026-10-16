package journal

import (
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Idea pages are the ledger's counterpart for exploration. While a task tries
// something new rather than implementing a ticket, its agent keeps a page
// about the idea (the document-idea skill, installed into the workspace), and
// the person publishes it to the team's ideas repository (config.json:
// ideas).
//
// The page lives in <workspace>/.idea-brain/ in exactly the repository's
// layout (<dir>/<slug>.md, assets beside it), so publishing is a copy onto a
// branch of its own, plus the page's entry in the root index.md, pushed with
// the configured push options — plain git, the same as the ledger. The same
// clone answers "which ideas exist already?" for the skill, so an idea is
// extended rather than duplicated.

//go:embed idea_skill.md
var ideaSkill string

// IdeaFolder is where the skill writes inside a workspace.
const IdeaFolder = ".idea-brain"

// ErrNoIdeas is returned when config.json configures no ideas repository.
var ErrNoIdeas = fmt.Errorf("no ideas repository is configured (see `ideas` in docs/CONFIG.md)")

// IdeasEnabled reports whether an ideas repository is configured.
func IdeasEnabled(cfg *config.Config) bool {
	return cfg.Settings != nil && cfg.Settings.Ideas != nil && strings.TrimSpace(cfg.Settings.Ideas.Remote) != ""
}

// IdeaDir is the workspace folder the idea pages are written to.
func IdeaDir(cfg *config.Config, slug string) string {
	return filepath.Join(cfg.WorkspaceDir(slug), IdeaFolder)
}

// ideasSub is the repository folder holding one page per idea.
func ideasSub(cfg *config.Config) string {
	if cfg.Settings != nil && cfg.Settings.Ideas != nil && cfg.Settings.Ideas.Dir != "" {
		return filepath.ToSlash(filepath.Clean(cfg.Settings.Ideas.Dir))
	}
	return "ideas"
}

// InstallIdeaSkill drops the document-idea skill where Claude Code finds
// project skills; the guide points other agents at the same file. Idempotent,
// so it runs on every task open as well as at creation, which is how older
// workspaces pick it up — and how a changed skill reaches them.
func InstallIdeaSkill(cfg *config.Config, slug string) error {
	if !IdeasEnabled(cfg) {
		return nil
	}
	dir := filepath.Join(cfg.WorkspaceDir(slug), ".claude", "skills", "document-idea")
	body := strings.ReplaceAll(ideaSkill, "{{dir}}", ideasSub(cfg))
	path := filepath.Join(dir, "SKILL.md")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == body {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// IdeaPage is one idea page, read from its frontmatter.
type IdeaPage struct {
	Path        string `json:"path"` // relative to the repository root, e.g. ideas/dynamic-dashboards.md
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// IdeaPages lists the idea pages the skill has written in the workspace.
func IdeaPages(cfg *config.Config, slug string) ([]IdeaPage, error) {
	sub := ideasSub(cfg)
	return pagesIn(filepath.Join(IdeaDir(cfg, slug), filepath.FromSlash(sub)), sub)
}

func pagesIn(dir, sub string) ([]IdeaPage, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []IdeaPage{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []IdeaPage{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" || e.Name() == "index.md" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		fm, _ := ParseFrontmatter(string(b))
		title := fm["title"]
		if title == "" {
			title = strings.TrimSuffix(e.Name(), ".md")
		}
		out = append(out, IdeaPage{Path: sub + "/" + e.Name(), Title: title, Description: fm["description"], Status: fm["status"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type ideasConf struct {
	remote, branch, sub string
	options, env        []string
}

func ideasOf(cfg *config.Config) (*ideasConf, error) {
	if !IdeasEnabled(cfg) {
		return nil, ErrNoIdeas
	}
	i := cfg.Settings.Ideas
	c := &ideasConf{remote: i.Remote, branch: i.Branch, sub: ideasSub(cfg), options: i.PushOptions}
	if c.branch == "" {
		c.branch = "main"
	}
	env, err := authorEnv(i.Author, "ideas.author")
	if err != nil {
		return nil, err
	}
	c.env = env
	return c, nil
}

func ideasClone(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "ideas") }

// ExistingIdeas lists the ideas already in the repository's main branch, so a
// page written in a task can extend one instead of duplicating it.
func ExistingIdeas(cfg *config.Config) ([]IdeaPage, error) {
	c, err := ideasOf(cfg)
	if err != nil {
		return nil, err
	}
	ideaMu.Lock()
	defer ideaMu.Unlock()
	dir, err := ensureClone(ideasClone(cfg), c.remote, c.branch)
	if err != nil {
		return nil, err
	}
	return pagesIn(filepath.Join(dir, filepath.FromSlash(c.sub)), c.sub)
}

// ReadIdea returns one page of the repository's main branch, e.g.
// ideas/dynamic-dashboards.md.
func ReadIdea(cfg *config.Config, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("not a path inside the ideas repository: %s", rel)
	}
	c, err := ideasOf(cfg)
	if err != nil {
		return "", err
	}
	ideaMu.Lock()
	defer ideaMu.Unlock()
	dir, err := ensureClone(ideasClone(cfg), c.remote, c.branch)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// IdeasPublished is what a publish did.
type IdeasPublished struct {
	Branch  string   `json:"branch"`
	Files   []string `json:"files"`
	Link    string   `json:"link,omitempty"`
	Skipped string   `json:"skipped,omitempty"`
}

// ideaMu serialises use of the one clone: a publish resets and switches its
// branch, which must not happen under a concurrent `idea ls`.
var ideaMu sync.Mutex

// PublishIdeas copies the workspace's idea pages and assets into the ideas
// repository on a branch of their own and pushes it.
func PublishIdeas(cfg *config.Config, slug string) (*IdeasPublished, error) {
	c, err := ideasOf(cfg)
	if err != nil {
		return nil, err
	}
	pages, err := IdeaPages(cfg, slug)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no idea page in %s — the agent writes one there when the task is exploring an idea",
			filepath.Join(IdeaDir(cfg, slug), c.sub))
	}
	ideaMu.Lock()
	defer ideaMu.Unlock()
	dir, err := ensureClone(ideasClone(cfg), c.remote, c.branch)
	if err != nil {
		return nil, err
	}
	// A new branch per publish: each one is its own merge request, so
	// publishing again after the first was merged never fights a stale branch.
	branch := "idea/" + slug + "-" + time.Now().UTC().Format("20060102-150405")
	if _, err := run(dir, nil, "checkout", "--quiet", "-B", branch); err != nil {
		return nil, err
	}

	// Everything under .idea-brain/ is laid out as the repository expects; copy
	// it as is. Regular files only: a symlink the agent left there would
	// otherwise publish whatever it points at.
	src := IdeaDir(cfg, slug)
	var files []string
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return nil, err
	}
	if err := updateIdeaIndex(dir, c.sub, pages); err != nil {
		return nil, err
	}
	if _, err := run(dir, nil, "add", "-A"); err != nil {
		return nil, err
	}
	p := &IdeasPublished{Files: files}
	if _, err := run(dir, nil, "diff", "--cached", "--quiet"); err == nil {
		p.Skipped = "the idea pages are unchanged since the last publish"
		return p, nil
	}

	titles := make([]string, 0, len(pages))
	for _, pg := range pages {
		titles = append(titles, pg.Title)
	}
	title := strings.Join(titles, ", ")
	if _, err := run(dir, c.env, "commit", "--quiet", "-m", fmt.Sprintf("idea: %s (from task %s)", title, slug)); err != nil {
		if strings.Contains(err.Error(), "identity unknown") {
			return nil, fmt.Errorf("git does not know who is committing to the ideas repository — set ideas.author in config.json (\"Name <email>\")")
		}
		return nil, err
	}
	p.Branch = branch
	args := []string{"push", "-u", "origin", "HEAD:" + branch}
	fill := strings.NewReplacer("{{task}}", slug, "{{title}}", title, "{{branch}}", branch, "{{target}}", c.branch)
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
	return p, nil
}

// updateIdeaIndex keeps the root index.md listing the pages: one line each,
// `- [Title](path) — description`, under a `## <dir>` heading, replaced in
// place when the page is listed already. Edited rather than regenerated: the
// repository may be maintained by other tools too, and its other sections are
// theirs.
func updateIdeaIndex(dir, sub string, pages []IdeaPage) error {
	path := filepath.Join(dir, "index.md")
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		b = []byte("---\nokf_version: \"" + OKFVersion + "\"\n---\n\n# Index\n\n## " + sub + "\n")
	}
	lines := strings.Split(string(b), "\n")
	for _, p := range pages {
		line := "- [" + p.Title + "](" + p.Path + ")"
		if p.Description != "" {
			line += " — " + p.Description
		}
		replaced := false
		for i, l := range lines {
			if strings.Contains(l, "]("+p.Path+")") {
				lines[i], replaced = line, true
				break
			}
		}
		if !replaced {
			lines = insertUnder(lines, "## "+sub, line)
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// insertUnder appends line after the last `- ` entry of the section, creating
// the section at the end when it is missing.
func insertUnder(lines []string, heading, line string) []string {
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start == -1 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		return append(lines, heading, "", line, "")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	at := start + 1
	if at < len(lines) && strings.TrimSpace(lines[at]) == "" {
		at++
	}
	for i := start + 1; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
			at = i + 1
		}
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, line)
	return append(out, lines[at:]...)
}
