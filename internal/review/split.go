package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A change past either size is too large to review in one sitting: defect
// detection drops sharply after a few hundred lines.
const (
	largeLines = 400
	largeFiles = 25
)

// Size is how much of a change a reader has to get through: folded noise
// does not count.
type Size struct {
	Lines int  `json:"lines"`
	Files int  `json:"files"`
	Large bool `json:"large"`
}

func sizeOf(repos []RepoChanges) Size {
	var s Size
	for _, rc := range repos {
		for _, f := range rc.Files {
			if f.Noise == "" {
				s.Files++
				s.Lines += f.Added + f.Deleted
			}
		}
	}
	s.Large = s.Lines > largeLines || s.Files > largeFiles
	return s
}

// Part is one commit of a split: a title and the files it takes, whole.
type Part struct {
	Title string   `json:"title"`
	Files []string `json:"files"`
}

// Stack is a change split into commits on a branch of its own.
type Stack struct {
	Repo    string        `json:"repo"`
	Branch  string        `json:"branch"`
	Base    string        `json:"base"`
	Commits []StackCommit `json:"commits"`
}

// StackCommit is one commit of a Stack.
type StackCommit struct {
	Title string   `json:"title"`
	Files []string `json:"files"`
	SHA   string   `json:"sha"`
}

var layerTitles = map[string]string{
	"schema": "Change the schema", "logic": "Change the logic", "api": "Change the interface",
	"consumer": "Update the callers", "test": "Cover it with tests", "other": "Update docs and build files",
}

// Plan says how one repository's change would split: by the AI pass's intent
// groups while they describe the change as it is, by tour layer otherwise.
// A file goes whole to the group holding most of its hunks; the parts are
// ordered by layer, so the schema comes before what uses it.
//
// ponytail: whole files per commit. A file whose hunks serve two intents lands
// in one of them; splitting hunks needs a patch per commit and a check that each applies.
func Plan(rc RepoChanges, groups []Group) []Part {
	groupOf := map[string]int{}
	for gi, g := range groups {
		for _, id := range g.Hunks {
			groupOf[id] = gi
		}
	}
	type part struct {
		Part
		rank int
	}
	var parts []*part
	byKey := map[string]*part{}
	get := func(key, title string) *part {
		if p, ok := byKey[key]; ok {
			return p
		}
		p := &part{Part: Part{Title: title}, rank: len(Layers)}
		byKey[key] = p
		parts = append(parts, p)
		return p
	}
	for _, f := range rc.Files {
		votes := map[int]int{}
		best := -1
		for _, h := range f.Hunks {
			if gi, ok := groupOf[h.ID]; ok {
				votes[gi]++
				if best < 0 || votes[gi] > votes[best] || votes[gi] == votes[best] && gi < best {
					best = gi
				}
			}
		}
		layer := layerOf(f.Path)
		var p *part
		if best >= 0 {
			p = get(fmt.Sprintf("group/%d", best), groups[best].Title)
		} else {
			p = get("layer/"+layer, layerTitles[layer])
		}
		p.Files = append(p.Files, f.Path)
		p.rank = min(p.rank, layerRank(layer))
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].rank < parts[j].rank })
	out := make([]Part, len(parts))
	for i, p := range parts {
		out[i] = p.Part
	}
	return out
}

// Split writes the parts as a stack of commits on a new branch, <branch>-stack,
// starting at the merge-base. The task's own branch and working tree are not
// touched, and the branch is created only once the stack's last tree is
// identical to the working tree: a split that lost a line is no split.
func Split(dir string, rc RepoChanges, parts []Part) (Stack, error) {
	st := Stack{Repo: rc.Repo, Base: rc.MergeBase}
	if rc.MergeBase == "" {
		return st, fmt.Errorf("%s: %s", rc.Repo, rc.Err)
	}
	files := map[string]File{}
	for _, f := range rc.Files {
		// Committing a .env would put a credential in history, where it is
		// much harder to take back than to leave out now.
		if f.Withheld {
			return st, fmt.Errorf("%s holds credentials; add it to .gitignore before splitting", f.Path)
		}
		files[f.Path] = f
	}
	st.Branch = rc.Branch + "-stack"
	if rc.Branch == "" || rc.Branch == "HEAD" {
		st.Branch = "fride-stack-" + rc.Head[:min(8, len(rc.Head))]
	}
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+st.Branch); err == nil {
		return st, fmt.Errorf("branch %s already exists; delete or rename it to split again", st.Branch)
	}

	tmp, err := os.MkdirTemp("", "fride-split-*")
	if err != nil {
		return st, err
	}
	defer os.RemoveAll(tmp)
	withIndex := func(name string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(tmp, name))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}

	// What the stack has to end at: the working tree as `git add -A` sees it,
	// built in an index of its own so the real one is left alone.
	if _, err := withIndex("want", "read-tree", "HEAD"); err != nil {
		return st, err
	}
	if _, err := withIndex("want", "add", "-A"); err != nil {
		return st, err
	}
	want, err := withIndex("want", "write-tree")
	if err != nil {
		return st, err
	}

	if _, err := withIndex("stack", "read-tree", rc.MergeBase); err != nil {
		return st, err
	}
	parent, tree := rc.MergeBase, ""
	for _, p := range parts {
		if len(p.Files) == 0 {
			continue
		}
		for _, path := range p.Files {
			f, ok := files[path]
			if !ok {
				return st, fmt.Errorf("%s is not part of the change", path)
			}
			if f.OldPath != "" {
				if _, err := withIndex("stack", "update-index", "--force-remove", "--", f.OldPath); err != nil {
					return st, err
				}
			}
			args := []string{"update-index", "--add", "--", path}
			if f.Status == "D" {
				args = []string{"update-index", "--force-remove", "--", path}
			}
			if _, err := withIndex("stack", args...); err != nil {
				return st, err
			}
		}
		if tree, err = withIndex("stack", "write-tree"); err != nil {
			return st, err
		}
		sha, err := git(dir, "commit-tree", tree, "-p", parent, "-m", p.Title)
		if err != nil {
			return st, err
		}
		parent = sha
		st.Commits = append(st.Commits, StackCommit{Title: p.Title, Files: p.Files, SHA: sha})
	}
	if tree != want {
		return st, fmt.Errorf("the stack does not end at the working tree (%s, want %s), so no branch was made",
			short(tree), short(want))
	}
	// An empty old value makes git refuse if the branch appeared meanwhile.
	if _, err := git(dir, "update-ref", "refs/heads/"+st.Branch, parent, ""); err != nil {
		return st, err
	}
	return st, nil
}

func short(s string) string { return s[:min(10, len(s))] }
