// Package review starts a reviewer for a task's merge requests: a skill
// written into the workspace, and the first message that points an agent at
// it. The skill is agent-neutral Markdown, so every agent CLI follows the same
// rubric; Claude Code also finds it as a skill of its own.
package review

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Icewater1337/fride/internal/config"
)

//go:embed skill.md
var skill string

// SkillPath is where the skill lives, relative to the workspace.
var SkillPath = filepath.Join(".claude", "skills", "review-merge-requests", "SKILL.md")

// Repo is one repository of the task, as the reviewer needs to know it.
type Repo struct {
	Name    string // also its folder in the workspace
	Branch  string
	BaseRef string
}

// Install writes the skill into the workspace, refreshing a copy written by
// an older FrIDE.
func Install(cfg *config.Config, slug string) error {
	path := filepath.Join(cfg.WorkspaceDir(slug), SkillPath)
	if cur, err := os.ReadFile(path); err == nil && string(cur) == skill {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(skill), 0o644)
}

// Prompt is the reviewer's first message. The repositories are spelled out
// so the agent starts from what FrIDE knows instead of rediscovering it.
func Prompt(repos []Repo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review the merge requests of this task. Follow %s exactly.\n\n", filepath.ToSlash(SkillPath))
	b.WriteString("Repositories (workspace folder: branch, branched from):\n")
	for _, r := range repos {
		fmt.Fprintf(&b, "- %s: %s, from %s\n", r.Name, orUnknown(r.Branch), orUnknown(r.BaseRef))
	}
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
