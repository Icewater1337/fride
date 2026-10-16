// Package usage reports how many tokens a task workspace has consumed, by
// reading Claude Code's own session transcripts.
//
// Claude Code writes one JSONL file per session under
// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl, where the directory name
// is the session's working directory with "/", "." and "_" replaced by "-".
// Each `assistant` line carries message.usage with the token counts for that
// API response.
//
// Two things this deliberately does NOT do:
//   - guess at a dollar cost, which would mean hardcoding per-model prices that
//     drift; the raw token counts are reported instead.
//   - trust the encoded directory name alone. Encoding is lossy (a workspace
//     "ide-live" would prefix-match "ide-live-test"), so every candidate file is
//     confirmed by reading the `cwd` recorded inside it.
package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Usage struct {
	InputTokens         int64     `json:"inputTokens"`
	OutputTokens        int64     `json:"outputTokens"`
	CacheReadTokens     int64     `json:"cacheReadTokens"`
	CacheCreationTokens int64     `json:"cacheCreationTokens"`
	Total               int64     `json:"total"`
	Messages            int       `json:"messages"`
	Sessions            int       `json:"sessions"`
	Models              []string  `json:"models,omitempty"`
	LastActivity        time.Time `json:"lastActivity,omitempty"`
	// PerSession is newest-first, so the UI can show the current session first.
	PerSession []Session `json:"perSession,omitempty"`
}

type Session struct {
	ID           string    `json:"id"`
	Dir          string    `json:"dir"`
	Total        int64     `json:"total"`
	Messages     int       `json:"messages"`
	LastActivity time.Time `json:"lastActivity,omitempty"`
}

// encodeDir mirrors Claude Code's project-directory naming.
var encodeDir = strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace

type entry struct {
	Type      string    `json:"type"`
	UUID      string    `json:"uuid"`
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// under reports whether p is root or nested inside it.
func under(p, root string) bool {
	p = filepath.Clean(p)
	root = filepath.Clean(root)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// ForWorkspace totals the tokens spent by every Claude session whose working
// directory was the workspace or anything inside it (so agents started in a
// single repo's subdirectory are counted too).
func ForWorkspace(claudeHome, workspaceDir string) (*Usage, error) {
	projects := filepath.Join(claudeHome, "projects")
	des, err := os.ReadDir(projects)
	if err != nil {
		if os.IsNotExist(err) {
			return &Usage{}, nil
		}
		return nil, err
	}
	prefix := encodeDir(filepath.Clean(workspaceDir))

	u := &Usage{}
	seen := map[string]bool{}   // dedupe by entry uuid: resumed/forked
	models := map[string]bool{} // transcripts repeat earlier lines
	sessions := map[string]*Session{}

	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		name := de.Name()
		if name != prefix && !strings.HasPrefix(name, prefix+"-") {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(projects, name, "*.jsonl"))
		for _, f := range files {
			accumulate(f, workspaceDir, u, seen, models, sessions)
		}
	}

	for m := range models {
		u.Models = append(u.Models, m)
	}
	sort.Strings(u.Models)
	for _, s := range sessions {
		u.PerSession = append(u.PerSession, *s)
	}
	sort.Slice(u.PerSession, func(i, j int) bool {
		return u.PerSession[i].LastActivity.After(u.PerSession[j].LastActivity)
	})
	u.Sessions = len(sessions)
	u.Total = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
	return u, nil
}

func accumulate(path, workspaceDir string, u *Usage, seen map[string]bool,
	models map[string]bool, sessions map[string]*Session) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Transcript lines carry whole tool results and can be far larger than the
	// 64 KiB default; without this the scanner stops at the first long line and
	// silently under-reports.
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)

	confirmed := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		// Confirm the file really belongs to this workspace, using the recorded
		// cwd rather than the lossy directory name.
		if !confirmed {
			if e.CWD == "" {
				continue
			}
			if !under(e.CWD, workspaceDir) {
				return // encoded-name collision: this file is a different workspace
			}
			confirmed = true
		}
		if e.Type != "assistant" || e.UUID == "" || seen[e.UUID] {
			continue
		}
		seen[e.UUID] = true

		us := e.Message.Usage
		if us.InputTokens == 0 && us.OutputTokens == 0 &&
			us.CacheReadInputTokens == 0 && us.CacheCreationInputTokens == 0 {
			continue
		}
		u.InputTokens += us.InputTokens
		u.OutputTokens += us.OutputTokens
		u.CacheReadTokens += us.CacheReadInputTokens
		u.CacheCreationTokens += us.CacheCreationInputTokens
		u.Messages++
		if e.Message.Model != "" {
			models[e.Message.Model] = true
		}
		if e.Timestamp.After(u.LastActivity) {
			u.LastActivity = e.Timestamp
		}
		id := e.SessionID
		if id == "" {
			id = filepath.Base(path)
		}
		s := sessions[id]
		if s == nil {
			s = &Session{ID: id, Dir: e.CWD}
			sessions[id] = s
		}
		s.Total += us.InputTokens + us.OutputTokens + us.CacheReadInputTokens + us.CacheCreationInputTokens
		s.Messages++
		if e.Timestamp.After(s.LastActivity) {
			s.LastActivity = e.Timestamp
		}
	}
}
