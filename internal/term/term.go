// Package term manages PTY-backed terminal sessions: the per-task agent
// session plus any extra shell tabs.
//
// Every supported agent CLI is a fullscreen TUI, so a real PTY (not pipes) is
// required — otherwise it will not render and will not accept keys.
package term

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/agent"
	"github.com/creack/pty"
)

// scrollback is how much recent output a reattaching browser gets replayed.
// Enough to restore a screenful of a TUI without unbounded memory per session.
const scrollback = 256 * 1024

// Session kinds. KindAgent covers every agent CLI: which one it is shows in the
// title, and nothing here needs to know beyond "this is an agent, not a shell".
const (
	KindAgent = "agent"
	KindShell = "shell"
)

type Session struct {
	ID      string    `json:"id"`
	Task    string    `json:"task"`
	Title   string    `json:"title"`
	Kind    string    `json:"kind"` // KindAgent | KindShell
	Dir     string    `json:"dir"`
	Created time.Time `json:"created"`
	// Dangerous records that this agent was launched with permission checks
	// bypassed, so the UI can keep flagging it even after the task's setting
	// changes — the flag is fixed at exec time.
	Dangerous bool `json:"dangerous,omitempty"`
	// ResumedFrom is the Claude transcript id this session owns — either the one
	// it was resumed from, or the one it created. It is what makes a resume
	// button disappear once its conversation is live again.
	ResumedFrom string `json:"resumedFrom,omitempty"`

	// Busy is true while the process is actively producing output; Attention is
	// set when it stops, i.e. the agent finished its turn and is waiting on you.
	Busy      bool `json:"busy"`
	Attention bool `json:"attention"`

	mu         sync.Mutex
	claims     []string
	ptmx       *os.File
	cmd        *exec.Cmd
	buf        []byte
	subs       map[chan []byte]struct{}
	closed     bool
	exitMsg    string
	lastOutput time.Time
	rows, cols uint16
}

// idleAfter is how long output must stop before a session counts as waiting.
//
// Claude Code's TUI repaints its spinner and elapsed-time counter roughly every
// second while it is working — including while a tool runs — so a gap this long
// reliably means it has finished the turn and is sitting at the prompt.
const idleAfter = 3 * time.Second

func (s *Session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Write sends user keystrokes to the process. Typing is the acknowledgement
// that clears the attention flag — nothing else does, so a notice cannot be
// dismissed by merely looking at the tab.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	s.Attention = false
	s.Busy = true
	s.lastOutput = time.Now()
	f, closed := s.ptmx, s.closed
	s.mu.Unlock()
	if closed || f == nil {
		return errors.New("session has exited")
	}
	_, err := f.Write(p)
	return err
}

func (s *Session) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		// A pane measured while it is hidden reports nothing. Passing that on
		// would tell the TUI it has no room and make it reflow to a single line.
		return nil
	}
	s.mu.Lock()
	f, closed := s.ptmx, s.closed
	if !closed {
		s.rows, s.cols = rows, cols
	}
	s.mu.Unlock()
	if closed || f == nil {
		return nil
	}
	return pty.Setsize(f, &pty.Winsize{Rows: rows, Cols: cols})
}

// Redraw makes a fullscreen TUI repaint itself from scratch.
//
// A browser that reattaches gets the scrollback replayed into a brand-new
// terminal, and for a line-oriented program that is enough. A TUI draws by
// addressing cursor positions on the alternate screen, so replaying its old
// bytes reconstructs nothing — the pane comes back blank or mangled and the
// agent looks like it has died, when it is only waiting quietly.
//
// There is no "please repaint" escape sequence to send; the portable trigger is
// a window-size change, which every TUI answers with a full redraw. So the size
// is nudged by one row and put straight back. The pause between the two is what
// makes it work: without it the second Setsize can land before the process has
// handled the first SIGWINCH, and both collapse into no change at all.
func (s *Session) Redraw() {
	s.mu.Lock()
	f, closed, rows, cols := s.ptmx, s.closed, s.rows, s.cols
	s.mu.Unlock()
	if closed || f == nil || rows < 2 || cols == 0 {
		return
	}
	_ = pty.Setsize(f, &pty.Winsize{Rows: rows - 1, Cols: cols})
	time.Sleep(60 * time.Millisecond)
	_ = pty.Setsize(f, &pty.Winsize{Rows: rows, Cols: cols})
}

