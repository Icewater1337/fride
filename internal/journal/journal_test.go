package journal

import (
	"os"
	"strings"
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/jsonl"
)

// One oversized line used to end Read silently, dropping every later entry.
func TestReadSkipsAnOversizedLineAndKeepsTheRest(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if err := Append(cfg, "t", Entry{Kind: KindNote, Text: "before"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(Path(cfg, "t"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"kind":"note","text":"` + strings.Repeat("x", jsonl.MaxLine) + "\"}\n")
	f.Close()
	if err := Append(cfg, "t", Entry{Kind: KindNote, Text: "after"}); err != nil {
		t.Fatal(err)
	}

	got, err := Read(cfg, "t")
	if err != nil || len(got) != 2 || got[0].Text != "before" || got[1].Text != "after" {
		t.Fatalf("got %d entries (err %v), want before and after", len(got), err)
	}
}

func TestAppendRefusesAnEntryReadCannotReturn(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if Append(cfg, "t", Entry{Kind: KindPlan, Text: strings.Repeat("x", jsonl.MaxLine)}) == nil {
		t.Fatal("an entry over jsonl.MaxLine was accepted")
	}
	if err := Append(cfg, "t", Entry{Kind: KindPlan, Text: strings.Repeat("x", 9<<20)}); err != nil {
		t.Fatalf("a 9 MiB plan was refused: %v", err)
	}
	if got, err := Read(cfg, "t"); err != nil || len(got) != 1 {
		t.Fatalf("got %d entries (err %v), want the 9 MiB plan back", len(got), err)
	}
}
