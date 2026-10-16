package journal

import "strings"

import "testing"

func TestFrontmatterRoundTrip(t *testing.T) {
	// A title that would break unquoted YAML in three separate ways.
	f := Frontmatter{
		Type:        DocType,
		Title:       `PROJ-903: "prefill", or not`,
		Description: "A one-sentence summary.",
		Timestamp:   "2026-08-15T08:24:00+02:00",
		Tags:        []string{"rationale", "backend"},
	}
	doc := f.Render() + "# Heading\n\nbody\n"
	fm, body := ParseFrontmatter(doc)
	if fm["type"] != DocType {
		t.Errorf("type = %q", fm["type"])
	}
	if fm["title"] != f.Title {
		t.Errorf("title round-trip: got %q want %q", fm["title"], f.Title)
	}
	if fm["description"] != f.Description {
		t.Errorf("description = %q", fm["description"])
	}
	if !strings.HasPrefix(body, "# Heading") {
		t.Errorf("body = %q", body)
	}
}

func TestParseFrontmatterAbsent(t *testing.T) {
	fm, body := ParseFrontmatter("# Plain\n\nno frontmatter\n")
	if len(fm) != 0 {
		t.Errorf("expected no fields, got %v", fm)
	}
	if !strings.HasPrefix(body, "# Plain") {
		t.Errorf("body was altered: %q", body)
	}
}

func TestMergeLogOrdersDatesDescendingAndDedupes(t *testing.T) {
	out := MergeLog("", "2026-05-15", []string{"**Init**: first."})
	out = MergeLog(out, "2026-05-22", []string{"**Create**: second."})
	out = MergeLog(out, "2026-05-22", []string{"**Create**: second."}) // repeat
	out = MergeLog(out, "2026-05-22", []string{"**Update**: third."})

	if !strings.HasPrefix(out, "# Update Log\n") {
		t.Fatalf("missing title heading:\n%s", out)
	}
	i22, i15 := strings.Index(out, "## 2026-05-22"), strings.Index(out, "## 2026-05-15")
	if i22 < 0 || i15 < 0 || i22 > i15 {
		t.Errorf("dates not newest-first:\n%s", out)
	}
	if n := strings.Count(out, "**Create**: second."); n != 1 {
		t.Errorf("duplicate entry written %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "**Update**: third.") {
		t.Errorf("later entry lost:\n%s", out)
	}
}

func TestRenderIndexFrontmatterOnlyAtRoot(t *testing.T) {
	secs := []IndexSection{{Heading: "2026", Entries: []IndexEntry{
		{Title: "A", Path: "rationale/2026/a.md", Description: "d"},
	}}}
	root := RenderIndex("intro", OKFVersion, secs)
	if !strings.HasPrefix(root, "---\nokf_version: \"0.1\"\n---\n") {
		t.Errorf("root index missing version declaration:\n%s", root)
	}
	if !strings.Contains(root, "* [A](rationale/2026/a.md) - d\n") {
		t.Errorf("entry not in spec shape:\n%s", root)
	}
	sub := RenderIndex("", "", secs)
	if strings.HasPrefix(sub, "---") {
		t.Errorf("non-root index must carry no frontmatter:\n%s", sub)
	}
}
