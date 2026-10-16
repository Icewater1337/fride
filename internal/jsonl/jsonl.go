// Package jsonl reads line-delimited JSON: the transcripts agent CLIs write,
// and anything else kept one record per line.
//
// It exists because the obvious tool, bufio.Scanner, fails quietly in exactly
// the way that matters for a transcript. Its token limit is hard: the first
// line over it ends the scan for good, the only report is Err, and nothing
// that used it here ever looked. A transcript line can carry a whole tool
// result, so one large file read would hide every turn written after it — an
// agent would look silent while it was working, and its token usage would
// stop counting.
package jsonl

import (
	"bufio"
	"bytes"
	"io"
)

// MaxLine is the longest line handed to a callback. A longer one is skipped,
// not fatal, so what follows it is still read.
const MaxLine = 16 << 20

// Scan calls fn with each non-blank line of r, in order, without its line
// ending, until fn returns false or r runs out. A line longer than MaxLine is
// skipped. The error is the reader's, and nil at the end of the input.
//
// The slice passed to fn is only valid until fn returns.
func Scan(r io.Reader, fn func(line []byte) bool) error {
	return scan(r, MaxLine, fn)
}

func scan(r io.Reader, max int, fn func([]byte) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	// long collects a line that outgrew the reader's buffer; over marks one
	// that has outgrown max as well, and is being read past rather than kept.
	var long []byte
	over := false
	for {
		frag, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if !over {
				if len(long)+len(frag) > max {
					over, long = true, long[:0]
				} else {
					long = append(long, frag...)
				}
			}
			continue
		}
		line := frag
		if len(long) > 0 {
			long = append(long, frag...)
			line = long
		}
		if !over && len(line) <= max {
			if line = bytes.TrimSpace(line); len(line) > 0 && !fn(line) {
				return nil
			}
		}
		long, over = long[:0], false
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
