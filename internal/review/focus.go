package review

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/Icewater1337/fride/internal/journal"
)

// Focus is one spot the reviewer should look at first, and why.
//
// Every source is deterministic: a path, a line pattern, a count, a command's
// result, or the agent's own decision log. A list that may hallucinate a risk
// teaches the reader to skip it, and then it cannot point at a real one. An
// AI pass can be added later as one more source, verified first.
type Focus struct {
	Severity string `json:"severity"` // high, medium, low
	Kind     string `json:"kind"`     // which rule raised it, for the UI's icon
	Title    string `json:"title"`
	Reason   string `json:"reason"`
	Repo     string `json:"repo,omitempty"`
	Path     string `json:"path,omitempty"`
	Hunk     string `json:"hunk,omitempty"`
}

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

// Sensitive areas, matched against a path's words, so "author.go" is not auth.
var sensitive = []struct {
	severity, why string
	re            *regexp.Regexp
}{
	{"high", "authentication or access control", regexp.MustCompile(`(?i)(^|[/_.-])(auth|authn|authz|login|logout|session|sessions|oauth|oidc|saml|jwt|token|tokens|permission|permissions|acl|rbac|policy|policies|guard|guards)([/_.-]|$)`)},
	{"high", "secrets or cryptography", regexp.MustCompile(`(?i)(^|[/_.-])(secret|secrets|crypto|crypt|encrypt|encryption|password|passwords|credential|credentials|keystore|cert|certs|tls|ssl)([/_.-]|$)`)},
	{"high", "a database migration or schema", regexp.MustCompile(`(?i)((^|/)(migrations?|migrate|schema|db/changelog|liquibase|flyway)(/|$)|\.sql$|(^|/)schema\.[a-z]+$)`)},
	{"medium", "CI or deployment", regexp.MustCompile(`(?i)(^\.github/workflows/|^\.gitlab-ci|(^|/)Jenkinsfile$|(^|/)Dockerfile[^/]*$|(^|/)docker-compose[^/]*\.ya?ml$|(^|/)compose\.ya?ml$|(^|/)(k8s|kubernetes|helm|charts|deploy|deployment|terraform|infra)/|\.tf$)`)},
}

// manifests declare dependencies. Their lockfiles are folded as noise, so the
// manifest is where a new dependency has to be caught.
var manifests = map[string]bool{
	"go.mod": true, "package.json": true, "requirements.txt": true, "pyproject.toml": true,
	"Pipfile": true, "Cargo.toml": true, "pom.xml": true, "build.gradle": true,
	"build.gradle.kts": true, "Gemfile": true, "composer.json": true, "setup.py": true,
	"setup.cfg": true, "mix.exs": true, "pubspec.yaml": true,
}

// Added lines that deserve a second look wherever they appear.
var riskyLines = []struct {
	severity, title string
	re              *regexp.Regexp
}{
	{"high", "A test is skipped or focused", regexp.MustCompile(`(\bt\.Skip(Now|f)?\(|\b(it|describe|test)\.(skip|only)\(|\b(xit|xdescribe|fit|fdescribe)\(|@Disabled\b|@Ignore\b|@pytest\.mark\.skip|@unittest\.skip)`)},
	{"high", "TLS verification is turned off", regexp.MustCompile(`(InsecureSkipVerify:\s*true|verify\s*=\s*False|rejectUnauthorized:\s*false|NODE_TLS_REJECT_UNAUTHORIZED|CURLOPT_SSL_VERIFYPEER,\s*(0|false))`)},
	{"high", "Code is evaluated from a string", regexp.MustCompile(`(^|[^\w.])(eval|exec)\(|new Function\(|dangerouslySetInnerHTML`)},
	{"medium", "A safety check is bypassed", regexp.MustCompile(`(--no-verify\b|--force\b|chmod (-R )?777|0o?777\b|# ?noqa\b|eslint-disable|@ts-ignore|@ts-nocheck|//nolint|# type: ignore)`)},
	{"low", "Left a TODO", regexp.MustCompile(`\b(TODO|FIXME|HACK|XXX)\b`)},
	{"low", "Debug output left in", regexp.MustCompile(`(\bconsole\.log\(|^\s*debugger;?\s*$|\bbreakpoint\(\)|\bpdb\.set_trace\(|\bdbg!\()`)},
}

var codeExt = map[string]bool{
	".go": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
	".py": true, ".rb": true, ".java": true, ".kt": true, ".kts": true, ".scala": true, ".rs": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".php": true,
	".swift": true, ".m": true, ".ex": true, ".exs": true, ".vue": true, ".svelte": true,
	".dart": true, ".lua": true, ".sh": true, ".sql": true,
}

// IsTest reports a file that holds tests, by the naming conventions of the
// common languages and test runners.
func IsTest(p string) bool {
	lp := strings.ToLower(p)
	base := path.Base(lp)
	for _, d := range []string{"test/", "tests/", "__tests__/", "spec/", "specs/", "testdata/", "e2e/"} {
		if strings.HasPrefix(lp, d) || strings.Contains(lp, "/"+d) {
			return true
		}
	}
	return strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py") || strings.HasSuffix(base, "_spec.rb") ||
		strings.HasSuffix(base, "test.java") || strings.HasSuffix(base, "tests.java") ||
		strings.HasSuffix(base, "test.kt") || strings.HasSuffix(base, "tests.cs") ||
		strings.HasSuffix(base, "test.cs")
}

