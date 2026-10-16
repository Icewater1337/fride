package jsonl

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, r io.Reader, max int) ([]string, error) {
	t.Helper()
	var got []string
	err := scan(r, max, func(l []byte) bool {
		got = append(got, string(l))
		return true
	})
	return got, err
}

// One oversized line used to end the read for good, silently. It has to cost
// that line and nothing after it.
func TestAnOversizedLineIsSkippedNotFatal(t *testing.T) {
	in := "first\n" + strings.Repeat("x", 300<<10) + "\nlast\n"
	got, err := collect(t, strings.NewReader(in), 100<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "last" {
		t.Fatalf("got %d lines %.20q, want first and last", len(got), got)
	}
}

// Longer than the reader's own buffer but within the limit: delivered whole.
func TestALongLineWithinTheLimitArrivesWhole(t *testing.T) {
	long := strings.Repeat("y", 200<<10)
	got, err := collect(t, strings.NewReader("a\r\n"+long+"\n\n  \nb"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != long || got[2] != "b" {
		t.Fatalf("got %d lines, want a, the long line, and b (blank lines dropped, no newline needed at the end)", len(got))
	}
}

func TestAReadErrorIsReported(t *testing.T) {
	boom := errors.New("disk on fire")
	r := io.MultiReader(strings.NewReader("ok\n"), errReader{boom})
	got, err := collect(t, r, MaxLine)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("got %q, want the line before the failure", got)
	}
}

func TestTheCallbackCanStopTheRead(t *testing.T) {
	n := 0
	err := Scan(strings.NewReader("1\n2\n3\n"), func([]byte) bool { n++; return n < 2 })
	if err != nil || n != 2 {
		t.Fatalf("read %d lines (err %v), want to stop after 2", n, err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
