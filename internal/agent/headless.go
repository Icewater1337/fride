package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Headless is one `claude -p` turn whose reply the CLI validates against a
// JSON schema: how the orchestrator thinks and the review's AI pass asks.
type Headless struct {
	Bin    string // the claude binary
	Dir    string // the working directory, which the tools read
	Model  string
	System string // replaces the default system prompt
	Schema string // the JSON schema of the reply
	Tools  string // the tools it may use, comma-separated
	// AddDirs are further folders the tools may read, beyond Dir.
	AddDirs []string
	// Resume is the session to continue; "" starts a new one.
	Resume string
	// Persist keeps the session to resume later. Without it the turn leaves
	// no conversation behind among the ones offered for resuming.
	Persist bool
}

// ErrUnreadable is a reply that is not the JSON the schema asks for.
var ErrUnreadable = errors.New("unreadable reply")

// Run runs the turn and decodes its validated reply into out. It returns the
// session the CLI reported, which comes back from a turn the CLI ran to the
// end but reported as failed too: that conversation happened. A run that
// failed returns none, and leaves the caller's session alone.
//
// A failure is one line, clipped: the CLI's own is_error result, else the
// context's error when the run was cut off by it, else the CLI's stderr.
// With --output-format json the CLI reports some failures, an expired login
// among them, as a result on stdout, exits 1 and leaves stderr empty, and
// "exit status 1" alone says nothing worth acting on.
func (h Headless) Run(ctx context.Context, input string, out any) (string, error) {
	argv := []string{"-p", "--output-format", "json", "--json-schema", h.Schema,
		"--tools", h.Tools, "--system-prompt", h.System, "--model", h.Model,
		"--permission-mode", "dontAsk"}
	for _, d := range h.AddDirs {
		argv = append(argv, "--add-dir", d)
	}
	if h.Resume != "" {
		argv = append(argv, "--resume", h.Resume)
	}
	if !h.Persist {
		argv = append(argv, "--no-session-persistence")
	}
	cmd := exec.CommandContext(ctx, h.Bin, argv...)
	cmd.Dir = h.Dir
	// The prompt goes on stdin, which -p reads when it is given no prompt
	// argument. As an argument it would be bounded by the OS: Linux caps a
	// single one at 128 KiB, and the orchestrator's state of a handful of
	// supervised tasks, each with its recent turns and its screen, gets there.
	cmd.Stdin = strings.NewReader(input)
	// A clean top-level session, whose transcript is written: see CleanEnv.
	cmd.Env = CleanEnv()
	b, err := cmd.Output()

	var res struct {
		SessionID  string          `json:"session_id"`
		IsError    bool            `json:"is_error"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	perr := json.Unmarshal(b, &res)
	session := ""
	if err == nil && perr == nil {
		session = res.SessionID
	}
	var ee *exec.ExitError
	switch {
	case perr == nil && res.IsError:
		return session, errors.New(firstLine(res.Result))
	case err != nil && ctx.Err() != nil:
		return "", ctx.Err()
	case errors.As(err, &ee) && len(ee.Stderr) > 0:
		return "", errors.New(firstLine(string(ee.Stderr)))
	case err != nil:
		return "", err
	case perr != nil:
		return "", fmt.Errorf("%w: %w", ErrUnreadable, perr)
	}
	raw := res.Structured
	if len(raw) == 0 {
		// The schema is enforced by the CLI, so this is the rare shape where
		// the validated object is only present as the result string.
		raw = json.RawMessage(res.Result)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return session, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return session, nil
}

// firstLine is the first line of what the CLI said, clipped to fit an error
// shown in a chat or a banner.
func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if s = strings.TrimSpace(s); s == "" {
		return "no detail given"
	}
	return clip(s, 400)
}
