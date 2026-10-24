// Package settings owns config.json: everything FrIDE knows about YOUR stack.
//
// FrIDE ships with no knowledge of any particular project. Which folders hold
// your repositories, which of them make up the app, how each one is installed,
// started and pointed at the others — all of it lives in this one file, written
// during setup (normally by a coding agent following SETUP.md, which interviews
// you) and editable by hand afterwards. `fride config check` validates it.
//
// The file is declarative on purpose: the Go side only executes it. That keeps
// the judgement — "this repo reads its port from the PORT key in .env" — in a
// place a human can read, diff and correct.
package settings

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Version is the config.json schema version this build reads.
const Version = 1

// Settings is config.json. Zero value means "not set up yet".
type Settings struct {
	Version int `json:"version"`
	// Name is a human label for the stack, shown in the UI header.
	Name string `json:"name,omitempty"`
	// WorkspaceRoot holds one folder per task: <root>/<task>/<repo>.
	// Supports a leading ~. Default ~/fride-workspaces.
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	// BaseFolders are scanned one level deep for git repositories.
	BaseFolders []string `json:"baseFolders"`
	// CoreRepos make up the app. A new task checks them out by default, and
	// they are what a profile is built around.
	CoreRepos []string `json:"coreRepos,omitempty"`
	// MaxSlots is how many task stacks can run at once. Every per-task port is
	// base+slot, so each service's base needs MaxSlots free ports above it.
	MaxSlots int `json:"maxSlots,omitempty"`
	// Repos is per-repository configuration, keyed by repository folder name.
	Repos map[string]*Repo `json:"repos,omitempty"`
	// DefaultProfile is what a new task boots with.
	DefaultProfile string `json:"defaultProfile,omitempty"`
	// Profiles are the ways the stack can be booted ("local", "docker", …).
	Profiles map[string]*Profile `json:"profiles,omitempty"`
	// Requirements are machine prerequisites, checked by `fride doctor`.
	Requirements []Requirement `json:"requirements,omitempty"`
	// Fleet links this computer with the user's other computers (FLEET.md).
	Fleet *Fleet `json:"fleet,omitempty"`
	// Ledger publishes every task's rationale document to a shared repository.
	Ledger *Ledger `json:"ledger,omitempty"`
	// Ideas is the repository a task's idea pages are published to.
	Ideas *Ideas `json:"ideas,omitempty"`
	// Memory is what one more stack needs, checked before a stack starts.
	Memory *Memory `json:"memory,omitempty"`
	// Inbox configures messages from outside the IDE (INBOX.md).
	Inbox *Inbox `json:"inbox,omitempty"`
	// Companion runs FrIDE under a local companion program (COMPANION.md).
	Companion *Companion `json:"companion,omitempty"`

	mu   sync.Mutex
	path string
	// prefs are remembered UI choices, kept in their own file so that the
	// declarative config is never rewritten just because a dialog was used.
	prefs     prefs
	prefsPath string
}

// Repo is the per-repository configuration.
type Repo struct {
	// Copy lists git-ignored local files (".env", "config/local.yml") copied
	// from the main checkout into every new worktree. nil means "use the repo's
	// .worktreeinclude", which is what `claude --worktree` uses too.
	Copy []string `json:"copy,omitempty"`
	// IDE is the JetBrains launcher this repo opens in ("idea", "pycharm",
	// "webstorm", "goland"). Empty means detect from the build files.
	IDE string `json:"ide,omitempty"`
	// Setup prepares a fresh worktree: dependency installs, code generation.
	// Runs in the background when a task is created, and again before a
	// service from this repo starts if a step's Creates path is missing.
	Setup []Step `json:"setup,omitempty"`
	// Test runs this repo's tests from a fresh checkout, installs included
	// ("npm ci && npm test"). The Review tab's revert check runs it with the
	// change reverted, to see whether the tests notice.
	Test string `json:"test,omitempty"`
}

// Step is one setup command.
type Step struct {
	// Run is a shell command, run in the repo's worktree.
	Run string `json:"run"`
	// Creates is a path (relative to the worktree) the step produces. When it
	// exists the step is skipped, which is what makes setup cheap to re-run.
	Creates string `json:"creates,omitempty"`
	// Fallback runs when Run fails (`npm ci` refusing an out-of-sync lockfile →
	// `npm install`).
	Fallback string `json:"fallback,omitempty"`
}

