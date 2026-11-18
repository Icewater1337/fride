package review

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/Icewater1337/fride/internal/files"
	"github.com/Icewater1337/fride/internal/settings"
)

// A change's review tier is set by the paths it touches, never by who wrote
// it: docs need no human, code needs one, the areas where a mistake is
// expensive (auth, payments, migrations, CI/CD, infrastructure) need two. The
// highest tier of any file is the repo's, and the highest repo's is the
// task's. The 0–100 risk score is separate: it weighs the evidence, the tier
// only the paths, so it can be stated as rules a team agrees on.

// RulesFile is where a repository keeps its own review settings, in the
// shape of repos.<name>.review. It is read at the merge-base, not from the
// working tree: an agent's change must not lower its own tier.
const RulesFile = ".fride/review.json"

// Need is how many human approvals a tier asks for. An unknown tier asks for
// one: a tier nobody can name is not a reason to skip review.
func Need(tier string) int {
	switch tier {
	case settings.TierAuto:
		return 0
	case settings.TierRisky:
		return 2
	}
	return 1
}

func tierRank(tier string) int {
	switch tier {
	case settings.TierAuto:
		return 0
	case settings.TierRisky:
		return 2
	}
	return 1
}

// FileTier is why one changed file sits in its tier.
type FileTier struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
	Tier string `json:"tier"`
	Why  string `json:"why"`
	// Rule says whose rule placed it: "config.json", RulesFile, or "default".
	Rule string `json:"rule"`
}

// RepoTier is one repository's tier: the highest of its files.
type RepoTier struct {
	Repo string `json:"repo"`
	Tier string `json:"tier"`
	// Err says why the repo's change or its own rules could not be read; the
	// tier then errs on the side of more review.
	Err string `json:"err,omitempty"`
}

// rule places the paths it matches in a tier. The first rule that matches
// a path decides, so the order of a rule list is its precedence.
type rule struct {
	tier, why, source string
	match             func(path string) bool
}

func reRule(tier, why string, re *regexp.Regexp) rule {
	return rule{tier: tier, why: why, source: "default", match: re.MatchString}
}

// rulesFileRule comes before every rule a person wrote: a change to the
// rules is a change to how much review everything else gets.
var rulesFileRule = rule{tier: settings.TierRisky, why: "changes the repo's review tier rules", source: "default",
	match: func(p string) bool { return p == RulesFile }}

// credentialsRule: a file that holds credentials (the review withholds its
// lines) has no business in a change, so one that is in it gets two pairs of
// eyes. After the configured rules, so a repo that commits a placeholder
// under such a name can say so.
var credentialsRule = rule{tier: settings.TierRisky, why: "a file that holds credentials", source: "default",
	match: files.Secretish}

