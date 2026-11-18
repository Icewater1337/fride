package agent

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reading when a usage limit lifts.
//
// A turn that hits the account's usage limit ends like any other API error:
// the CLI writes the message and goes back to its prompt. But unlike a dropped
// response, repeating the request changes nothing until the limit resets, and
// the message says when that is. Claude Code has written it in two shapes over
// time: a machine form, "Claude AI usage limit reached|1760000000" (the reset as
// Unix seconds), and a sentence for a person, "5-hour limit reached ∙ resets
// 3pm", "… resets 3:30pm (Europe/Zurich)", "Weekly limit reached ∙ resets
// Nov 20, 5am", "Your limit will reset at 3pm". Anything else is not read as a
// reset: a guess would park a task for no reason, and an unknown reset is
// handled like any other API error.

var (
	// limitEpoch is the machine form. Milliseconds are accepted too: the
	// number is the whole of the message after the bar, and reading one as
	// the other would land millennia off.
	limitEpoch = regexp.MustCompile(`(?i)limit reached\|(\d{9,13})\b`)
	// limitAt is the human form: "reset(s) [at|on]", an optional month and
	// day, a time of day, and an optional IANA zone in parentheses.
	limitAt = regexp.MustCompile(`(?i)\breset(?:s)?(?:\s+(?:at|on))?\s+` +
		`(?:(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?` +
		`(\d{1,2})(?::(\d{2}))?(?:\s*([ap])\.?m\b\.?)?` +
		`(?:\s*\(([A-Za-z0-9_+\-/]+)\))?`)
)

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
	"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
	"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

// resetGrace is how far before the message a stated reset may lie and still
// be read as that moment rather than as its next occurrence. A message written
// at 15:01 that says "resets 3pm" is about the 3pm just gone — the limit was
// read a moment before it lifted, or this clock runs ahead of the API's — not
// about tomorrow's, which no session limit is that far off from. Taken as the
// past, it is retried like any other API error rather than parked for a day.
const resetGrace = time.Hour

// LimitReset reads when the usage limit an error message reports will reset,
// or the zero time when the message is not a usage limit or does not say.
//
// written is when the message was written: a time of day without a date is
// the next one after it, and a date without a year the next such date. It is
// the message's own time, not the reader's, so reading the same message again
// later gives the same answer and never rolls a reset already passed on to
// the next day. A named zone is honoured; without one, the time is in loc,
// which is time.Local outside tests.
func LimitReset(text string, written time.Time, loc *time.Location) time.Time {
	if m := limitEpoch.FindStringSubmatch(text); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}
		}
		if n >= 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if !strings.Contains(strings.ToLower(text), "limit") {
		return time.Time{} // "reset" alone could be a connection reset
	}
	m := limitAt.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	mon, day, hour, minute, ampm, zone := m[1], m[2], m[3], m[4], strings.ToLower(m[5]), m[6]
	h, _ := strconv.Atoi(hour)
	mi := 0
	if minute != "" {
		mi, _ = strconv.Atoi(minute)
	}
	switch {
	case mi > 59:
		return time.Time{}
	case ampm != "":
		if h < 1 || h > 12 {
			return time.Time{}
		}
		h %= 12
		if ampm == "p" {
			h += 12
		}
	case minute == "" || h > 23:
		// A bare number is not a time: "resets 30 days after" is not 30
		// o'clock, and "resets 3" could be either half of the day.
		return time.Time{}
	}
	if zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	if loc == nil {
		loc = time.Local
	}
	if written.IsZero() {
		written = time.Now()
	}
	ref := written.In(loc)
	floor := written.Add(-resetGrace)
	if mon == "" {
		t := time.Date(ref.Year(), ref.Month(), ref.Day(), h, mi, 0, 0, loc)
		if t.Before(floor) {
			t = time.Date(ref.Year(), ref.Month(), ref.Day()+1, h, mi, 0, 0, loc)
		}
		return t
	}
	d, _ := strconv.Atoi(day)
	month := months[strings.ToLower(mon)]
	t := time.Date(ref.Year(), month, d, h, mi, 0, 0, loc)
	if t.Month() != month || t.Day() != d {
		return time.Time{} // Feb 30: not a date, so not a reset
	}
	if t.Before(floor) {
		t = time.Date(ref.Year()+1, month, d, h, mi, 0, 0, loc)
	}
	return t
}
