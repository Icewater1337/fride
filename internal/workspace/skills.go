package workspace

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/journal"
)

// Skills of the user's own: folders in $FRIDE_HOME/skills, each a Claude Code
// skill (SKILL.md, and whatever else it needs beside it), linked into every
// workspace. FrIDE's own skills are not among them (see installBuiltins). The
// Settings page and `fride skill` manage them, and so does anyone editing the
// folder by hand.

// skillName is the name Claude Code accepts for a skill. It is also a folder
// name here, so it is the only thing that keeps a request inside SkillsDir.
var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// deletedDir is where a deleted skill goes: its folder may hold scripts the
// user wrote, so deleting moves it out of the way instead of erasing it.
const deletedDir = ".deleted"

// Skill is one of the user's skills, as the Settings page lists it.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Files       []string `json:"files"` // beside SKILL.md, relative to the skill's folder
}

// SkillsDir is where the user keeps skills of their own for every workspace:
// $FRIDE_HOME/skills/<name>/SKILL.md. They are the user's, not FrIDE's, so
// they live with config.json.
func SkillsDir(cfg *config.Config) string { return filepath.Join(cfg.StateDir, "skills") }

func skillDir(cfg *config.Config, name string) (string, error) {
	if !skillName.MatchString(name) {
		return "", fmt.Errorf("%q is not a skill name: lower-case letters, digits and hyphens, at most 64", name)
	}
	return filepath.Join(SkillsDir(cfg), name), nil
}

// Skills lists the user's skills: the folders of SkillsDir with a SKILL.md.
func Skills(cfg *config.Config) ([]Skill, error) {
	entries, err := os.ReadDir(SkillsDir(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return []Skill{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Skill{}
	for _, e := range entries {
		if !skillName.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(SkillsDir(cfg), e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
		if err != nil {
			continue
		}
		fm, _ := journal.ParseFrontmatter(string(b))
		sk := Skill{Name: e.Name(), Description: fm["description"], Files: []string{}}
		// DirFS, not WalkDir on the path: a skill folder may itself be a
		// link (to the user's dotfiles, say), and this follows it.
		_ = fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == "." || p == "SKILL.md" {
				return nil
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			sk.Files = append(sk.Files, p)
			return nil
		})
		out = append(out, sk)
	}
	return out, nil
}

// ReadSkill returns a skill's SKILL.md.
func ReadSkill(cfg *config.Config, name string) (string, error) {
	dir, err := skillDir(cfg, name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no skill %q", name)
	}
	return string(b), err
}

// ErrSkillExists refuses a new skill whose name is taken.
var ErrSkillExists = errors.New("a skill of that name exists already")

// WriteSkill writes a skill's SKILL.md: a new skill when create is set, which
// a taken name refuses rather than overwrites, else an existing one's. Claude
// Code names a skill by the name in its frontmatter when there is one, so a
// different one there is refused: it would make /<that name> a skill the list
// here calls something else.
func WriteSkill(cfg *config.Config, name, content string, create bool) error {
	dir, err := skillDir(cfg, name)
	if err != nil {
		return err
	}
	if fm, _ := journal.ParseFrontmatter(content); fm["name"] != "" && fm["name"] != name {
		return fmt.Errorf("SKILL.md names itself %q; the skill is %q, so the two must match", fm["name"], name)
	}
	_, err = os.Stat(filepath.Join(dir, "SKILL.md"))
	switch {
	case create && err == nil:
		return fmt.Errorf("%s: %w", name, ErrSkillExists)
	case !create && errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("no skill %q", name)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644)
}

// DeleteSkill moves a skill's folder into SkillsDir/.deleted, under its name
// and the time, and returns where it went. InstallSkills then drops its links.
func DeleteSkill(cfg *config.Config, name string) (string, error) {
	dir, err := skillDir(cfg, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dir); err != nil {
		return "", fmt.Errorf("no skill %q", name)
	}
	to := filepath.Join(SkillsDir(cfg), deletedDir, name+"-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(to); err == nil {
		return "", fmt.Errorf("%s exists already; try again in a second", to)
	}
	return to, os.Rename(dir, to)
}

// InstallSkills links each of the user's skills (SkillsDir) into the workspace
// as .claude/skills/<name>, where Claude Code finds project skills. A link and
// not a copy: an edit of the original reaches every workspace at once, and
// trash and move, which carry no symlinks, never take the user's files along.
// A link whose skill is gone is removed. A folder of the same name that is not
// such a link (one of FrIDE's own skills, or one made by hand) is left alone
// and wins. Idempotent: an agent start runs it too, which is how older
// workspaces and new skills meet.
func InstallSkills(cfg *config.Config, slug string) error {
	ws := cfg.WorkspaceDir(slug)
	if _, err := os.Stat(ws); err != nil {
		return err // a task whose workspace is gone gets none made by this
	}
	src := SkillsDir(cfg)
	entries, err := os.ReadDir(src)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // unreadable is not "none": no link is removed on a guess
	}
	want := map[string]bool{}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(src, e.Name(), "SKILL.md")); err == nil && skillName.MatchString(e.Name()) {
			want[e.Name()] = true
		}
	}
	dst := filepath.Join(ws, ".claude", "skills")
	have, _ := os.ReadDir(dst)
	for _, h := range have {
		p := filepath.Join(dst, h.Name())
		if target, err := os.Readlink(p); err == nil && filepath.Dir(target) == src && !want[h.Name()] {
			_ = os.Remove(p) // the link only, never what it pointed to
		}
	}
	var first error
	for name := range want {
		p := filepath.Join(dst, name)
		if _, err := os.Lstat(p); err == nil {
			continue // linked already, or taken by a skill that is not the user's
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.Symlink(filepath.Join(src, name), p); err != nil && first == nil {
			first = err
		}
	}
	if err := installBuiltins(dst); err != nil && first == nil {
		first = err
	}
	return first
}

// FrIDE's own skills, written into every workspace after the user's links:
// pr (commit, then `fride pr`), and pr-review (review the change, then pr) and
// watch-pr (the pipeline), both started only by the user. Each SKILL.md carries builtinMark, which is how a file FrIDE wrote
// is told from one somebody made: only the former is ever rewritten.
//
//go:embed builtin
var builtins embed.FS

const builtinMark = "<!-- FrIDE writes this file"

// installBuiltins writes FrIDE's skills into dst, and rewrites one FrIDE wrote
// that has changed since, so an older workspace gets the new version at its
// next agent start. A user's skill of the same name (a link) and a folder made
// by hand are left alone and win.
func installBuiltins(dst string) error {
	entries, err := builtins.ReadDir("builtin")
	if err != nil {
		return err
	}
	for _, e := range entries {
		body, err := builtins.ReadFile("builtin/" + e.Name() + "/SKILL.md")
		if err != nil {
			return err
		}
		dir := filepath.Join(dst, e.Name())
		if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, "SKILL.md")
		cur, err := os.ReadFile(path)
		switch {
		case err == nil && (bytes.Equal(cur, body) || !bytes.Contains(cur, []byte(builtinMark))):
			continue
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return err // unreadable is not "missing": nothing is overwritten on a guess
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}
