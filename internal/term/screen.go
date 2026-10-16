package term

import (
	"strings"
	"time"
)

// SendLine types text into the session and submits it, which is how the
// orchestrator answers an agent that is waiting at its prompt.
//
// The text and the Enter go in two writes with a pause between them. Every
// supported agent runs a fullscreen TUI that reads its input through a line
// editor, and pasting the newline in the same chunk as the text races that
// editor: the submit is handled before the last characters have been inserted,
// which sends half a sentence. The pause is what makes it a keystroke sequence
// rather than a paste.
//
// Newlines inside the text are turned into spaces for the same reason — each
// one would submit early, splitting a single instruction into several prompts.
func (s *Session) SendLine(text string) error {
	text = strings.TrimSpace(collapseNewlines(text))
	if text == "" {
		return nil
	}
	if err := s.Write([]byte(text)); err != nil {
		return err
	}
	time.Sleep(120 * time.Millisecond)
	return s.Write([]byte("\r"))
}

func collapseNewlines(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// AgentSession is the session the orchestrator supervises for a task: the
// newest agent tab that is still running.
//
// Newest rather than first: a task whose agent died and was resumed has both
// tabs, and the live conversation is the later one. Nil means there is nothing
// to supervise — every agent tab has exited, or none was ever opened.
func (m *Manager) AgentSession(task string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Session
	for _, s := range m.sessions {
		if s.Task != task || s.Kind != KindAgent || !s.Alive() {
			continue
		}
		if best == nil || s.Created.After(best.Created) {
			best = s
		}
	}
	return best
}

// Waiting is whether this session has finished its turn and is sitting at its
// prompt — the same signal the board's amber dot uses. Read under the lock,
// because the flag is written from the PTY reader and the idle watcher.
func (s *Session) Waiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Attention && !s.closed
}

// IdleFor is how long the session's output has been quiet. Zero while it is
// producing, and zero for a session that has never written anything.
func (s *Session) IdleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastOutput.IsZero() {
		return 0
	}
	return time.Since(s.lastOutput)
}

// Screen renders the last lines of the session's output as readable text.
//
// This is the fallback way to see what an agent is doing, used only for the
// CLIs whose conversation cannot be read from a transcript. It is genuinely
// lossy: a TUI paints by addressing cursor positions on the alternate screen,
// so what comes out here is the order the bytes were written in, not the
// picture they compose. Enough to tell "it is asking something" from "it is
// working"; not enough to trust as a quotation.
func (s *Session) Screen(lines int) string {
	s.mu.Lock()
	raw := make([]byte, len(s.buf))
	copy(raw, s.buf)
	s.mu.Unlock()

	out := []string{}
	for _, l := range strings.Split(stripANSI(string(raw)), "\n") {
		l = strings.TrimRight(l, " \t\r")
		// Collapse the runs of blank lines a repainting TUI leaves behind, so the
		// budget is spent on content rather than on whitespace.
		if l == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if lines > 0 && len(out) > lines {
		out = out[len(out)-lines:]
	}
	return strings.Join(out, "\n")
}

// stripANSI removes escape sequences and the control bytes that carry no text.
//
// It handles the three forms that actually appear: CSI (`ESC [` … final byte in
// @-~), OSC (`ESC ]` … BEL or ST), and the two-byte escapes everything else
// collapses to. Anything unrecognised after ESC is dropped along with its next
// byte rather than being emitted, because a stray `[?25l` in the middle of a
// sentence is worse than a missing character.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			i += 2
			for i < len(s) && !(s[i] >= '@' && s[i] <= '~') {
				i++
			}
			i++ // the final byte
		case c == 0x1b && i+1 < len(s) && s[i+1] == ']':
			i += 2
			for i < len(s) {
				if s[i] == 0x07 { // BEL terminator
					i++
					break
				}
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' { // ST
					i += 2
					break
				}
				i++
			}
		case c == 0x1b:
			i += 2
		case c == '\r':
			// A bare CR rewinds the line for an in-place repaint. Treated as a
			// break so the overwritten text does not run into what replaced it.
			b.WriteByte('\n')
			i++
		case c == '\n' || c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++ // BEL, backspace, and the rest carry no text
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
