package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

func TestIdeaIndexIsEditedInPlace(t *testing.T) {
	dir := t.TempDir()
	index := "---\nokf_version: \"0.1\"\n---\n\n# Index\n\n## Brain\n\n- [Overview](overview.md) — hub\n\n## ideas\n\n- [Old](ideas/old.md) — stale description\n\n## people\n"
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	pages := []IdeaPage{
		{Path: "ideas/old.md", Title: "Old idea", Description: "fresh description"},
		{Path: "ideas/new.md", Title: "New idea", Description: "just published"},
	}
	if err := updateIdeaIndex(dir, "ideas", pages); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "index.md"))
	s := string(got)
	if !strings.Contains(s, "- [Old idea](ideas/old.md) — fresh description\n- [New idea](ideas/new.md) — just published\n\n## people") {
		t.Fatalf("index not updated in place under ## ideas:\n%s", s)
	}
	if strings.Count(s, "ideas/old.md") != 1 || !strings.Contains(s, "- [Overview](overview.md) — hub") {
		t.Fatalf("other entries disturbed:\n%s", s)
	}
}

// ideasCfg is a FrIDE with one task and a bare repository, seeded with one
// idea, as its ideas repository.
func ideasCfg(t *testing.T, dir string) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "ideas.git")
	gitT(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitT(t, bare, "config", "receive.advertisePushOptions", "true")
	seed := filepath.Join(root, "seed")
	gitT(t, root, "init", "-q", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	page := "---\ntype: idea\ntitle: \"Old idea\"\ndescription: \"already here\"\nstatus: draft\n---\n\n# body\n"
	if err := os.WriteFile(filepath.Join(seed, dir, "old.md"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", "-A")
	gitT(t, seed, "commit", "-qm", "init")
	gitT(t, seed, "push", "-q", bare, "main")

	s := &settings.Settings{Version: settings.Version, Ideas: &settings.Ideas{
		Remote: bare, Dir: dir, Author: "Jane Doe <jane@example.com>",
		PushOptions: []string{"merge_request.create", "merge_request.title=idea: {{title}}"},
	}}
	cfg := &config.Config{Home: root, StateDir: filepath.Join(root, "state"), Settings: s,
		WorkspaceRoot: filepath.Join(root, "ws")}
	if err := os.MkdirAll(cfg.WorkspaceDir("spike"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, bare
}

func TestIdeasRoundTrip(t *testing.T) {
	cfg, bare := ideasCfg(t, "proposals")

	// The skill names the configured folder, not a default.
	if err := InstallIdeaSkill(cfg, "spike"); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(cfg.WorkspaceDir("spike"), ".claude", "skills", "document-idea", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), ".idea-brain/proposals/<slug>.md") || strings.Contains(string(skill), "{{dir}}") {
		t.Fatalf("skill not installed for the configured folder: %v", err)
	}

	existing, err := ExistingIdeas(cfg)
	if err != nil || len(existing) != 1 || existing[0].Path != "proposals/old.md" || existing[0].Title != "Old idea" {
		t.Fatalf("existing ideas: %+v %v", existing, err)
	}
	if body, err := ReadIdea(cfg, "proposals/old.md"); err != nil || !strings.Contains(body, "already here") {
		t.Fatalf("read: %q %v", body, err)
	}
	if _, err := ReadIdea(cfg, "../state/x"); err == nil {
		t.Fatal("a path outside the repository was read")
	}

	if _, err := PublishIdeas(cfg, "spike"); err == nil {
		t.Fatal("published with no idea page")
	}
	brain := IdeaDir(cfg, "spike")
	for path, body := range map[string]string{
		"proposals/dashboards.md":      "---\ntype: idea\ntitle: Dynamic dashboards\ndescription: widgets from prose\nstatus: draft\n---\n\n# What\n",
		"references/assets/sketch.png": "png",
		".scratch/notes.md":            "never published",
		"proposals/.hidden-draft.md":   "never published",
	} {
		full := filepath.Join(brain, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink must not smuggle a file from elsewhere into the repository.
	secret := filepath.Join(cfg.Home, "secret.txt")
	_ = os.WriteFile(secret, []byte("token"), 0o600)
	if err := os.Symlink(secret, filepath.Join(brain, "references", "assets", "link.txt")); err != nil {
		t.Fatal(err)
	}

	p, err := PublishIdeas(cfg, "spike")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Branch, "idea/spike-") || p.Skipped != "" {
		t.Fatalf("unexpected publish %+v", p)
	}
	if strings.Join(p.Files, ",") != "proposals/dashboards.md,references/assets/sketch.png" {
		t.Fatalf("published files: %v", p.Files)
	}
	index := gitT(t, bare, "show", p.Branch+":index.md")
	if !strings.Contains(index, "## proposals") || !strings.Contains(index, "- [Dynamic dashboards](proposals/dashboards.md) — widgets from prose") {
		t.Fatalf("index:\n%s", index)
	}
	if author := gitT(t, bare, "log", "-1", "--format=%an <%ae>", p.Branch); author != "Jane Doe <jane@example.com>" {
		t.Fatalf("author %q", author)
	}
	if main := gitT(t, bare, "rev-parse", "main"); main == gitT(t, bare, "rev-parse", p.Branch) {
		t.Fatal("published onto main instead of its own branch")
	}
}