// Subscribe replays the scrollback and then streams live output. The returned
// cancel func must be called by the caller when the websocket goes away.
func (s *Session) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	s.mu.Lock()
	history := make([]byte, len(s.buf))
	copy(history, s.buf)
	if s.subs == nil {
		s.subs = map[chan []byte]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	if len(history) > 0 {
		ch <- history
	}
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

func (s *Session) publish(p []byte) {
	s.mu.Lock()
	s.lastOutput = time.Now()
	if !s.Busy {
		s.Busy = true
	}
	// A BEL is Claude Code's terminal-bell notification. It only arrives when
	// the user has that channel enabled, so it is a bonus signal, not the
	// primary one — but when it does arrive it is unambiguous.
	if bytes.IndexByte(p, 0x07) >= 0 {
		s.Attention = true
	} else {
		s.Attention = false
	}
	s.buf = append(s.buf, p...)
	if len(s.buf) > scrollback {
		s.buf = s.buf[len(s.buf)-scrollback:]
	}
	for ch := range s.subs {
		select {
		case ch <- append([]byte(nil), p...):
		default: // a stalled browser must never block the PTY reader
		}
	}
	s.mu.Unlock()
}

func (s *Session) close(msg string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.exitMsg = msg
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
	f := s.ptmx
	s.ptmx = nil
	s.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// SetTitle renames the session. Titles are display-only and live for as long as
// the server does, which matches the lifetime of the session itself.
func (s *Session) SetTitle(t string) {
	t = strings.TrimSpace(t)
	if len(t) > 40 {
		t = t[:40]
	}
	s.mu.Lock()
	if t != "" {
		s.Title = t
	}
	s.mu.Unlock()
}

// Kill terminates the session's process group.
func (s *Session) Kill() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	s.close("killed")
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
	// released is task -> transcript ids whose owning process this Manager
	// watched exit. Closing a tab drops the session, and with it the only
	// evidence that its conversation is no longer being written; without this
	// ledger such a conversation would look indistinguishable from one that is
	// still open in a process fride cannot see.
	released map[string]map[string]bool
}

func NewManager() *Manager {
	m := &Manager{sessions: map[string]*Session{}, released: map[string]map[string]bool{}}
	go m.watchIdle()
	return m
}

// watchIdle flips a session from busy to "waiting for you" once its output has
// been quiet for idleAfter. Only agent sessions raise attention: a shell sitting
// at a prompt is its normal state, not something to notify about.
func (m *Manager) watchIdle() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		sessions := make([]*Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			sessions = append(sessions, s)
		}
		m.mu.Unlock()
		now := time.Now()
		for _, s := range sessions {
			s.mu.Lock()
			if s.Kind == KindAgent && s.Busy && !s.closed &&
				!s.lastOutput.IsZero() && now.Sub(s.lastOutput) > idleAfter {
				s.Busy = false
				s.Attention = true
			}
			s.mu.Unlock()
		}
	}
}

// Attention reports whether any agent session of a task is waiting for input.
func (m *Manager) Attention(task string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Task != task {
			continue
		}
		s.mu.Lock()
		a := s.Attention && !s.closed
		s.mu.Unlock()
		if a {
			return true
		}
	}
	return false
}

// Start launches a command under a PTY in dir.
func (m *Manager) Start(task, kind, title, dir string, argv []string, env []string) (*Session, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("working directory %s does not exist", dir)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(),
		append([]string{"TERM=xterm-256color", "COLORTERM=truecolor"}, env...)...)

	const startRows, startCols = 30, 100
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: startRows, Cols: startCols})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}

	m.mu.Lock()
	m.seq++
	// Random suffix: ids appear in the websocket URL, and a predictable
	// "<task>-1" would be trivially guessable by anything that got past the
	// origin checks.
	id := fmt.Sprintf("%s-%d-%s", task, m.seq, randSuffix())
	s := &Session{
		ID: id, Task: task, Title: title, Kind: kind, Dir: dir,
		Created: time.Now(), ptmx: ptmx, cmd: cmd, subs: map[chan []byte]struct{}{},
		rows: startRows, cols: startCols,
	}
	m.sessions[id] = s
	m.mu.Unlock()

	go func() {
		bufr := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(bufr)
			if n > 0 {
				s.publish(bufr[:n])
			}
			if err != nil {
				break
			}
		}
		err := cmd.Wait()
		msg := "\r\n\x1b[90m[process exited]\x1b[0m\r\n"
		if err != nil {
			msg = fmt.Sprintf("\r\n\x1b[90m[process exited: %v]\x1b[0m\r\n", err)
		}
		s.publish([]byte(msg))
		s.close(msg)
	}()
	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// ForTask lists a task's sessions, oldest first, so the agent session stays tab 1.
