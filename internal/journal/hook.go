package journal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Icewater1337/fride/internal/config"
)

// The PreCompact hook fires just before Claude Code summarises the conversation
// to reclaim context.
//
// Worth being precise about what is at risk there. The transcript FILE keeps
// every line — a compacted session of this size still holds thousands of
// entries on disk — so compaction is not a data-loss point for mining facts
// afterwards. What it does destroy is the agent's working memory: after it, the
// agent may no longer recall the reasoning it has not yet written down.
//
// So the hook does two things: it marks the boundary in the journal, which is
// useful provenance when reading a long task back, and it asks the agent to
// flush anything unrecorded while it still remembers.

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	Trigger        string `json:"trigger"`
}

// PreCompact handles one hook invocation: stdin is the event, stdout is the
// response. Any failure is silent by design — a hook must never be the reason
// a session cannot compact.
func PreCompact(cfg *config.Config, slugFor func(dir string) string) {
	var in hookInput
	if b, err := io.ReadAll(os.Stdin); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &in)
	}
	dir := in.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	slug := slugFor(dir)

	if slug != "" {
		trigger := in.Trigger
		if trigger == "" {
			trigger = "auto"
		}
		_ = Append(cfg, slug, Entry{
			Kind: KindNote,
			Text: fmt.Sprintf("Context was compacted here (%s). Detail before this point is only "+
				"in the transcript, not in the agent's working memory.", trigger),
			Source: "fride/precompact",
		})
	}

	// Ask the agent to flush what it has not written down. Emitted as
	// additionalContext, which is injected when the event supports it and
	// ignored harmlessly when it does not.
	resp := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": "PreCompact",
			"additionalContext": "Your conversation is about to be compacted, so anything you have " +
				"not written down will be lost to you. Before continuing, record any plan changes, " +
				"decisions taken with their reasons, assumptions you have not verified, and issues " +
				"worth knowing about, using `fride note <kind> \"...\" --why \"...\"`. " +
				"Include the questions the user settled in this conversation — what was being " +
				"decided, the option that lost, and the reason they gave — with `--source user`. " +
				"Record nothing if there is genuinely nothing new.",
		},
	}
	_ = json.NewEncoder(os.Stdout).Encode(resp)
}

// InstallHook writes a workspace-scoped settings file wiring the hook up. It is
// scoped to the workspace rather than the user's global settings on purpose:
// creating a task should not modify configuration that outlives it.
func InstallHook(cfg *config.Config, slug, frideBin string) error {
	dir := filepath.Join(cfg.WorkspaceDir(slug), ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"PreCompact": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":          "command",
							"command":       frideBin + " hook precompact",
							"statusMessage": "Recording rationale before compaction",
						},
					},
				},
			},
		},
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644)
}
