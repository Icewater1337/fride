// Package panels stores the web UI panels the orchestrator builds for itself:
// small HTML/JS apps shown as tabs beside FrIDE's own views.
//
// Each panel is two files in one folder. <id>.html is the current source, kept
// as a plain file so the orchestrator's Read tool can open it and edit it with
// find/replace edits instead of writing the whole page again. <id>.json holds
// everything else: title, summary, the user's hide choice, the last error the
// UI reported, the panel's own persisted state, and the previous versions.
package panels

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/atomicfile"
)

const (
	// MaxHTML bounds one panel's source. A dashboard with inline data and
	// styles fits comfortably; anything bigger is a sign the model is pasting
	// data into the page that it should fetch through the bridge instead.
	MaxHTML = 512 << 10
	// MaxState bounds a panel's persisted state, written by the panel itself.
	MaxState = 256 << 10
	// MaxPanels bounds how many exist at once, so a model that creates a panel
	// on every turn fills a list the user can still read, not the disk.
	MaxPanels = 24
	// KeepVersions is how many earlier versions are kept for a restore.
	KeepVersions = 5
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidID reports whether id can name a panel. It is also a file name, so it
// is the only thing standing between a model's string and a path.
func ValidID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("panel id %q: use 1-40 lowercase letters, digits and dashes", id)
	}
	return nil
}

// Version is one earlier source of a panel.
type Version struct {
	Version int       `json:"version"`
	Note    string    `json:"note,omitempty"`
	Time    time.Time `json:"time"`
	HTML    string    `json:"html,omitempty"`
}

// Panel is a panel's metadata. HTML is filled only by Get.
type Panel struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Summary string    `json:"summary,omitempty"`
	Version int       `json:"version"`
	Note    string    `json:"note,omitempty"`
	Updated time.Time `json:"updated"`
	// Hidden is the user closing the tab. The panel is kept; the orchestrator
	// sees the choice and should not reopen it unasked.
	Hidden bool `json:"hidden,omitempty"`
	// Error is the last error the UI caught from the panel, cleared by the
	// next version.
	Error   string          `json:"error,omitempty"`
	State   json.RawMessage `json:"state,omitempty"`
	History []Version       `json:"history,omitempty"`
	HTML    string          `json:"html,omitempty"`
}

// Edit replaces one exact occurrence of Find with Replace.
type Edit struct {
	Find    string `json:"find"`
	Replace string `json:"replace"`
}

// Store is the panels folder. Safe for concurrent use within one process;
// only the server writes it.
type Store struct {
	mu  sync.Mutex
	dir string
}

func New(dir string) *Store { return &Store{dir: dir} }

// Dir is the folder, which the orchestrator is allowed to read.
func (s *Store) Dir() string { return s.dir }

// Path is where a panel's current source is.
func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+".html") }

func (s *Store) meta(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) load(id string) (*Panel, error) {
	if err := ValidID(id); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.meta(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no panel %q", id)
	}
	if err != nil {
		return nil, err
	}
	var p Panel
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("panel %q: %w", id, err)
	}
	return &p, nil
}

func (s *Store) save(p *Panel) error {
	p.HTML = ""
	return atomicfile.WriteJSON(s.meta(p.ID), p, 0o600)
}

// List is every panel, oldest first, without sources or history.
func (s *Store) List() []Panel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

