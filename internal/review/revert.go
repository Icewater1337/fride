package review

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/Icewater1337/fride/internal/config"
)

// RevertCheck is whether a repository's changed tests notice the change.
//
// A green test run proves the tests pass, not that they test anything: a test
// that passes with and without the fix holds nothing down. So the tests of
// the change are run against the code as it was at the merge-base. If they
// fail there, they depend on the change.
type RevertCheck struct {
	Repo     string    `json:"repo"`
	State    string    `json:"state"` // running, done, failed
	Command  string    `json:"command"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// Caught is the verdict: the tests failed without the change.
	Caught   bool     `json:"caught"`
	ExitCode int      `json:"exitCode"`
	Output   string   `json:"output,omitempty"` // the end of it
	Tests    []string `json:"tests,omitempty"`
	// Err says why the check could not run at all.
	Err    string `json:"err,omitempty"`
	Anchor Anchor `json:"anchor"`
	Stale  bool   `json:"stale,omitempty"` // computed when served
}

const revertFile = "revert.json"

// revertTimeout bounds one run: a full test suite, with whatever install the
// command starts with.
const revertTimeout = 20 * time.Minute

// LoadRevert reads the checks, by repo.
func LoadRevert(cfg *config.Config, slug string) map[string]RevertCheck {
	stateMu.Lock()
	defer stateMu.Unlock()
	return loadRevert(cfg, slug)
}

func loadRevert(cfg *config.Config, slug string) map[string]RevertCheck {
	out := map[string]RevertCheck{}
	load(cfg, slug, revertFile, &out)
	for repo, rc := range out {
		if rc.State == "running" && !isRunning("revert/"+slug+"/"+repo) {
			rc.State, rc.Err = "failed", "interrupted: FrIDE stopped while it ran"
			out[repo] = rc
		}
	}
	return out
}

func putRevert(cfg *config.Config, slug string, rc RevertCheck) {
	stateMu.Lock()
	defer stateMu.Unlock()
	all := loadRevert(cfg, slug)
	all[rc.Repo] = rc
	if err := save(cfg, slug, revertFile, all); err != nil {
		log.Printf("task %s: revert check not saved: %v", slug, err)
	}
}

// StartRevert runs the check for one repository in the background. command is
// the repo's test command from config.json; it runs in a scratch worktree, so
// it has to install what it needs ("npm ci && npm test").
func StartRevert(cfg *config.Config, slug string, spec RepoSpec, rc RepoChanges, command string) error {
	if command == "" {
		return fmt.Errorf("%s has no test command: set repos.%s.test in config.json", spec.Name, spec.Name)
	}
	if rc.MergeBase == "" {
		return fmt.Errorf("%s: %s", spec.Name, rc.Err)
	}
	var tests, gone []string
	for _, f := range rc.Files {
		if !IsTest(f.Path) {
			continue
		}
		if f.Status == "D" {
			gone = append(gone, f.Path)
		} else {
			tests = append(tests, f.Path)
		}
		if f.OldPath != "" && IsTest(f.OldPath) {
			gone = append(gone, f.OldPath)
		}
	}
	if len(tests) == 0 {
		return fmt.Errorf("no test file changed in %s, so there is nothing to check", spec.Name)
	}
	key := "revert/" + slug + "/" + spec.Name
	if !claim(key) {
		return fmt.Errorf("a revert check of %s is already running", spec.Name)
	}
	check := RevertCheck{Repo: spec.Name, State: "running", Command: command, Started: time.Now(),
		Tests: tests, Anchor: Anchor{Head: rc.Head, Hash: repoHash(rc)}}
	putRevert(cfg, slug, check)
	go func() {
		defer release(key)
		check.ExitCode, check.Output, check.Err = revertRun(spec.Dir, rc.MergeBase, tests, gone, command)
		check.State, check.Finished = "done", time.Now()
		if check.Err != "" {
			check.State = "failed"
		}
		check.Caught = check.Err == "" && check.ExitCode != 0
		putRevert(cfg, slug, check)
	}()
	return nil
}

// revertRun builds the scratch worktree (the merge-base, plus the tests as
// they are now), runs the command there, and removes it again.
func revertRun(dir, mergeBase string, tests, gone []string, command string) (exit int, output, errMsg string) {
	scratch, err := os.MkdirTemp("", "fride-revert-*")
	if err != nil {
		return 0, "", err.Error()
	}
	// The worktree goes, whatever happened; prune drops its registration if
	// the remove itself failed.
	defer func() {
		_, _ = git(dir, "worktree", "remove", "--force", scratch)
		_ = os.RemoveAll(scratch)
		_, _ = git(dir, "worktree", "prune")
	}()
	if _, err := git(dir, "worktree", "add", "--detach", scratch, mergeBase); err != nil {
		return 0, "", err.Error()
	}
	for _, p := range gone {
		_ = os.Remove(filepath.Join(scratch, filepath.FromSlash(p)))
	}
	for _, p := range tests {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return 0, "", fmt.Sprintf("reading %s: %v", p, err)
		}
		dst := filepath.Join(scratch, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, "", err.Error()
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return 0, "", err.Error()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), revertTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = scratch
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	output = tail(out.String(), 4000)
	if ctx.Err() != nil {
		return 0, output, fmt.Sprintf("timed out after %s", revertTimeout)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), output, ""
	}
	if err != nil {
		return 0, output, err.Error()
	}
	return 0, output, ""
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}
