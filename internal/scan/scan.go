// Package scan takes a first look at repositories for the setup agent: what
// they are built with, how they seem to be started, which compose services and
// local (git-ignored) config files they have, and where their docs are.
//
// It deliberately reports evidence rather than conclusions. Deciding that
// "this repo runs with `./gradlew :app:bootRun` and reads its port from
// server.port in .env.properties" takes reading the README and the build files,
// which is the agent's job; this only makes sure it knows where to look and
// does not miss a compose file three folders down.
package scan

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Repo is what was found in one repository.
type Repo struct {
	Name            string            `json:"name"`
	Path            string            `json:"path"`
	Remote          string            `json:"remote,omitempty"`
	DefaultBase     string            `json:"defaultBase,omitempty"`
	Branch          string            `json:"branch,omitempty"`
	Ecosystems      []string          `json:"ecosystems"`
	Markers         []string          `json:"markers"`
	Docs            []string          `json:"docs,omitempty"`
	NPMScripts      map[string]string `json:"npmScripts,omitempty"`
	PackageManager  string            `json:"packageManager,omitempty"`
	MakeTargets     []string          `json:"makeTargets,omitempty"`
	TaskfileTasks   []string          `json:"taskfileTasks,omitempty"`
	SpringBoot      []string          `json:"springBootModules,omitempty"`
	Compose         []Compose         `json:"compose,omitempty"`
	LocalConfig     []string          `json:"localConfig,omitempty"`
	Templates       []string          `json:"templates,omitempty"`
	WorktreeInclude []string          `json:"worktreeInclude,omitempty"`
	PortHints       []string          `json:"portHints,omitempty"`
}

// Compose is one compose file and its services.
type Compose struct {
	File     string           `json:"file"`
	Services []ComposeService `json:"services"`
	Note     string           `json:"note,omitempty"`
}

type ComposeService struct {
	Name      string   `json:"name"`
	Image     string   `json:"image,omitempty"`
	Build     bool     `json:"build,omitempty"`
	Ports     []string `json:"ports,omitempty"`
	Profiles  []string `json:"profiles,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// Folder scans every git repository directly under dir. If dir is itself a
// repository, it is scanned alone.
func Folder(dir string) ([]Repo, error) {
	if isRepo(dir) {
		return []Repo{One(dir)}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Repo
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() && isRepo(p) {
			out = append(out, One(p))
		}
	}
	return out, nil
}

func isRepo(p string) bool {
	_, err := os.Stat(filepath.Join(p, ".git"))
	return err == nil
}

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// skipDirs are never descended into: dependencies and build output.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "build": true, "dist": true, "target": true,
	".gradle": true, ".venv": true, "venv": true, "__pycache__": true, ".idea": true,
	"vendor": true, ".next": true, "out": true, "coverage": true, ".turbo": true, ".cache": true,
}

// markers map a file name to the ecosystem it signals.
var markers = map[string]string{
	"build.gradle": "gradle", "build.gradle.kts": "gradle", "settings.gradle": "gradle", "settings.gradle.kts": "gradle",
	"pom.xml": "maven", "package.json": "node", "pyproject.toml": "python", "requirements.txt": "python",
	"uv.lock": "python", "poetry.lock": "python", "Pipfile": "python", "go.mod": "go", "Cargo.toml": "rust",
	"Gemfile": "ruby", "composer.json": "php", "mix.exs": "elixir", "deno.json": "deno",
	"Dockerfile": "docker", "Taskfile.yml": "task", "Taskfile.yaml": "task", "Makefile": "make",
	"justfile": "just", "Procfile": "procfile", ".nvmrc": "node", ".tool-versions": "asdf",
	".sdkmanrc": "sdkman", ".java-version": "java", "tilt_config.json": "tilt", "Tiltfile": "tilt",
	"devbox.json": "devbox", "flake.nix": "nix",
}

var composeRe = regexp.MustCompile(`^(docker-)?compose([.-][A-Za-z0-9_.-]+)?\.ya?ml$`)

// One scans a single repository.
func One(path string) Repo {
	r := Repo{Name: filepath.Base(path), Path: path, Ecosystems: []string{}, Markers: []string{}}
	// Remote URLs can carry credentials (https://user:token@host/…), and this
	// output is read by an agent and shown in its transcript — so they are
	// always stripped.
	r.Remote = RedactURL(git(path, "remote", "get-url", "origin"))
	r.Branch = git(path, "rev-parse", "--abbrev-ref", "HEAD")
	if ref := git(path, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); ref != "" {
		r.DefaultBase = ref
	}
	r.WorktreeInclude = readLines(filepath.Join(path, ".worktreeinclude"))

	eco := map[string]bool{}
	var composeFiles []string
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(path, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if p != path && (skipDirs[d.Name()] || depth >= 4) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if e, ok := markers[name]; ok && depth <= 2 {
			eco[e] = true
			r.Markers = append(r.Markers, rel)
		}
		if composeRe.MatchString(name) && depth <= 3 {
			composeFiles = append(composeFiles, rel)
		}
		if depth == 0 {
			low := strings.ToLower(name)
			if strings.HasPrefix(low, "readme") || name == "CLAUDE.md" || name == "AGENTS.md" ||
				name == "CONTRIBUTING.md" || name == "GUIDELINES.md" || name == "DEVELOPMENT.md" {
				r.Docs = append(r.Docs, rel)
			}
		}
		if depth <= 2 && isTemplate(name) {
			r.Templates = append(r.Templates, rel)
		}
		if (name == "build.gradle.kts" || name == "build.gradle") && depth <= 3 {
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "org.springframework.boot") {
				mod := filepath.Dir(rel)
				task := ":bootRun"
				if mod != "." {
					task = ":" + strings.ReplaceAll(mod, string(filepath.Separator), ":") + ":bootRun"
				}
				r.SpringBoot = append(r.SpringBoot, task)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(path, "docs")); err == nil {
		r.Docs = append(r.Docs, "docs/")
	}
	for e := range eco {
		r.Ecosystems = append(r.Ecosystems, e)
	}
	sort.Strings(r.Ecosystems)
	sort.Strings(r.SpringBoot)

	if b, err := os.ReadFile(filepath.Join(path, "package.json")); err == nil {
		var pkg struct {
			Scripts        map[string]string `json:"scripts"`
			PackageManager string            `json:"packageManager"`
		}
		if json.Unmarshal(b, &pkg) == nil {
			r.NPMScripts = pkg.Scripts
			r.PackageManager = pkg.PackageManager
		}
		if r.PackageManager == "" {
			switch {
			case exists(path, "pnpm-lock.yaml"):
				r.PackageManager = "pnpm"
			case exists(path, "yarn.lock"):
				r.PackageManager = "yarn"
			case exists(path, "bun.lockb"), exists(path, "bun.lock"):
				r.PackageManager = "bun"
			case exists(path, "package-lock.json"):
				r.PackageManager = "npm (package-lock.json)"
			}
		}
	}
	r.MakeTargets = makeTargets(filepath.Join(path, "Makefile"))
	for _, tf := range []string{"Taskfile.yml", "Taskfile.yaml"} {
		if t := taskfileTasks(filepath.Join(path, tf)); len(t) > 0 {
			r.TaskfileTasks = t
		}
	}
	sort.Strings(composeFiles)
	for _, cf := range composeFiles {
		r.Compose = append(r.Compose, composeServices(path, cf))
	}
	r.LocalConfig = localConfig(path)
	r.PortHints = portHints(path)
	return r
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func isTemplate(name string) bool {
	low := strings.ToLower(name)
	for _, suf := range []string{".example", ".template", ".sample", ".dist", ".tmpl"} {
		if strings.HasSuffix(low, suf) {
			return true
		}
	}
	return strings.Contains(low, ".env.") && (strings.Contains(low, "example") || strings.Contains(low, "template"))
}

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

var makeRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*):([^=]|$)`)

