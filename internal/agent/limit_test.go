package agent

import (
	"testing"
	"time"
)

// Each shape a usage limit's reset has been written in is read, relative to
// when the message was written, and nothing else is read as one.
func TestLimitResetReadsEachShape(t *testing.T) {
	here := time.FixedZone("here", 2*3600) // the reader's zone, when none is named
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Skip("no time zone database:", err)
	}
	newYork, _ := time.LoadLocation("America/New_York")
	at := func(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, loc)
	}
	morning := at(2026, time.November, 18, 10, 0, here)
	cases := []struct {
		name    string
		text    string
		written time.Time
		want    time.Time
	}{
		{"the machine form, in seconds", "Claude AI usage limit reached|1763470800", morning,
			time.Unix(1763470800, 0)},
		{"the machine form, in milliseconds", "Claude AI usage limit reached|1763470800000", morning,
			time.Unix(1763470800, 0)},
		{"a time of day later today", "5-hour limit reached ∙ resets 3pm", morning,
			at(2026, time.November, 18, 15, 0, here)},
		{"with minutes and a named zone", "Session limit reached ∙ resets 3:30pm (Europe/Zurich)", morning,
			at(2026, time.November, 18, 15, 30, zurich)},
		{"a sentence", "Claude usage limit reached. Your limit will reset at 3pm (America/New_York).", morning,
			at(2026, time.November, 18, 15, 0, newYork)},
		{"spaced and dotted", "You've hit your limit · resets 3 p.m.", morning,
			at(2026, time.November, 18, 15, 0, here)},
		{"a 24-hour clock", "Usage limit reached, resets at 15:00", morning,
			at(2026, time.November, 18, 15, 0, here)},
		{"midnight", "5-hour limit reached ∙ resets 12am", morning,
			at(2026, time.November, 19, 0, 0, here)},
		{"a time of day already past is tomorrow's", "5-hour limit reached ∙ resets 9am",
			at(2026, time.November, 18, 22, 0, here), at(2026, time.November, 19, 9, 0, here)},
		{"a time just past is the one just gone, not tomorrow's", "5-hour limit reached ∙ resets 3pm",
			at(2026, time.November, 18, 15, 1, here), at(2026, time.November, 18, 15, 0, here)},
		{"a date", "Weekly limit reached ∙ resets Nov 20, 5am", morning,
			at(2026, time.November, 20, 5, 0, here)},
		{"a date in words, with at and a zone", "Opus weekly limit reached ∙ resets November 20 at 5:15am (Europe/Zurich)",
			morning, at(2026, time.November, 20, 5, 15, zurich)},
		{"a date past is next year's", "Weekly limit reached ∙ resets Jan 2, 10am",
			at(2026, time.December, 30, 10, 0, here), at(2027, time.January, 2, 10, 0, here)},
		{"a zone this machine does not know is the reader's", "5-hour limit reached ∙ resets 3pm (Mars/Olympus)",
			morning, at(2026, time.November, 18, 15, 0, here)},

		{"an API error that is no limit", "API Error: The response stopped arriving.", morning, time.Time{}},
		{"a reset that is no limit", "API Error: connection reset at 3pm", morning, time.Time{}},
		{"a limit that does not say when", "API Error: Rate limit reached", morning, time.Time{}},
		{"a limit that says so in other words", "Usage limit reached ∙ resets in 2 hours", morning, time.Time{}},
		{"a number that is no time", "Usage limit reached ∙ resets 30 days after", morning, time.Time{}},
		{"an hour that is no hour", "Usage limit reached ∙ resets 13pm", morning, time.Time{}},
		{"a day that is no day", "Weekly limit reached ∙ resets Feb 30, 5am", morning, time.Time{}},
	}
	for _, c := range cases {
		got := LimitReset(c.text, c.written, here)
		if !got.Equal(c.want) {
			t.Errorf("%s: LimitReset(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
	// Read again later, the same message gives the same answer: a reset that
	// has passed meanwhile is not rolled on to the next day.
	text, written := "5-hour limit reached ∙ resets 3pm", morning
	first := LimitReset(text, written, here)
	if again := LimitReset(text, written, here); !again.Equal(first) {
		t.Fatalf("read again: %v, then %v", first, again)
	}
}

// Failed carries the reset of a turn that ended on a usage limit, and none
// for any other API error.
func TestFailedReadsTheResetOfAUsageLimit(t *testing.T) {
	home := t.TempDir()
	limit := assistant("e1", "Claude AI usage limit reached|1763470800")
	limit["isApiErrorMessage"] = true
	limit["error"] = "rate_limit"
	limit["timestamp"] = "2026-11-18T10:00:00.000Z"
	writeTurns(t, home, "/ws", "limited", []map[string]any{assistant("a1", "working on it"), limit})
	f, err := Failed(Claude, home, "/ws", "limited", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if f == nil || f.UUID != "e1" || !f.Reset.Equal(time.Unix(1763470800, 0)) {
		t.Fatalf("Failed = %+v, want e1 resetting at %v", f, time.Unix(1763470800, 0))
	}

	other := assistant("e2", "API Error: The response stopped arriving.")
	other["isApiErrorMessage"] = true
	writeTurns(t, home, "/ws", "dropped", []map[string]any{other})
	if f, _ := Failed(Claude, home, "/ws", "dropped", time.Time{}); f == nil || !f.Reset.IsZero() {
		t.Fatalf("Failed = %+v, want an error with no reset", f)
	}
}