// Profile is one way to boot the stack.
type Profile struct {
	Description string `json:"description,omitempty"`
	// Entry names the service whose URL is "the app" — what Open app opens.
	Entry string `json:"entry,omitempty"`
	// Compose is the docker compose project that container-backed services
	// come from. Optional: a profile may run everything from source.
	Compose *Compose `json:"compose,omitempty"`
	// Services, keyed by a short name used in templates ({{ports.api}}).
	Services map[string]*Service `json:"services"`
}

// Compose locates the compose project for container-backed services.
type Compose struct {
	// Files are compose files, templated ({{main.backend}}/docker-compose.yml).
	// Taken from the MAIN checkout normally: a task that did not check that
	// repo out has no copy of its own.
	Files []string `json:"files"`
	// Dir is what relative paths in the compose files resolve against for
	// this task's own containers. Default: the first file's directory.
	Dir string `json:"dir,omitempty"`
}

// Service is one component of a profile.
//
// How a service is provided to a task, decided in this order:
//
//  1. source   — its Repo is checked out in the task's workspace and it has a
//     Run command: started from that worktree on port Port+slot.
//  2. external — something else starts it (a dev server spawning a proxy):
//     nothing is started, its port Port+slot is only watched.
//  3. container — it has a Container: the task's own scope override, else
//     Container.Scope (shared by default).
//  4. absent   — none of the above. Templates that reference it drop their
//     key instead of writing it, so the consumer falls back to its default.
type Service struct {
	// Repo is the repository this service runs from.
	Repo string `json:"repo,omitempty"`
	// Dir is the working directory, templated. Default {{repo.<Repo>}}.
	Dir string `json:"dir,omitempty"`
	// Run is the shell command that starts the service in the foreground.
	Run string `json:"run,omitempty"`
	// Pre runs before Run, every time (clean up a stale sidecar, generate a
	// file). Its failure is reported but does not stop the start.
	Pre string `json:"pre,omitempty"`
	// Port is the base port. A task in slot k gets Port+k; slot 0 (Port itself)
	// is left to the main checkout, so a task never collides with it.
	Port int `json:"port,omitempty"`
	// External marks a service started by something else (see above).
	External bool `json:"external,omitempty"`
	// Env is extra environment for Run, templated.
	Env map[string]string `json:"env,omitempty"`
	// Files sets keys in KEY=VALUE files (dotenv, .properties) before start,
	// relative to Dir: {".env": {"PORT": "{{port}}"}}. A value that references
	// an absent service removes the key instead.
	Files map[string]map[string]string `json:"files,omitempty"`
	// URL is how to open it, templated. Default http://localhost:{{port}}.
	URL string `json:"url,omitempty"`
	// Health is an HTTP URL, templated, that answers < 500 once the service is
	// ready. Without it, "listening on its port" counts as ready.
	Health string `json:"health,omitempty"`
	// DependsOn lists services this one waits for (listening) before Run.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Container makes the service available as a compose container when it is
	// not run from source.
	Container *Container `json:"container,omitempty"`
	// IDE describes a JetBrains run configuration, so `stack up --ide <svc>`
	// can hand the service to the IDE instead of starting it headlessly.
	IDE *IDERun `json:"ide,omitempty"`
	// Facts are regexes over the service log whose last match is shown in the
	// status view — a private database's JDBC URL, a generated admin password.
	Facts map[string]string `json:"facts,omitempty"`
	// StartTimeout is how many seconds `stack up --wait` gives this service.
	// Default 300 (a cold build can be slow).
	StartTimeout int `json:"startTimeout,omitempty"`
}

// Container is the compose side of a service.
type Container struct {
	// Service is the compose service name. Default: the profile's key.
	Service string `json:"service,omitempty"`
	// Port is the host port compose publishes for the shared container.
	Port int `json:"port,omitempty"`
	// TargetPort is the port inside the container. Default Port.
	TargetPort int `json:"targetPort,omitempty"`
	// Scope: "shared" (one container serves every task, on Port — the
	// default), "task" (this task's own, on the service's Port+slot), "off".
	Scope string `json:"scope,omitempty"`
	// OneShot marks a container that runs to completion (a bucket init).
	OneShot bool `json:"oneShot,omitempty"`
	// Env overrides the container environment for task-scoped containers,
	// templated — to point it at this task's ports instead of compose names.
	Env map[string]string `json:"env,omitempty"`
}