func makeTargets(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := makeRe.FindStringSubmatch(sc.Text()); m != nil && m[1] != ".PHONY" {
			out = append(out, m[1])
		}
	}
	return out
}

var taskRe = regexp.MustCompile(`^  ([A-Za-z0-9][A-Za-z0-9_:.-]*):\s*$`)

func taskfileTasks(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "tasks:") {
			in = true
			continue
		}
		if in && len(l) > 0 && l[0] != ' ' && l[0] != '#' {
			in = false
		}
		if in {
			if m := taskRe.FindStringSubmatch(l); m != nil {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// composeServices asks compose itself to parse the file when it can — it
// resolves extends, anchors and overrides correctly — and falls back to a
// line-based read of the top-level services otherwise.
func composeServices(repo, rel string) Compose {
	c := Compose{File: rel}
	cmd := exec.Command("docker", "compose", "-f", rel, "config", "--format", "json", "--no-interpolate")
	cmd.Dir = repo
	if out, err := cmd.Output(); err == nil {
		var doc struct {
			Services map[string]struct {
				Image    string   `json:"image"`
				Build    any      `json:"build"`
				Profiles []string `json:"profiles"`
				Ports    []struct {
					Published any `json:"published"`
					Target    any `json:"target"`
				} `json:"ports"`
				DependsOn map[string]any `json:"depends_on"`
			} `json:"services"`
		}
		if json.Unmarshal(out, &doc) == nil {
			for name, s := range doc.Services {
				cs := ComposeService{Name: name, Image: s.Image, Build: s.Build != nil, Profiles: s.Profiles}
				for _, p := range s.Ports {
					cs.Ports = append(cs.Ports, toStr(p.Published)+":"+toStr(p.Target))
				}
				for d := range s.DependsOn {
					cs.DependsOn = append(cs.DependsOn, d)
				}
				sort.Strings(cs.DependsOn)
				c.Services = append(c.Services, cs)
			}
			sort.Slice(c.Services, func(i, j int) bool { return c.Services[i].Name < c.Services[j].Name })
			return c
		}
	}
	c.Note = "parsed without docker compose (ports may be incomplete)"
	f, err := os.Open(filepath.Join(repo, rel))
	if err != nil {
		return c
	}
	defer f.Close()
	in := false
	var cur *ComposeService
	svcRe := regexp.MustCompile(`^  ([A-Za-z0-9][A-Za-z0-9_.-]*):\s*$`)
	portRe := regexp.MustCompile(`^\s+-\s*"?(\d+(:\d+)?)"?`)
	inPorts := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "services:") {
			in = true
			continue
		}
		if in && len(l) > 0 && l[0] != ' ' && l[0] != '#' {
			in = false
		}
		if !in {
			continue
		}
		if m := svcRe.FindStringSubmatch(l); m != nil {
			c.Services = append(c.Services, ComposeService{Name: m[1]})
			cur = &c.Services[len(c.Services)-1]
			inPorts = false
			continue
		}
		t := strings.TrimSpace(l)
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(t, "image:"):
			cur.Image = strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "image:")), `"'`)
		case strings.HasPrefix(t, "build:"):
			cur.Build = true
		case t == "ports:":
			inPorts = true
		case inPorts && strings.HasPrefix(t, "-"):
			if m := portRe.FindStringSubmatch(l); m != nil {
				cur.Ports = append(cur.Ports, m[1])
			}
		case strings.HasSuffix(t, ":"):
			inPorts = false
		}
	}
	return c
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// localConfig lists git-ignored files in the main checkout that look like local
// configuration — the files a fresh worktree will NOT have, and which a task
// therefore needs copied in (repos.<name>.copy).
func localConfig(repo string) []string {
	out := git(repo, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "--no-empty-directory")
	var res []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasSuffix(l, "/") {
			continue
		}
		if strings.Count(l, "/") > 3 {
			continue
		}
		base := strings.ToLower(filepath.Base(l))
		looksLikeConfig := strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".properties") ||
			strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".json") ||
			strings.HasSuffix(base, ".toml") || strings.HasSuffix(base, ".ini") || strings.HasSuffix(base, ".conf") ||
			strings.HasSuffix(base, ".cfg") || base == ".npmrc" || strings.Contains(base, "local")
		if !looksLikeConfig || strings.HasSuffix(base, ".log") || strings.Contains(base, ".bak") {
			continue
		}
		if fi, err := os.Stat(filepath.Join(repo, l)); err == nil && fi.Size() < 256<<10 {
			res = append(res, l)
		}
	}
	sort.Strings(res)
	return res
}