func (s *Store) list() []Panel {
	names, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	out := []Panel{}
	for _, n := range names {
		p, err := s.load(strings.TrimSuffix(filepath.Base(n), ".json"))
		if err != nil {
			continue
		}
		p.History = nil
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get is one panel with its current source.
func (s *Store) Get(id string) (Panel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return Panel{}, err
	}
	b, err := os.ReadFile(s.Path(id))
	if err != nil {
		return Panel{}, err
	}
	p.HTML = string(b)
	for i := range p.History {
		p.History[i].HTML = "" // listed, not shipped: Restore brings one back
	}
	return *p, nil
}

// Upsert creates a panel or writes its next version. The new source is html,
// or, when html is empty, the current source with edits applied. Title and
// summary keep their value when empty. A new version unhides the panel: the
// orchestrator changing it is a reason to show it again.
func (s *Store) Upsert(id, title, summary, note, html string, edits []Edit) (Panel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ValidID(id); err != nil {
		return Panel{}, err
	}
	p, err := s.load(id)
	isNew := err != nil
	if isNew {
		if len(s.list()) >= MaxPanels {
			return Panel{}, fmt.Errorf("there are already %d panels; remove one first", MaxPanels)
		}
		if strings.TrimSpace(title) == "" {
			return Panel{}, fmt.Errorf("a new panel needs a title")
		}
		p = &Panel{ID: id}
	}
	var old string
	if !isNew {
		b, err := os.ReadFile(s.Path(id))
		if err != nil {
			return Panel{}, err
		}
		old = string(b)
	}
	switch {
	case html != "":
	case len(edits) > 0 && !isNew:
		if html, err = applyEdits(old, edits); err != nil {
			return Panel{}, err
		}
	default:
		return Panel{}, fmt.Errorf("panel %q: give html, or edits to an existing panel", id)
	}
	if len(html) > MaxHTML {
		return Panel{}, fmt.Errorf("panel %q is %d bytes; the limit is %d", id, len(html), MaxHTML)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Panel{}, err
	}
	if !isNew {
		p.History = append([]Version{{Version: p.Version, Note: p.Note, Time: p.Updated, HTML: old}}, p.History...)
		if len(p.History) > KeepVersions {
			p.History = p.History[:KeepVersions]
		}
	}
	if t := strings.TrimSpace(title); t != "" {
		p.Title = t
	}
	if sm := strings.TrimSpace(summary); sm != "" {
		p.Summary = sm
	}
	p.Version++
	p.Note, p.Updated, p.Hidden, p.Error = strings.TrimSpace(note), time.Now(), false, ""
	if err := atomicfile.Write(s.Path(id), []byte(html), 0o600); err != nil {
		return Panel{}, err
	}
	if err := s.save(p); err != nil {
		return Panel{}, err
	}
	p.HTML = html
	return *p, nil
}

// applyEdits applies each edit in order. Each Find must occur exactly once in
// the source as it stands at that edit, so an edit never lands somewhere the
// model did not mean.
func applyEdits(src string, edits []Edit) (string, error) {
	for i, e := range edits {
		if e.Find == "" {
			return "", fmt.Errorf("edit %d: find is empty", i+1)
		}
		switch n := strings.Count(src, e.Find); n {
		case 1:
			src = strings.Replace(src, e.Find, e.Replace, 1)
		case 0:
			return "", fmt.Errorf("edit %d: find text not in the panel's source; Read it again", i+1)
		default:
			return "", fmt.Errorf("edit %d: find text occurs %d times; make it unique", i+1, n)
		}
	}
	return src, nil
}

// Restore makes an earlier version current again, as a new version.
func (s *Store) Restore(id string, version int) (Panel, error) {
	s.mu.Lock()
	p, err := s.load(id)
	s.mu.Unlock()
	if err != nil {
		return Panel{}, err
	}
	for _, v := range p.History {
		if v.Version == version {
			return s.Upsert(id, "", "", fmt.Sprintf("restored version %d", version), v.HTML, nil)
		}
	}
	return Panel{}, fmt.Errorf("panel %q has no version %d kept", id, version)
}

// Remove deletes a panel. Removing one that is not there is not an error.
func (s *Store) Remove(id string) error {
	if err := ValidID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range []string{s.Path(id), s.meta(id)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// update loads a panel, changes it and saves it.
func (s *Store) update(id string, fn func(p *Panel) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return err
	}
	if err := fn(p); err != nil {
		return err
	}
	return s.save(p)
}

// SetHidden records the user closing or reopening a panel's tab.
func (s *Store) SetHidden(id string, hidden bool) error {
	return s.update(id, func(p *Panel) error { p.Hidden = hidden; return nil })
}

// SetError records what the UI caught from the panel; "" clears it.
func (s *Store) SetError(id, msg string) error {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	return s.update(id, func(p *Panel) error { p.Error = msg; return nil })
}

// State is the panel's persisted state, null when it has none.
func (s *Store) State(id string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.load(id)
	if err != nil {
		return nil, err
	}
	if len(p.State) == 0 {
		return json.RawMessage("null"), nil
	}
	return p.State, nil
}

// SetState replaces the panel's persisted state with v, which must be JSON.
func (s *Store) SetState(id string, v json.RawMessage) error {
	if len(v) > MaxState {
		return fmt.Errorf("panel state is %d bytes; the limit is %d", len(v), MaxState)
	}
	if !json.Valid(v) {
		return fmt.Errorf("panel state is not JSON")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		return err
	}
	return s.update(id, func(p *Panel) error { p.State = buf.Bytes(); return nil })
}
