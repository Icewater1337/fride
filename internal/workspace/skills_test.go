package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
)

func TestInstallSkillsLinksTheUsersSkills(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir(), StateDir: t.TempDir()}
	src, dst := SkillsDir(cfg), filepath.Join(cfg.WorkspaceDir("t1"), ".claude", "skills")
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "e2e", "SKILL.md"), "user's e2e")
	write(filepath.Join(src, "ship", "SKILL.md"), "user's ship")
	write(filepath.Join(src, "notes", "README.md"), "not a skill")

	if err := InstallSkills(cfg, "t1"); err == nil {
		t.Fatal("a missing workspace was not refused")
	}
	if _, err := os.Stat(cfg.WorkspaceDir("t1")); err == nil {
		t.Fatal("a missing workspace was created")
	}
	// A skill of the same name that is not the user's link stays as it is.
	write(filepath.Join(dst, "ship", "SKILL.md"), "made by hand")
	for range 2 {
		if err := InstallSkills(cfg, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "e2e", "SKILL.md")); string(b) != "user's e2e" {
		t.Fatalf("e2e is not linked: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ship", "SKILL.md")); string(b) != "made by hand" {
		t.Fatalf("a skill that was not the user's link was replaced: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(dst, "notes")); err == nil {
		t.Fatal("a folder without SKILL.md was linked")
	}

	// While the skills folder cannot be read, nothing is removed.
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	err := InstallSkills(cfg, "t1")
	_ = os.Chmod(src, 0o755)
	if err == nil {
		t.Fatal("an unreadable skills folder was taken for an empty one")
	}
	if _, err := os.Lstat(filepath.Join(dst, "e2e")); err != nil {
		t.Fatal("a link was removed while the skills folder could not be read")
	}

	// A removed skill loses its link, and only the link.
	if err := os.RemoveAll(filepath.Join(src, "e2e")); err != nil {
		t.Fatal(err)
	}
	if err := InstallSkills(cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "e2e")); err == nil {
		t.Fatal("the link of a removed skill is still there")
	}
	if b, _ := os.ReadFile(filepath.Join(src, "ship", "SKILL.md")); string(b) != "user's ship" {
		t.Fatal("the user's own skill was touched")
	}
}
func TestSkillsAreWrittenListedAndDeleted(t *testing.T) {
	cfg := &config.Config{WorkspaceRoot: t.TempDir(), StateDir: t.TempDir()}
	if err := os.MkdirAll(cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: e2e\ndescription: Test it end to end.\n---\n\n# e2e\n"

	// Only a skill name, which is also what keeps a write inside the folder.
	for _, bad := range []string{"../e2e", "E2E", "a/b", ".deleted", ""} {
		if err := WriteSkill(cfg, bad, body, true); err == nil {
			t.Errorf("%q was accepted as a skill name", bad)
		}
	}
	if err := WriteSkill(cfg, "e2e", body, false); err == nil {
		t.Error("a change to a skill that does not exist was written")
	}
	if err := WriteSkill(cfg, "check", body, true); err == nil {
		t.Error("a skill whose SKILL.md names another skill was written")
	}
	if err := WriteSkill(cfg, "e2e", body, true); err != nil {
		t.Fatal(err)
	}
	if err := WriteSkill(cfg, "e2e", "changed", true); !errors.Is(err, ErrSkillExists) {
		t.Fatalf("a new skill over an existing one: %v", err)
	}
	if b, _ := ReadSkill(cfg, "e2e"); b != body {
		t.Fatalf("the taken name was overwritten: %q", b)
	}
	if err := os.WriteFile(filepath.Join(SkillsDir(cfg), "e2e", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	list, err := Skills(cfg)
	if err != nil || len(list) != 1 || list[0].Description != "Test it end to end." || strings.Join(list[0].Files, ",") != "run.sh" {
		t.Fatalf("skills = %+v, %v", list, err)
	}
	if err := InstallSkills(cfg, "t1"); err != nil {
		t.Fatal(err)
	}

	// Deleting keeps the folder, out of the way, and the link goes.
	to, err := DeleteSkill(cfg, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "run.sh")); string(b) != "#!/bin/sh\n" {
		t.Fatal("the deleted skill's files were not kept")
	}
	if list, _ := Skills(cfg); len(list) != 0 {
		t.Fatalf("a deleted skill is still listed: %+v", list)
	}
	if err := InstallSkills(cfg, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(cfg.WorkspaceDir("t1"), ".claude", "skills", "e2e")); err == nil {
		t.Fatal("a deleted skill is still linked")
	}
}
