package quality

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Everything here reads git, and nothing but git: the base branch as it was
// last fetched is enough to see a merge, a revert and a fix. No forge API, so
// it works the same on any host, offline, and without a token.
//
// The rules are heuristics. They are few, they say which one fired, and they
// would rather miss a follow-up than call unrelated work one:
//
// Merged, in this order:
//   - the change's head is on the base: a merge commit or a fast-forward;
//   - a commit on the base has the patch-id of the whole change: a squash;
//   - every commit of the change has its patch-id on the base: a rebase;
//   - a merge commit on the base names the branch ("Merge pull request #12
//     from owner/<branch>", "Merge branch '<branch>'"), for a change that
//     moved on after it was last seen.
//
// Reverted, by a commit after the merge whose message
//   - says "This reverts commit <sha>" for one of the change's commits;
//   - or, saying no such thing, is Revert "<subject>" for one of its subjects;
//   - or says "Reverts #12" (or owner/repo#12, !12) for its request.
//
// Followed up, by a commit after the merge, not a revert, whose message
//   - says it fixes or follows up the request: "fixes #12", "follow-up to
//     #12", "regression from #12", "broken by #12", "hotfix for #12";
//   - or says so of one of its commits: "Fixes: <sha>";
//   - or, within the follow-up window (14 days by default), is a fix by its
//     subject ("fix…", "fix(scope): …", "hotfix…") and removes or rewrites
//     a line the change added. Lines count when they hold 10 letters or
//     digits, so a closing brace touched by both is not a follow-up.
//
// A request number counts only in its forge's form: "#12" where the merge
// commit said "#12" (GitHub), "!12" where it said "!12" (GitLab, whose #12 is
// an issue). A reference qualified with another repository does not count.

func git(dir string, args ...string) (string, error) {
	s, err := gitRaw(context.Background(), dir, nil, args...)
	return strings.TrimSpace(s), err
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	s, err := gitRaw(ctx, dir, env, args...)
	return strings.TrimSpace(s), err
}