// IDERun is a JetBrains run configuration.
type IDERun struct {
	// Kind: "gradle", "npm" or "uvicorn".
	Kind   string            `json:"kind"`
	Task   string            `json:"task,omitempty"`   // gradle task
	Args   string            `json:"args,omitempty"`   // gradle script parameters
	Script string            `json:"script,omitempty"` // npm script
	Module string            `json:"module,omitempty"` // uvicorn app, module:attr
	Env    map[string]string `json:"env,omitempty"`
}

// Fleet configures this computer's membership in the user's fleet.
type Fleet struct {
	// Enabled joins the fleet whenever `fride serve` runs.
	Enabled bool `json:"enabled"`
	// Name is this computer's name in the fleet. Default: its hostname.
	Name string `json:"name,omitempty"`
	// Ephemeral removes this computer from the tailnet when it goes offline —
	// for throwaway setups.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// Ledger is a git repository collecting the rationale documents of all tasks,
// so the reasoning behind past work can be found in one place. Each document is
// also kept in its task, under docs/rationale/ — the ledger is a copy.
type Ledger struct {
	// Remote is the ledger repository's git URL. Pushing uses git's own
	// credentials; nothing else is needed.
	Remote string `json:"remote"`
	// Branch is the ledger's main branch. Default "main".
	Branch string `json:"branch,omitempty"`
	// Dir is the folder documents go into, by year: <dir>/<year>/<task>.md.
	// Default "rationale".
	Dir string `json:"dir,omitempty"`
	// Push is "branch" (the default): each document goes to its own branch,
	// rationale/<task>, for a merge or pull request; or "direct": straight
	// onto Branch.
	Push string `json:"push,omitempty"`
	// PushOptions are passed to `git push -o` — how a GitLab server is asked
	// to open a merge request and merge it by itself, with no API token:
	// merge_request.create, merge_request.target={{target}},
	// merge_request.merge_when_pipeline_succeeds, … {{task}}, {{title}},
	// {{branch}} and {{target}} are filled in.
	PushOptions []string `json:"pushOptions,omitempty"`
	// WaitForMerge is how many seconds a publish waits for its branch to be
	// merged before the next one may start. Set it when the server merges by
	// itself (GitLab auto-merge): publishes based on a main without the
	// previous one conflict on the shared index and never merge.
	WaitForMerge int `json:"waitForMerge,omitempty"`
	// Author is "Name <email>" for the ledger's commits. Default: git's own
	// configuration — which, for a ledger outside your usual folders, may not
	// be the identity that server expects.
	Author string `json:"author,omitempty"`
	// OnDone publishes automatically when a task moves to Done. Default true.
	OnDone *bool `json:"onDone,omitempty"`
}

// Ideas is the team's ideas repository. While a task explores an idea rather
// than implementing a ticket, its agent keeps a page about it in the
// workspace (the document-idea skill FrIDE installs), and Publish idea copies
// the page into this repository. The skill also reads it, so an idea that
// exists already is extended instead of duplicated.
type Ideas struct {
	// Remote is the repository's git URL. Pushing uses git's own credentials.
	Remote string `json:"remote"`
	// Branch is its main branch. Default "main".
	Branch string `json:"branch,omitempty"`
	// Dir is the folder holding one page per idea. Default "ideas".
	Dir string `json:"dir,omitempty"`
	// PushOptions are passed to `git push -o`, as for the ledger: how a
	// GitLab server is asked to open a merge request and merge it by itself.
	// {{task}}, {{title}}, {{branch}} and {{target}} are filled in.
	PushOptions []string `json:"pushOptions,omitempty"`
	// Author is "Name <email>" for the commits. Default: git's own
	// configuration.
	Author string `json:"author,omitempty"`
}

// Memory is what one more stack of this project needs, in GB. Before a stack
// starts, FrIDE checks that the computer, and Docker, have that much left: a
// boot into a full machine does not fail cleanly, the OS kills something.
// Pointers, because 0 is meaningful: it turns that check off.
type Memory struct {
	// PerStackGB is the computer's RAM one stack needs. Default 2.
	PerStackGB *float64 `json:"perStackGB,omitempty"`
	// DockerPerStackGB is Docker's memory one stack needs: the containers its
	// services start, Testcontainers included. Default 0 (not checked).
	DockerPerStackGB *float64 `json:"dockerPerStackGB,omitempty"`
}

// Inbox configures the orchestrator's inbox.
type Inbox struct {
	// Notify is a shell command run whenever an inbox thread has news: the
	// orchestrator's answer, what it did, what became of the thread's tasks.
	// It is how whoever sent the message hears back. The thread and its new
	// messages arrive as JSON on stdin, and FRIDE_THREAD, FRIDE_STATUS,
	// FRIDE_SOURCE, FRIDE_REF and FRIDE_TEXT (a summary ready to forward) are
	// set, so a one-line curl needs no JSON tooling.
	Notify string `json:"notify,omitempty"`
}

// Companion configures companion mode: FrIDE run by a program on the same
// computer that takes over what fleet mode does — placement, moves, relaying
// news — and talks to FrIDE's API with a token it was paired with.
type Companion struct {
	// Enabled starts `fride serve` in companion mode, as --under-fridolin does.
	Enabled bool `json:"enabled,omitempty"`
	// WebhookURL receives thread news as signed JSON POSTs. Loopback only:
	// http://127.0.0.1:<port>/…, http://[::1]:<port>/… or
	// http://localhost:<port>/…. Default: the URL the companion gave when it
	// paired, if any.
	WebhookURL string `json:"webhookUrl,omitempty"`
	// ClaudePlugins are Claude Code plugin folders every Claude Code task
	// agent loads, each passed as --plugin-dir: a plugin, or a folder of
	// plugins. A leading ~ is expanded; a relative path is relative to FrIDE's
	// home. Nothing is installed into ~/.claude. Remove an entry to opt out.
	ClaudePlugins []string `json:"claudePlugins,omitempty"`
}

// ClaudePluginDirs is companion.claudePlugins as absolute paths.
func (s *Settings) ClaudePluginDirs() []string {
	if s == nil || s.Companion == nil {
		return nil
	}
	var out []string
	for _, p := range s.Companion.ClaudePlugins {
		p = ExpandHome(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(s.path), p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// CompanionEnabled reports whether config.json turns companion mode on.
func (s *Settings) CompanionEnabled() bool {
	return s != nil && s.Companion != nil && s.Companion.Enabled
}

// CompanionWebhook is the configured webhook URL, "" when none is set.
func (s *Settings) CompanionWebhook() string {
	if s == nil || s.Companion == nil {
		return ""
	}
	return strings.TrimSpace(s.Companion.WebhookURL)
}

// LoopbackURL checks that a webhook URL can only reach this computer: http or
// https, and a host that is a loopback address or "localhost". Thread news
// carries what the user asked for and what the agents did, so it must not be
// sent anywhere else, whatever config.json says.
func LoopbackURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("not a URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http:// or https:// URL")
	}
	if u.User != nil {
		return fmt.Errorf("must not carry a user or password")
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("must be a loopback URL (127.0.0.1, [::1] or localhost), not %q", host)
}

// InboxNotify is the command to run for inbox news, "" when none is set.
func (s *Settings) InboxNotify() string {
	if s == nil || s.Inbox == nil {
		return ""
	}
	return strings.TrimSpace(s.Inbox.Notify)
}

// DefaultPerStackGB is the RAM a stack is assumed to need when config.json
// does not say.
const DefaultPerStackGB = 2

// MemoryGB returns what one stack needs of the computer and of Docker.
func (s *Settings) MemoryGB() (host, docker float64) {
	host = DefaultPerStackGB
	if m := s.Memory; m != nil {
		if m.PerStackGB != nil {
			host = *m.PerStackGB
		}
		if m.DockerPerStackGB != nil {
			docker = *m.DockerPerStackGB
		}
	}
	return host, docker
}

// Requirement is a machine prerequisite. Exactly one of Env and Command.
type Requirement struct {
	Name string `json:"name"`
	// Env must be set (non-empty) in the environment FrIDE runs in.
	Env string `json:"env,omitempty"`
	// Command must exit 0.
	Command string `json:"command,omitempty"`
	// Why says what fails without it.
	Why string `json:"why,omitempty"`
	// Fix says how to get it.
	Fix string `json:"fix,omitempty"`
}

type prefs struct {
	// LastAgent is the agent CLI the last task was created with, so the next
	// New task starts out where the last one left off.
	LastAgent string `json:"lastAgent,omitempty"`
}

// ErrNotConfigured is returned by Load when config.json does not exist yet.
var ErrNotConfigured = errors.New("FrIDE is not set up yet")

// Load reads config.json from stateDir. A missing file is ErrNotConfigured
// together with an empty, saveable Settings.
func Load(stateDir string) (*Settings, error) {
	s := &Settings{
		path:      filepath.Join(stateDir, "config.json"),
		prefsPath: filepath.Join(stateDir, "prefs.json"),
		Repos:     map[string]*Repo{},
		Profiles:  map[string]*Profile{},
	}
	if b, err := os.ReadFile(s.prefsPath); err == nil {
		_ = json.Unmarshal(b, &s.prefs)
	}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.Version = Version
		return s, ErrNotConfigured
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return s, fmt.Errorf("%s: %w", s.path, err)
	}
	if s.Repos == nil {
		s.Repos = map[string]*Repo{}
	}
	if s.Profiles == nil {
		s.Profiles = map[string]*Profile{}
	}
	return s, nil
}

// Path is where config.json lives.
func (s *Settings) Path() string { return s.path }

// Configured reports whether there is anything to run.
func (s *Settings) Configured() bool { return len(s.Profiles) > 0 }

func (s *Settings) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.path, s)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Settings) Agent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefs.LastAgent
}