// defaultRules are FrIDE's generic guesses, after the repo's own rules.
// Nothing is auto by default but documentation: whether code runs behind a
// flag cannot be seen from a path, so flag-guarded code is auto only where a
// repo's rules say so.
//
// Docs come before the risky areas, so a README in an auth folder is still a
// README; agent instructions come before docs, since a Markdown file an agent
// reads as orders is not documentation.
var defaultRules = []rule{
	reRule(settings.TierNormal, "instructions for coding agents, which steer what later agents do",
		regexp.MustCompile(`(?i)((^|/)(CLAUDE|AGENTS|GEMINI|SKILL)\.md$|(^|/)\.(claude|cursor|codex)/|^\.github/copilot-instructions\.md$)`)),
	reRule(settings.TierAuto, "documentation",
		regexp.MustCompile(`(?i)(\.(md|mdx|markdown|rst|adoc|asciidoc)$|(^|/)docs?/([^/]+/)*[^/]+\.(txt|png|jpe?g|gif|svg|webp)$)`)),
	reRule(settings.TierRisky, "authentication or access control",
		regexp.MustCompile(`(?i)(^|[/_.-])(auth|authn|authz|authentication|authorization|oauth2?|oidc|saml|sso|login|logout|signin|signup|permissions?|rbac|acls?)([/_.-]|$)`)),
	reRule(settings.TierRisky, "secrets or cryptography",
		regexp.MustCompile(`(?i)(^|[/_.-])(secrets?|credentials?|crypto|cryptography|encrypt|encryption|decrypt|keystore|keychain|passwords?|certs?|certificates?|tls|ssl|tokens?)([/_.-]|$)`)),
	reRule(settings.TierRisky, "payments or billing",
		regexp.MustCompile(`(?i)(^|[/_.-])(pay|payments?|payouts?|billing|invoices?|checkout|refunds?)([/_.-]|$)`)),
	// The Focus list's own pattern, so the two agree on what a migration is.
	reRule(settings.TierRisky, "a database migration or schema", sensitive[2].re),
	reRule(settings.TierRisky, "CI/CD",
		regexp.MustCompile(`(?i)(^\.github/(workflows|actions)/|^\.gitlab-ci|^\.gitlab/|(^|/)Jenkinsfile$|^\.circleci/|^\.buildkite/|(^|/)azure-pipelines[^/]*\.ya?ml$|^\.drone\.ya?ml$|^bitbucket-pipelines\.ya?ml$|^\.travis\.ya?ml$|(^|/)CODEOWNERS$)`)),
	reRule(settings.TierRisky, "infrastructure or deployment config",
		regexp.MustCompile(`(?i)((^|/)Dockerfile[^/]*$|(^|/)(docker-)?compose[^/]*\.ya?ml$|(^|/)(k8s|kubernetes|helm|charts|deploy|deployment|deployments|terraform|infra|infrastructure|ansible|pulumi)/|\.(tf|tfvars|hcl)$)`)),
}

// fallback places what no rule matched: code, config, anything else.
var fallback = rule{tier: settings.TierNormal, why: "code or config that no rule makes auto or risky", source: "default"}

// configRules compiles the rules of a Review. A rule whose tier is unknown
// counts as risky: `config check` reports it, and until it is fixed the
// typo must not be what lets a change through.
func configRules(rv *settings.Review, source string) []rule {
	if rv == nil {
		return nil
	}
	var out []rule
	for _, t := range rv.Tiers {
		re := globsRE(t.Paths)
		if re == nil {
			continue
		}
		r := rule{tier: t.Tier, why: strings.TrimSpace(t.Why), source: source, match: re.MatchString}
		if tierRank(t.Tier) == 1 && t.Tier != settings.TierNormal {
			r.tier, r.why = settings.TierRisky, fmt.Sprintf("an unknown tier %q in the rule", t.Tier)
		}
		if r.why == "" {
			r.why = "matches " + strings.Join(t.Paths, ", ")
		}
		out = append(out, r)
	}
	return out
}

// repoRules is every rule for one repository, in order: the rules file
// itself, config.json, the repo's own file at the merge-base, credentials,
// then the defaults. err is a repo file that could not be used.
func repoRules(spec RepoSpec, mergeBase string) ([]rule, error) {
	rules := []rule{rulesFileRule}
	rules = append(rules, configRules(spec.Review, "config.json")...)
	own, err := readRepoRules(spec.Dir, mergeBase)
	rules = append(rules, configRules(own, RulesFile)...)
	rules = append(rules, credentialsRule)
	return append(rules, defaultRules...), err
}

// readRepoRules reads RulesFile as it is at the merge-base. No file is no
// rules. A file that does not parse, or names an unknown field or tier, is
// an error rather than half a rule set: a typo must not quietly drop a rule
// that raised a tier.
func readRepoRules(dir, mergeBase string) (*settings.Review, error) {
	if mergeBase == "" {
		return nil, nil
	}
	blob, err := git(dir, "rev-parse", "--verify", "--quiet", mergeBase+":"+RulesFile)
	if err != nil || blob == "" {
		return nil, nil
	}
	b, err := gitRaw(dir, "cat-file", "blob", blob)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", RulesFile, err)
	}
	var rv settings.Review
	dec := json.NewDecoder(strings.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rv); err != nil {
		return nil, fmt.Errorf("%s does not parse: %v", RulesFile, err)
	}
	for _, p := range rv.Problems(RulesFile) {
		if p.Error {
			return nil, fmt.Errorf("%s: %s", p.Where, p.Msg)
		}
	}
	return &rv, nil
}

