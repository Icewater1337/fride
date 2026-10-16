package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Open is one conversation that an agent CLI reports as being open right now,
// in a process that is still running.
type Open struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	// StartedAt is when the process started, in milliseconds since the epoch.
	// It bounds what that process can be responsible for: a conversation whose
	// transcript begins after this cannot have been left behind by anything
	// older, so one of the processes still running must have started it.
	StartedAt int64 `json:"startedAt"`
}

// Started is StartedAt as a time, or the zero time if the CLI did not record one.
func (o Open) Started() time.Time {
	if o.StartedAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(o.StartedAt)
}

// OpenByPID reports which conversation each running agent process is writing,
// as the CLI itself records it — not as fride launched it.
//
// The two differ, and that is the whole reason this exists. A session id is not
// fixed for the life of a process: /clear does not empty a conversation, it
// starts a NEW one under a new id and abandons the old, and the in-TUI /resume
// picker switches to a different one again. After either, the id fride put
// on the command line no longer names what that tab is doing — the conversation
// on screen looks unclaimed, gets offered on the board, and resuming it opens a
// second process on a transcript that is already being written.
//
// Only Claude Code publishes such a registry; for the other agents this is
// empty and the caller falls back to what it knows.
func OpenByPID(k Kind, home string) map[int]Open {
	if Normalize(string(k)) != Claude {
		return nil
	}
	return claudeOpen(home)
}

// claudeOpen reads ~/.claude/sessions/<pid>.json, one file per running CLI.
//
// The files outlive the processes that wrote them — this machine has them going
// back weeks — so every one is checked against a live pid. A recycled pid that
// lands on a stale file is possible in principle; the cost is one conversation
// wrongly reported busy, which is the safe direction to be wrong in.
func claudeOpen(home string) map[int]Open {
	des, err := os.ReadDir(filepath.Join(home, ".claude", "sessions"))
	if err != nil {
		return nil
	}
	out := map[int]Open{}
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil || !alive(pid) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", name))
		if err != nil {
			continue
		}
		var rec Open
		// The file names itself, so a mismatch means it is not describing the
		// process whose id it is filed under and nothing here can be trusted.
		if json.Unmarshal(raw, &rec) != nil || rec.PID != pid || rec.SessionID == "" {
			continue
		}
		out[pid] = rec
	}
	return out
}

// alive reports whether a process id currently belongs to a running process.
// Signal 0 performs the permission and existence checks without delivering
// anything; EPERM means it exists but is not ours, which still counts.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
