package forge

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a CI log's tail is cut to: the last 64 KB of the log, of which the
// last 20 lines, each at most 400 characters. Enough to show the failing
// assertion or compiler error; the agent fetches the whole log if it needs
// more.
const (
	logBytes = 64 << 10
	logLines = 20
	lineMax  = 400
)

var (
	// csi, osc and esc are terminal escape sequences: colours, cursor moves,
	// titles. A CI log is full of them, and typed into the agent's terminal
	// they would drive it.
	csi = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	osc = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)
	esc = regexp.MustCompile(`\x1b[@-Z\\-_]?`)
	// stamp is the time GitHub Actions puts before every log line.
	stamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z ?`)
)

// Sanitize makes text from a forge safe to type into a terminal: escape
// sequences go, and every control character except a line break and a tab.
// Titles, comments and logs are written by whoever can push or comment, and
// all of them end up in the agent's prompt.
func Sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	s = osc.ReplaceAllString(s, "")
	s = csi.ReplaceAllString(s, "")
	s = esc.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, s)
}

// Tail is the part of a CI job's log worth putting in front of the agent.
//
// A carriage return inside a line is what a progress bar redraws with, and
// GitLab hides its section markers before one, so of each line only what
// follows the last is kept: what a terminal would have shown. GitHub Actions
// writes its cleanup steps after the failure, so when the log marks an error
// (##[error]) the tail ends at the last one rather than at the end of the
// log, where it would only show the runner tidying up.
func Tail(log []byte) string {
	if len(log) > logBytes {
		log = log[len(log)-logBytes:]
	}
	raw := strings.Split(strings.ReplaceAll(string(log), "\r\n", "\n"), "\n")
	var lines []string
	end := -1
	for _, l := range raw {
		if i := strings.LastIndex(l, "\r"); i >= 0 {
			l = l[i+1:]
		}
		l = strings.TrimRight(Sanitize(stamp.ReplaceAllString(l, "")), " \t")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.Contains(l, "##[error]") {
			end = len(lines)
		}
		lines = append(lines, clipRunes(l, lineMax))
	}
	if end >= 0 {
		lines = lines[:end+1]
	}
	if len(lines) > logLines {
		lines = lines[len(lines)-logLines:]
	}
	return strings.Join(lines, "\n")
}

// clipRunes cuts s to n characters, marking the cut.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// oneLine is s on one line, cut to n characters.
func oneLine(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(Sanitize(s)), " "), n)
}