// place finds the rule for one path.
func place(rules []rule, p string) rule {
	for _, r := range rules {
		if r.match(p) {
			return r
		}
	}
	return fallback
}

// tierFiles places every file of a repository's change in a tier, and the
// repository in the highest of them. A rename counts the higher of its two
// paths: moving a file out of an auth folder changes the auth folder.
func tierFiles(rc *RepoChanges, rules []rule, rulesErr error) RepoTier {
	rt := RepoTier{Repo: rc.Repo, Tier: settings.TierAuto}
	raise := func(tier string) {
		if tierRank(tier) > tierRank(rt.Tier) {
			rt.Tier = tier
		}
	}
	for i := range rc.Files {
		f := &rc.Files[i]
		r := place(rules, f.Path)
		if f.OldPath != "" {
			if o := place(rules, f.OldPath); tierRank(o.tier) > tierRank(r.tier) {
				r = o
			}
		}
		f.Tier, f.TierWhy, f.TierRule = r.tier, r.why, r.source
		raise(r.tier)
	}
	switch {
	case rulesErr != nil:
		// The repo's rules may have raised a tier the defaults would not:
		// until they can be read, the repo gets the most review.
		rt.Tier, rt.Err = settings.TierRisky, "its own review rules could not be read: "+rulesErr.Error()
	case rc.Err != "":
		raise(settings.TierNormal)
		rt.Err = "its change could not be read: " + rc.Err
	}
	return rt
}

// placeRepo reads a repository's rules and places its change.
func placeRepo(spec RepoSpec, rc *RepoChanges) RepoTier {
	rules, err := repoRules(spec, rc.MergeBase)
	return tierFiles(rc, rules, err)
}

// globsRE compiles gitignore-style patterns into one expression, nil for
// none. A pattern without a slash matches a name at any depth ("*.md"), one
// with a slash matches from the repo root ("docs/**", "/Makefile"), and every
// pattern also matches what is below it, so "flags" and "flags/" both cover
// flags/a/b.go. Case matters, as it does to git.
func globsRE(pats []string) *regexp.Regexp {
	var alts []string
	for _, p := range pats {
		if s := globBody(p); s != "" {
			alts = append(alts, s)
		}
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`^(?:` + strings.Join(alts, "|") + `)(/.*)?$`)
}

func globBody(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	rooted := strings.HasPrefix(p, "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	var b strings.Builder
	if !rooted && !strings.Contains(p, "/") {
		b.WriteString(`(?:.*/)?`)
	}
	b.WriteString(globPart(p))
	return b.String()
}

// globPart translates *, **, ? and one level of {a,b}. Everything else is
// literal; a brace that does not close is a brace.
func globPart(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString(`(?:.*/)?`)
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(`.*`)
			i++
		case c == '*':
			b.WriteString(`[^/]*`)
		case c == '?':
			b.WriteString(`[^/]`)
		case c == '{' && strings.IndexByte(p[i:], '}') > 0:
			end := i + strings.IndexByte(p[i:], '}')
			var alts []string
			for _, a := range strings.Split(p[i+1:end], ",") {
				alts = append(alts, globPart(a))
			}
			b.WriteString(`(?:` + strings.Join(alts, "|") + `)`)
			i = end
		default:
			// One byte at a time: a multi-byte character's bytes are copied in
			// order, and QuoteMeta only ever escapes ASCII.
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		}
	}
	return b.String()
}