func (s *Settings) SetAgent(kind string) {
	s.mu.Lock()
	s.prefs.LastAgent = kind
	p := s.prefs
	s.mu.Unlock()
	_ = writeJSON(s.prefsPath, p)
}

// Profile looks a profile up by name; "" means the default profile.
func (s *Settings) Profile(name string) (*Profile, string, bool) {
	if name == "" {
		name = s.DefaultProfileName()
	}
	p, ok := s.Profiles[name]
	return p, name, ok && p != nil
}

// DefaultProfileName is DefaultProfile, or the alphabetically first profile.
func (s *Settings) DefaultProfileName() string {
	if _, ok := s.Profiles[s.DefaultProfile]; ok {
		return s.DefaultProfile
	}
	names := s.ProfileNames()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (s *Settings) ProfileNames() []string {
	out := make([]string, 0, len(s.Profiles))
	for n := range s.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ServiceNames returns a profile's services in a stable order.
func (p *Profile) ServiceNames() []string {
	out := make([]string, 0, len(p.Services))
	for n := range p.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Discovered is one repository found under a base folder.
type Discovered struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Base string `json:"base"`
	Core bool   `json:"core,omitempty"`
}

// ExpandHome turns a leading ~ into the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Discover scans every base folder one level deep for git repositories. One
// level is deliberate: these folders hold checkouts side by side, and recursing
// would wander into worktrees and vendored copies.
func (s *Settings) Discover() []Discovered {
	var out []Discovered
	seen := map[string]bool{}
	core := map[string]bool{}
	for _, c := range s.CoreRepos {
		core[c] = true
	}
	for _, base := range s.BaseFolders {
		base = ExpandHome(base)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(base, e.Name())
			if _, err := os.Stat(filepath.Join(p, ".git")); err != nil {
				continue
			}
			if seen[e.Name()] {
				continue // same name in two base folders: first wins
			}
			seen[e.Name()] = true
			out = append(out, Discovered{Name: e.Name(), Path: p, Base: base, Core: core[e.Name()]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Settings) Repo(name string) *Repo {
	if r, ok := s.Repos[name]; ok && r != nil {
		return r
	}
	return &Repo{}
}

func (s *Settings) SetRepo(name string, fn func(*Repo)) {
	s.mu.Lock()
	r := s.Repos[name]
	if r == nil {
		r = &Repo{}
		s.Repos[name] = r
	}
	fn(r)
	s.mu.Unlock()
}

// ReadWorktreeInclude returns the entries of a repo's .worktreeinclude, which is
// the default copy list.
func ReadWorktreeInclude(repoPath string) []string {
	f, err := os.Open(filepath.Join(repoPath, ".worktreeinclude"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// EffectiveCopy is what will actually be copied into a new worktree: the
// repo's configured list when set, otherwise its own .worktreeinclude.
func (s *Settings) EffectiveCopy(name, repoPath string) (entries []string, overridden bool) {
	if r, ok := s.Repos[name]; ok && r != nil && r.Copy != nil {
		return r.Copy, true
	}
	return ReadWorktreeInclude(repoPath), false
}

func (s *Settings) AddBaseFolder(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.BaseFolders {
		if b == path {
			return
		}
	}
	s.BaseFolders = append(s.BaseFolders, path)
}

func (s *Settings) RemoveBaseFolder(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.BaseFolders[:0]
	for _, b := range s.BaseFolders {
		if b != path {
			out = append(out, b)
		}
	}
	s.BaseFolders = out
}

func (s *Settings) IDE(name string) string { return s.Repo(name).IDE }

func (s *Settings) SetIDE(name, ide string) {
	s.SetRepo(name, func(r *Repo) { r.IDE = ide })
}