func isCode(p string) bool { return codeExt[strings.ToLower(path.Ext(p))] }

// focus builds the Focus list for the whole task, most severe first.
func focus(repos []RepoChanges, dirs map[string]string, tr agent.Trace, ev Evidence, entries []journal.Entry) []Focus {
	var out []Focus
	add := func(f Focus) { out = append(out, f) }

	// The evidence first: a failing or missing test run makes everything else
	// a guess.
	if last := ev.lastTest(); last != nil && !last.OK {
		add(Focus{Severity: "high", Kind: "evidence", Title: "The last test run failed",
			Reason: fmt.Sprintf("`%s` failed. Nothing proves the change works.", clipLine(last.Command.Command, 80))})
	} else if len(tr.Edits) > 0 && ev.Tested && !ev.TestedAfterEdit {
		add(Focus{Severity: "medium", Kind: "evidence", Title: "Not tested since the last edit",
			Reason: "The agent's last test run came before its last edit to the code."})
	} else if len(tr.Edits) > 0 && !ev.Tested {
		add(Focus{Severity: "medium", Kind: "evidence", Title: "The agent ran no tests",
			Reason: "No test command appears in its sessions in this workspace."})
	}

	for _, rc := range repos {
		edits := editsPerFile(dirs[rc.Repo], tr.Edits)
		srcChanged, testChanged := 0, 0
		for _, f := range rc.Files {
			if f.Noise != "" {
				continue
			}
			if IsTest(f.Path) {
				testChanged++
			} else if isCode(f.Path) {
				srcChanged++
			}
			ref := Focus{Repo: rc.Repo, Path: f.Path}

			for _, s := range sensitive {
				if s.re.MatchString(f.Path) {
					fc := ref
					fc.Severity, fc.Kind, fc.Title = s.severity, "area", f.Path
					fc.Reason = "Touches " + s.why + "."
					if f.Status == "D" {
						fc.Reason = "Deletes " + s.why + "."
					}
					add(fc)
					break
				}
			}
			if manifests[path.Base(f.Path)] {
				fc := ref
				fc.Severity, fc.Kind, fc.Title = "medium", "deps", f.Path
				fc.Reason = "Dependencies or build settings changed."
				add(fc)
			}
			if f.Status == "D" && !IsTest(f.Path) {
				fc := ref
				fc.Severity, fc.Kind, fc.Title = "medium", "delete", f.Path
				fc.Reason = "Deleted. Check that nothing still uses it."
				add(fc)
			}
			if f.Added+f.Deleted >= 300 {
				fc := ref
				fc.Severity, fc.Kind, fc.Title = "low", "size", f.Path
				fc.Reason = fmt.Sprintf("A large change (+%d −%d). Read it line by line.", f.Added, f.Deleted)
				add(fc)
			}
			if n := edits[f.Path]; n >= 6 {
				fc := ref
				fc.Severity, fc.Kind, fc.Title = "low", "churn", f.Path
				fc.Reason = fmt.Sprintf("The agent edited it %d times. It may have struggled here.", n)
				add(fc)
			}
			// One item per rule per file, pointing at the first hunk that
			// matched: a pattern found ten times is still one thing to check.
			for _, rl := range riskyLines {
				if IsTest(f.Path) && rl.title == "Debug output left in" {
					continue
				}
				if hunk, line := firstMatch(f, rl.re); hunk != "" {
					fc := ref
					fc.Severity, fc.Kind, fc.Title, fc.Hunk = rl.severity, "line", rl.title, hunk
					fc.Reason = fmt.Sprintf("%s: `%s`", f.Path, clipLine(line, 90))
					add(fc)
				}
			}
		}
		if srcChanged > 0 && testChanged == 0 {
			add(Focus{Severity: "medium", Kind: "tests", Repo: rc.Repo,
				Title:  "No test changed in " + rc.Repo,
				Reason: fmt.Sprintf("%d source file%s changed, and not one test.", srcChanged, plural(srcChanged))})
		}
	}

	// The agent's own doubts: what it assumed without checking, and what went
	// wrong along the way. These are its "what to watch" notes.
	for _, e := range entries {
		switch e.Kind {
		case journal.KindAssumption:
			add(Focus{Severity: "medium", Kind: "assumption", Repo: e.Repo,
				Title: "Assumed: " + clipLine(e.Text, 100), Reason: orDash(e.Why)})
		case journal.KindIssue:
			add(Focus{Severity: "low", Kind: "issue", Repo: e.Repo,
				Title: "Issue: " + clipLine(e.Text, 100), Reason: orDash(e.Why)})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return severityRank[out[i].Severity] < severityRank[out[j].Severity] })
	return out
}

// firstMatch finds the first added line of a file that matches re, and its hunk.
func firstMatch(f File, re *regexp.Regexp) (hunk, line string) {
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if strings.HasPrefix(l, "+") && re.MatchString(l[1:]) {
				return h.ID, strings.TrimSpace(l[1:])
			}
		}
	}
	return "", ""
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "No reason recorded."
	}
	return s
}