func (m *Manager) ForTask(task string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	// non-nil: this is serialised straight to JSON and the UI iterates it, so a
	// nil slice would arrive as `null` and blow up on a task with no terminals
	out := []*Session{}
	for _, s := range m.sessions {
		if s.Task == task {
			out = append(out, s)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Created.Before(out[i].Created) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (m *Manager) Close(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		m.release(s)
		s.Kill()
	}
}

// release records that a session's conversations have lost their writer.
func (m *Manager) release(s *Session) {
	if s.Kind != KindAgent {
		return
	}
	hist := s.claimHistory()
	if len(hist) == 0 {
		return
	}
	m.mu.Lock()
	set := m.released[s.Task]
	if set == nil {
		set = map[string]bool{}
		m.released[s.Task] = set
	}
	for _, id := range hist {
		set[id] = true
	}
	m.mu.Unlock()
}

// ReleasedIDs returns the conversations of a task that are known to be free:
// every one whose owning process this Manager saw exit, whether the tab was
// then closed or is still sitting there showing "[process exited]".
//
// Positive knowledge, and the only kind available. Everything else about a
// transcript is inference from what it looks like on disk.
func (m *Manager) ReleasedIDs(task string) map[string]bool {
	m.mu.Lock()
	out := map[string]bool{}
	for id := range m.released[task] {
		out[id] = true
	}
	var dead []*Session
	for _, s := range m.sessions {
		if s.Task == task && s.Kind == KindAgent {
			dead = append(dead, s)
		}
	}
	m.mu.Unlock()
	for _, s := range dead {
		if s.Alive() {
			continue
		}
		for _, id := range s.claimHistory() {
			out[id] = true
		}
	}
	return out
}

// CloseTask tears down every session of a task (used when a task moves to Done).
func (m *Manager) CloseTask(task string) int {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		if s.Task == task {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
	return len(ids)
}

// cleanEnv strips the markers Claude Code sets for its own child processes.
// The reasoning lives with the agent table, next to everything else this tool
// knows about launching one — see agent.CleanEnv.
func cleanEnv() []string { return agent.CleanEnv() }

// randSuffix returns a short unguessable token for session ids.
func randSuffix() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable here; a predictable id would
		// silently weaken the websocket, so refuse to produce one.
		panic("fride: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Claim records which Claude transcript this session owns.
//
// A session can claim more than one over its life: the id it was launched with
// is only the first, and /clear moves it to another. Every one is remembered,
// because when this process dies all of them are free to reopen and nothing
// else on the machine knows they were ever connected.
func (s *Session) Claim(id string) {
	s.mu.Lock()
	s.ResumedFrom = id
	if id != "" && !contains(s.claims, id) {
		s.claims = append(s.claims, id)
	}
	s.mu.Unlock()
}

// claimHistory is every transcript this session has owned.
func (s *Session) claimHistory() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.claims...)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Session) Claimed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResumedFrom
}

// Pid is the process id of the session's child, or 0 once it has exited.
//
// It is what lets the claim be checked against the agent CLI's own record of
// what that process is doing, instead of trusting the argv it was started with
// — which stops being true the moment the user runs /clear.
func (s *Session) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// ClaimedIDs returns the transcript ids currently owned by live agent sessions
// of a task, so those conversations are not offered for resuming again.
//
// A tab whose process has exited claims nothing: the conversation outlived it
// and reopening it is exactly what the resume button is for. Holding the claim
// until the dead tab is closed would only hide the one action left to take.
func (m *Manager) ClaimedIDs(task string) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, s := range m.sessions {
		if s.Task != task || s.Kind != KindAgent || !s.Alive() {
			continue
		}
		if id := s.Claimed(); id != "" {
			out[id] = true
		}
	}
	return out
}