// gitRaw keeps the output as it is: a patch's lines start with a space.
func gitRaw(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("the repository is not configured here any more")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// No prompt: a fetch without a credential must fail, not hang a refresh.
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0"}, env...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// resolveBase turns a task's base ref into the ref to read merges from, and
// the remote and branch to fetch it with. A task branched from "main" merges
// into the remote's main, which is what the forge moves.
func resolveBase(dir, baseRef string) (ref, remote, branch string) {
	remotes, _ := git(dir, "remote")
	if i := strings.Index(baseRef, "/"); i > 0 && slices.Contains(strings.Fields(remotes), baseRef[:i]) {
		return baseRef, baseRef[:i], baseRef[i+1:]
	}
	if _, err := git(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+baseRef); err == nil {
		return "origin/" + baseRef, "origin", baseRef
	}
	return baseRef, "", ""
}

// ownCommits lists the commits of mb..head, oldest first, with their
// patch-ids. Merges are left out: they carry no change of their own.
func ownCommits(dir, mb, head string) ([]Commit, error) {
	out, err := git(dir, "log", "--no-show-signature", "--no-merges", "--reverse", "--format=%H%x1f%s", mb+".."+head)
	if err != nil || out == "" {
		return nil, err
	}
	var commits []Commit
	for _, l := range strings.Split(out, "\n") {
		sha, subj, _ := strings.Cut(l, "\x1f")
		commits = append(commits, Commit{SHA: sha, Subject: subj})
	}
	pids := logPatchIDs(dir, mb+".."+head, 0)
	for i := range commits {
		commits[i].PatchID = pids.byCommit[commits[i].SHA]
	}
	return commits, nil
}

// patchIDs maps both ways between commits and git's patch-ids.
type patchIDs struct {
	byCommit map[string]string
	byPatch  map[string]string // the oldest commit with that patch-id
	order    map[string]int    // a commit's place in the log, 0 the newest
}

// logPatchIDs reads the patch-id of every non-merge commit in rng. limit
// caps how far back it reads; 0 is no cap.
func logPatchIDs(dir, rng string, limit int) patchIDs {
	p := patchIDs{byCommit: map[string]string{}, byPatch: map[string]string{}, order: map[string]int{}}
	args := []string{"-c", "core.quotepath=off", "log", "-p", "--no-show-signature", "--no-merges", "--pretty=medium",
		"--no-decorate", "--no-color", "--no-ext-diff", "--no-textconv"}
	if limit > 0 {
		args = append(args, "--max-count="+strconv.Itoa(limit))
	}
	patch, err := gitRaw(context.Background(), dir, nil, append(args, rng)...)
	if err != nil || patch == "" {
		return p
	}
	out := patchID(dir, patch)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range lines {
		pid, sha, ok := strings.Cut(l, " ")
		if !ok {
			continue
		}
		p.byCommit[sha] = pid
		p.byPatch[pid] = sha // the log is newest first, so the oldest wins
		p.order[sha] = i
	}
	return p
}

func patchID(dir, patch string) string {
	cmd := exec.Command("git", "patch-id", "--stable")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(patch)
	out, _ := cmd.Output()
	return string(out)
}

// diffPatchID is the patch-id of the whole change, as a squash lands it.
func diffPatchID(dir, mb, head string) string {
	patch, err := gitRaw(context.Background(), dir, nil, "-c", "core.quotepath=off", "diff",
		"--no-color", "--no-ext-diff", "--no-textconv", mb, head)
	if err != nil || patch == "" {
		return ""
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(patchID(dir, patch)), " ")
	return pid
}

// scanLimit caps the commits one look at a base reads.
const scanLimit = 5000

// scanner reads one base of one repository.
type scanner struct {
	dir, base string
	window    time.Duration
	// origin is the remote's owner/repo, lower case, to tell "owner/repo#12"
	// about this repository from one about another.
	origin string
	pids   map[string]patchIDs // by merge-base
	added  map[string]map[string]map[string]bool
}

func newScanner(dir, base string, window time.Duration) *scanner {
	s := &scanner{dir: dir, base: base, window: window, pids: map[string]patchIDs{},
		added: map[string]map[string]map[string]bool{}}
	remote := "origin"
	if i := strings.Index(base, "/"); i > 0 {
		remote = base[:i]
	}
	if u, err := git(dir, "remote", "get-url", remote); err == nil {
		s.origin = repoSlug(u)
	}
	return s
}

var slugRe = regexp.MustCompile(`[:/]([\w.-]+/[\w.-]+?)(?:\.git)?/?$`)

// repoSlug reads owner/repo from a remote URL, ssh or https.
func repoSlug(u string) string {
	m := slugRe.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

func (s *scanner) exists(rev string) bool {
	if rev == "" {
		return false
	}
	_, err := git(s.dir, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

// info reads one commit.
type commitInfo struct {
	sha     string
	parents []string
	at      time.Time
	subject string
	message string
}

func (s *scanner) info(rev string) (commitInfo, bool) {
	out, err := git(s.dir, "show", "-s", "--no-show-signature", "--format=%H%x1f%P%x1f%ct%x1f%B", rev)
	if err != nil {
		return commitInfo{}, false
	}
	recs := parseLog(out + "\x1e")
	if len(recs) != 1 {
		return commitInfo{}, false
	}
	return recs[0], true
}

// parseLog reads --format=%H%x1f%P%x1f%ct%x1f%B%x1e.
func parseLog(out string) []commitInfo {
	var recs []commitInfo
	for _, r := range strings.Split(out, "\x1e") {
		r = strings.TrimLeft(r, "\n")
		if r == "" {
			continue
		}
		f := strings.SplitN(r, "\x1f", 4)
		if len(f) != 4 {
			continue
		}
		ct, _ := strconv.ParseInt(f[2], 10, 64)
		msg := strings.TrimSpace(f[3])
		subj, _, _ := strings.Cut(msg, "\n")
		recs = append(recs, commitInfo{sha: f[0], parents: strings.Fields(f[1]), at: time.Unix(ct, 0),
			subject: strings.TrimSpace(subj), message: msg})
	}
	return recs
}

var (
	ghMergeRe  = regexp.MustCompile(`^Merge pull request #(\d+) from `)
	ghSquashRe = regexp.MustCompile(`\(#(\d+)\)$`)
	glMergeRe  = regexp.MustCompile(`See merge request \S*!(\d+)`)
)

// prFromMessage reads the request number a forge writes into the commit it
// merges with.
func prFromMessage(subject, message string) string {
	if m := ghMergeRe.FindStringSubmatch(subject); m != nil {
		return "#" + m[1]
	}
	if m := glMergeRe.FindStringSubmatch(message); m != nil {
		return "!" + m[1]
	}
	if m := ghSquashRe.FindStringSubmatch(subject); m != nil {
		return "#" + m[1]
	}
	return ""
}

// prOf reads a request a person names: "#12", "!12", or its URL.
var prURLRe = regexp.MustCompile(`/(pull|pulls|merge_requests)/(\d+)`)

func prOf(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1 && (s[0] == '#' || s[0] == '!') {
		if _, err := strconv.Atoi(s[1:]); err == nil {
			return s
		}
	}
	if m := prURLRe.FindStringSubmatch(s); m != nil {
		if m[1] == "merge_requests" {
			return "!" + m[2]
		}
		return "#" + m[2]
	}
	return ""
}

// mergeOf finds how the change reached the base, or nil if it has not.
func (s *scanner) mergeOf(c *Change) *Merge {
	if !s.exists(s.base) {
		return nil
	}
	landed := make([]string, len(c.Commits))
	for i, k := range c.Commits {
		landed[i] = k.SHA
	}

	if s.exists(c.Head) {
		if _, err := git(s.dir, "merge-base", "--is-ancestor", c.Head, s.base); err == nil {
			commit, how := c.Head, "fast-forward"
			// The first commit of the base's own line that descends from the
			// head is the merge, unless the head is on that line itself.
			if first, _ := git(s.dir, "rev-list", "--first-parent", "--ancestry-path", "--reverse",
				c.Head+".."+s.base); first != "" {
				first, _, _ = strings.Cut(first, "\n")
				if p1, _ := git(s.dir, "rev-parse", first+"^1"); p1 != c.Head {
					commit, how = first, "merge"
				}
			}
			return s.merge(commit, how, landed)
		}
	}

	pids, ok := s.pids[c.MergeBase]
	if !ok {
		pids = logPatchIDs(s.dir, c.MergeBase+".."+s.base, scanLimit)
		s.pids[c.MergeBase] = pids
	}
	if sha := pids.byPatch[c.PatchID]; c.PatchID != "" && sha != "" {
		return s.merge(sha, "squash", []string{sha})
	}
	var rebased []string
	newest := ""
	for _, k := range c.Commits {
		sha := pids.byPatch[k.PatchID]
		if k.PatchID == "" || sha == "" {
			rebased = nil
			break
		}
		rebased = append(rebased, sha)
		if newest == "" || pids.order[sha] < pids.order[newest] {
			newest = sha
		}
	}
	if len(rebased) > 0 {
		return s.merge(newest, "rebase", rebased)
	}

	if c.Branch == "" {
		return nil
	}
	out, err := git(s.dir, "log", "--no-show-signature", "--merges", "--format=%H%x1f%s", "--max-count="+strconv.Itoa(scanLimit),
		c.MergeBase+".."+s.base)
	if err != nil || out == "" {
		return nil
	}
	b := regexp.QuoteMeta(c.Branch)
	named := regexp.MustCompile(`^Merge pull request #\d+ from \S+/` + b + `$|^Merge branch '` + b + `'( into .*)?$`)
	var hit string
	for _, l := range strings.Split(out, "\n") {
		sha, subj, _ := strings.Cut(l, "\x1f")
		if named.MatchString(subj) {
			hit = sha // the log is newest first: keep the oldest
		}
	}
	if hit == "" {
		return nil
	}
	if in, err := git(s.dir, "rev-list", "--no-merges", hit+"^1.."+hit); err == nil && in != "" {
		landed = strings.Split(in, "\n")
	}
	return s.merge(hit, "message", landed)
}

func (s *scanner) merge(commit, how string, landed []string) *Merge {
	ci, ok := s.info(commit)
	if !ok {
		return nil
	}
	return &Merge{Commit: ci.sha, Subject: ci.subject, At: ci.at, How: how,
		PR: prFromMessage(ci.subject, ci.message), Landed: landed}
}

var (
	revertsCommitRe = regexp.MustCompile(`(?i)This reverts commit ([0-9a-f]{7,40})`)
	revertTitleRe   = regexp.MustCompile(`^Revert "(.+)"(?: \([#!]\d+\))?$`)
	revertsPRRe     = regexp.MustCompile(`(?im)^\s*Reverts?:? (?:([\w.-]+/[\w.-]+))?([#!])(\d+)\b`)

	fixWords = `\b(?:fix(?:es|ed|up)?|follow[- ]?up(?: (?:to|for|on|of))?|regression(?: (?:from|in|of|by|since))?|` +
		`broken (?:by|in|since)|hotfix(?: for)?)\b`
	fixRefRe     = regexp.MustCompile(`(?i)` + fixWords + `[\s:]*(?:([\w.-]+/[\w.-]+))?([#!])(\d+)\b`)
	fixShaRe     = regexp.MustCompile(`(?i)` + fixWords + `[\s:]*(?:commit )?([0-9a-f]{7,40})\b`)
	fixSubjectRe = regexp.MustCompile(`(?i)^(?:fix|fixes|fixed|hotfix|bugfix)\b|^(?:fix|hotfix|bugfix)(?:\([^)]*\))?!?:`)
	trailingPRRe = regexp.MustCompile(` \([#!]\d+\)$`)
)

// outcomes reads the base after the merge, within the watch, and sets what
// it says went wrong. It starts over every time, so a look after the watch
// ended finds what every earlier one found.
func (s *scanner) outcomes(c *Change) error {
	m := c.Merged
	if !s.exists(s.base) {
		return fmt.Errorf("%s is not in %s: fetch it, or check the task's base", s.base, c.Repo)
	}
	if !s.exists(m.Commit) {
		return fmt.Errorf("the merge %s is not in %s any more", short(m.Commit), c.Repo)
	}
	out, err := git(s.dir, "log", "--no-show-signature", "--format=%H%x1f%P%x1f%ct%x1f%B%x1e", "--max-count="+strconv.Itoa(scanLimit),
		m.Commit+".."+s.base)
	if err != nil {
		return err
	}
	ours := map[string]bool{c.Head: true, m.Commit: true}
	for _, k := range c.Commits {
		ours[k.SHA] = true
	}
	for _, sha := range m.Landed {
		ours[sha] = true
	}
	isOurs := func(prefix string) bool {
		prefix = strings.ToLower(prefix)
		for sha := range ours {
			if len(prefix) >= 7 && strings.HasPrefix(sha, prefix) {
				return true
			}
		}
		return false
	}
	subjects := map[string]bool{norm(m.Subject): true}
	for _, k := range c.Commits {
		subjects[norm(k.Subject)] = true
	}
	refOurs := func(qual, sigil, n string) bool {
		return m.PR != "" && sigil+n == m.PR && (qual == "" || s.origin == "" || strings.ToLower(qual) == s.origin)
	}

	var reverted, follow []Evidence
	for _, k := range parseLog(out) {
		if ours[k.sha] || k.at.After(m.At.Add(watchFor)) {
			continue
		}
		ev := Evidence{Commit: k.sha, Subject: k.subject, At: k.at}

		// Reverts. A message naming the commit it reverts is believed over
		// its title: Revert "Update docs" may well be about another change
		// that had the same subject.
		named := revertsCommitRe.FindAllStringSubmatch(k.message, -1)
		for _, r := range named {
			if isOurs(r[1]) {
				ev.Rule, ev.Detail = RuleRevertsCommit, "says it reverts "+short(r[1])
				break
			}
		}
		if ev.Rule == "" && len(named) == 0 {
			if t := revertTitleRe.FindStringSubmatch(k.subject); t != nil && subjects[norm(t[1])] {
				ev.Rule, ev.Detail = RuleRevertTitle, "its title reverts "+strconv.Quote(t[1])
			} else {
				for _, r := range revertsPRRe.FindAllStringSubmatch(k.message, -1) {
					if refOurs(r[1], r[2], r[3]) {
						ev.Rule, ev.Detail = RuleRevertsPR, "says it reverts "+m.PR
						break
					}
				}
			}
		}
		if ev.Rule != "" {
			ev.Lines, ev.Paths = s.touched(c, k.sha)
			reverted = append(reverted, ev)
			continue
		}

		// Follow-ups.
		for _, r := range fixRefRe.FindAllStringSubmatch(k.message, -1) {
			if refOurs(r[1], r[2], r[3]) {
				ev.Rule, ev.Detail = RuleMentionsPR, "says it fixes or follows up "+m.PR
				break
			}
		}
		if ev.Rule == "" {
			for _, r := range fixShaRe.FindAllStringSubmatch(k.message, -1) {
				if isOurs(r[1]) {
					ev.Rule, ev.Detail = RuleMentionsCommit, "says it fixes or follows up "+short(r[1])
					break
				}
			}
		}
		if ev.Rule != "" {
			ev.Lines, ev.Paths = s.touched(c, k.sha)
			follow = append(follow, ev)
			continue
		}
		if len(k.parents) == 1 && !k.at.Before(m.At) && k.at.Sub(m.At) <= s.window && fixSubjectRe.MatchString(k.subject) {
			lines, paths := s.touched(c, k.sha)
			if len(lines) > 0 {
				ev.Rule = RuleSameLines
				ev.Detail = fmt.Sprintf("a fix %s after the merge rewrote %d line(s) the change added (heuristic)",
					ago(k.at.Sub(m.At)), len(lines))
				ev.Lines, ev.Paths = lines, paths
				follow = append(follow, ev)
			}
		}
	}
	c.Reverted, c.FollowUps = reverted, follow
	return nil
}

func norm(subject string) string {
	return strings.TrimSpace(trailingPRRe.ReplaceAllString(strings.TrimSpace(subject), ""))
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

func ago(d time.Duration) string {
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// maxLines caps the lines kept per piece of evidence.
const maxLines = 50

// touched is what commit k did to the change: the change's added lines it
// removed or rewrote, and the change's files it touched.
func (s *scanner) touched(c *Change, k string) ([]Line, []string) {
	added := s.addedBy(c)
	if len(added) == 0 {
		return nil, nil
	}
	patch, err := gitRaw(context.Background(), s.dir, nil, "-c", "core.quotepath=off", "show", "-U0",
		"--no-show-signature", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/",
		"--format=", k)
	if err != nil {
		return nil, nil
	}
	var lines []Line
	var paths []string
	seen := map[Line]bool{}
	for path, d := range parsePatch(patch) {
		if added[path] == nil {
			continue
		}
		paths = append(paths, path)
		for _, l := range d.removed {
			ln := Line{path, l}
			if added[path][l] && !seen[ln] && len(lines) < maxLines {
				seen[ln] = true
				lines = append(lines, ln)
			}
		}
	}
	slices.Sort(paths)
	slices.SortStableFunc(lines, func(a, b Line) int { return strings.Compare(a.Path, b.Path) })
	return lines, paths
}

// addedBy is the change's added lines that count, by path.
func (s *scanner) addedBy(c *Change) map[string]map[string]bool {
	if a, ok := s.added[c.ID]; ok {
		return a
	}
	a := map[string]map[string]bool{}
	s.added[c.ID] = a
	if !s.exists(c.MergeBase) || !s.exists(c.Head) {
		return a
	}
	patch, err := gitRaw(context.Background(), s.dir, nil, "-c", "core.quotepath=off", "diff", "-U0",
		"--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", c.MergeBase, c.Head)
	if err != nil {
		return a
	}
	for path, d := range parsePatch(patch) {
		a[path] = map[string]bool{}
		for _, l := range d.added {
			a[path][l] = true
		}
	}
	return a
}

type fileDiff struct{ added, removed []string }

// parsePatch reads a -U0 patch into the lines each file gained and lost,
// trimmed, keeping only the lines that count (see distinctive).
func parsePatch(patch string) map[string]*fileDiff {
	out := map[string]*fileDiff{}
	var cur *fileDiff
	var oldPath string
	// The --- and +++ lines are headers only before a file's first hunk: in
	// a hunk, "--- x" is the removed line "-- x", a SQL comment.
	header := false
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur, oldPath, header = nil, "", true
		case header && strings.HasPrefix(l, "@@"):
			header = false
		case header && strings.HasPrefix(l, "--- "):
			oldPath = pathOf(l[4:], "a/")
		case header && strings.HasPrefix(l, "+++ "):
			p := pathOf(l[4:], "b/")
			if p == "" {
				p = oldPath // a deleted file
			}
			if p == "" {
				continue
			}
			if out[p] == nil {
				out[p] = &fileDiff{}
			}
			cur = out[p]
		case cur == nil || header || l == "":
		case l[0] == '+':
			if t := strings.TrimSpace(l[1:]); distinctive(t) {
				cur.added = append(cur.added, t)
			}
		case l[0] == '-':
			if t := strings.TrimSpace(l[1:]); distinctive(t) {
				cur.removed = append(cur.removed, t)
			}
		}
	}
	return out
}

func pathOf(s, prefix string) string {
	s = strings.TrimRight(s, "\t")
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}

// distinctive keeps lines with something to recognise them by: 10 letters
// or digits. "}" and "return nil" are in every file.
func distinctive(s string) bool {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n >= 10
}