// portLineRe matches a port being set: `server.port=8080`, `port: 3000`,
// `PORT=8080`, `--port 5173`, `port: ${SERVER_PORT:8080}`.
var portLineRe = regexp.MustCompile(`(?i)(server\.port|\bport\b|_PORT\b|--port)\s*[:=]?\s*["'$]?[{(]?\s*\w*[:-]?\d{2,5}|(server\.port|\bport\b|_PORT\b)\s*[:=]\s*\$\{`)

// secretLineRe keeps lines that might hold a credential out of the hints.
var secretLineRe = regexp.MustCompile(`(?i)secret|password|passwd|token|apikey|api_key|credential|private`)

// portHints greps the usual places a port is configured, so the agent knows
// which files decide a service's port before it reads them in full.
func portHints(repo string) []string {
	var files []string
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repo, p)
		depth := strings.Count(rel, string(filepath.Separator))
		if d.IsDir() {
			if p != repo && (skipDirs[d.Name()] || depth >= 6) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		low := strings.ToLower(name)
		switch {
		case strings.HasPrefix(low, "application") && (strings.HasSuffix(low, ".yml") || strings.HasSuffix(low, ".yaml") || strings.HasSuffix(low, ".properties")):
		case strings.HasPrefix(low, "vite.config"), strings.HasPrefix(low, "next.config"), strings.HasPrefix(low, "webpack.config"),
			strings.HasPrefix(low, "angular.json"), strings.HasPrefix(low, ".env"), low == "package.json":
		default:
			return nil
		}
		if strings.Contains(rel, "test") && !strings.HasPrefix(low, ".env") {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	var out []string
	for _, rel := range files {
		f, err := os.Open(filepath.Join(repo, rel))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		n := 0
		for sc.Scan() {
			n++
			l := sc.Text()
			if m := portLineRe.FindStringSubmatch(l); m != nil {
				line := strings.TrimSpace(l)
				if len(line) > 100 {
					line = line[:100] + "…"
				}
				// Never echo a value that looks like a secret; ports never do.
				if secretLineRe.MatchString(line) || strings.Contains(line, "@") {
					continue
				}
				out = append(out, rel+":"+itoa(n)+": "+line)
				if len(out) >= 40 {
					f.Close()
					return out
				}
			}
		}
		f.Close()
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

// RedactURL removes credentials from a URL: https://user:token@host/x becomes
// https://host/x. Anything that does not parse as a URL with a host (an scp-
// style git@host:path remote, a local path) is returned unchanged, since it
// carries no password.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}
