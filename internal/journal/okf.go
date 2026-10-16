package journal

// Open Knowledge Format (https://okf.md/), version 0.1.
//
// The format is small: a directory of markdown files, every non-reserved one
// carrying YAML frontmatter with a non-empty `type`. `index.md` and `log.md`
// are reserved and have shapes of their own — index.md carries no frontmatter
// except at a bundle root, where it may declare the version it targets.
//
// Conforming matters here because the rationale documents are written for
// something else to read later. A bare markdown file has to be parsed by
// guesswork; a bundle states what each document is, what it is about, and when
// it was written, in a form an agent can consume without being told how.
//
// The YAML written here is deliberately narrow — scalars and one flat list —
// so it needs no dependency. Every scalar is double-quoted, which is always
// valid and removes any question of a title that begins with a digit, contains
// a colon, or happens to spell `true`.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OKFVersion is the spec version these bundles target, declared in the root
// index.md as the spec allows.
const OKFVersion = "0.1"

// DocType is the `type` every rationale document declares. The spec keeps type
// values unregistered and asks producers to pick something self-explanatory.
const DocType = "Design Rationale"

// yamlString renders a Go string as a double-quoted YAML scalar.
func yamlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

// Frontmatter is the subset of OKF fields these documents use. Only Type is
// required by the spec; the rest are optional but are what make an index
// readable without opening every file.
type Frontmatter struct {
	Type        string
	Title       string
	Description string
	Timestamp   string // ISO 8601
	Tags        []string
	Extra       [][2]string // rendered after the known keys, in order
}

func (f Frontmatter) Render() string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "type: %s\n", yamlString(f.Type))
	if f.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", yamlString(f.Title))
	}
	if f.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", yamlString(f.Description))
	}
	if f.Timestamp != "" {
		fmt.Fprintf(&b, "timestamp: %s\n", yamlString(f.Timestamp))
	}
	if len(f.Tags) > 0 {
		q := make([]string, len(f.Tags))
		for i, t := range f.Tags {
			q[i] = yamlString(t)
		}
		fmt.Fprintf(&b, "tags: [%s]\n", strings.Join(q, ", "))
	}
	for _, kv := range f.Extra {
		fmt.Fprintf(&b, "%s: %s\n", kv[0], yamlString(kv[1]))
	}
	b.WriteString("---\n\n")
	return b.String()
}

// ParseFrontmatter reads the leading YAML block. It is deliberately tolerant:
// the spec tells consumers to cope with unknown keys and missing optional
// fields, and this only ever needs the few values an index entry shows.
func ParseFrontmatter(md string) (map[string]string, string) {
	out := map[string]string{}
	if !strings.HasPrefix(md, "---\n") {
		return out, md
	}
	rest := md[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return out, md
	}
	block := rest[:end]
	body := rest[end+len("\n---"):]
	// Drop the rest of the closing line and the blank line after it, so the
	// body starts at its first real character.
	body = strings.TrimLeft(body, "\n")

	for _, line := range strings.Split(block, "\n") {
		i := strings.Index(line, ":")
		if i <= 0 || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			r := strings.NewReplacer(`\"`, `"`, `\n`, "\n", `\t`, "\t", `\r`, "\r", `\\`, `\`)
			val = r.Replace(val[1 : len(val)-1])
		}
		out[key] = val
	}
	return out, body
}

// IndexEntry is one line of an index.md.
type IndexEntry struct {
	Title       string
	Path        string // relative to the index file
	Description string
}

// IndexSection groups entries under a heading.
type IndexSection struct {
	Heading string
	Entries []IndexEntry
}

// RenderIndex writes a reserved index.md. Only a bundle root may carry
// frontmatter, and only to declare the version, so okfVersion is passed empty
// for any index below the root.
func RenderIndex(title, okfVersion string, sections []IndexSection) string {
	var b strings.Builder
	if okfVersion != "" {
		b.WriteString("---\n")
		fmt.Fprintf(&b, "okf_version: %s\n", yamlString(okfVersion))
		b.WriteString("---\n\n")
	}
	if title != "" {
		fmt.Fprintf(&b, "%s\n\n", title)
	}
	for _, s := range sections {
		fmt.Fprintf(&b, "# %s\n\n", s.Heading)
		for _, e := range s.Entries {
			if e.Description != "" {
				fmt.Fprintf(&b, "* [%s](%s) - %s\n", e.Title, e.Path, e.Description)
			} else {
				fmt.Fprintf(&b, "* [%s](%s)\n", e.Title, e.Path)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// MergeLog adds lines under an ISO date heading in a reserved log.md, keeping
// dates in descending order as the spec requires. Existing entries are kept and
// an identical line is not repeated, so re-publishing an unchanged document
// does not grow the log.
//
// Only date sections are carried over: anything else in the file is dropped
// rather than guessed at, which is safe because this file is generated.
func MergeLog(existing, date string, lines []string) string {
	type day struct {
		date  string
		lines []string
	}
	var days []day
	idx := map[string]int{}

	cur := ""
	for _, line := range strings.Split(existing, "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			cur = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			if _, seen := idx[cur]; !seen {
				idx[cur] = len(days)
				days = append(days, day{date: cur})
			}
		case strings.HasPrefix(line, "* ") && cur != "":
			d := &days[idx[cur]]
			d.lines = append(d.lines, line)
		}
	}
	if _, seen := idx[date]; !seen {
		idx[date] = len(days)
		days = append(days, day{date: date})
	}
	d := &days[idx[date]]
	for _, l := range lines {
		entry := "* " + l
		dup := false
		for _, have := range d.lines {
			if have == entry {
				dup = true
				break
			}
		}
		if !dup {
			d.lines = append(d.lines, entry)
		}
	}

	sort.SliceStable(days, func(i, j int) bool { return days[i].date > days[j].date })

	var b strings.Builder
	b.WriteString("# Update Log\n\n")
	for _, dd := range days {
		if len(dd.lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n", dd.date)
		for _, l := range dd.lines {
			b.WriteString(l + "\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ScanBundle reads every concept document under root (skipping the reserved
// names) and returns one index entry per file, so an index can be rebuilt from
// what is actually on disk rather than from what this process happens to know.
func ScanBundle(root, sub string) ([]IndexEntry, error) {
	var out []IndexEntry
	dir := filepath.Join(root, sub)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if d.Name() == "index.md" || d.Name() == "log.md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fm, _ := ParseFrontmatter(string(b))
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		title := fm["title"]
		if title == "" {
			title = strings.TrimSuffix(d.Name(), ".md")
		}
		out = append(out, IndexEntry{Title: title, Path: filepath.ToSlash(rel), Description: fm["description"]})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path > out[j].Path })
	return out, nil
}
