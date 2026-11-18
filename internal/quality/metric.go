package quality

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// The metric is per cohort: a change counts in the week or month it merged,
// and is marked there once it is reverted, followed up or causes an
// incident, however much later. So "of what merged in March, how much went
// wrong" keeps its answer as it ages, and the latest periods are still
// filling in: a change merged yesterday has not had time to be reverted.

// Row is one period of one repository.
type Row struct {
	Period     string `json:"period"`
	Repo       string `json:"repo"`
	Merged     int    `json:"merged"`
	Reverted   int    `json:"reverted"`
	FollowedUp int    `json:"followedUp"`
	Incident   int    `json:"incident"`
	// Bad counts the changes with any of the three; a change both reverted
	// and followed up is one bad change.
	Bad int `json:"bad"`
}

// Query picks what the metric covers.
type Query struct {
	Repo  string
	Since time.Time
	By    string // week or month (the default)
}

// Report is the metric and the changes behind it.
type Report struct {
	By    string    `json:"by"`
	Repo  string    `json:"repo,omitempty"`
	Since time.Time `json:"since,omitzero"`
	Rows  []Row     `json:"rows"`
	Total Row       `json:"total"`
	// Pending counts recorded changes not seen merged (yet).
	Pending   int       `json:"pending"`
	Refreshed time.Time `json:"refreshed,omitzero"`
	// Changes are the merged changes in range, newest merge first.
	Changes []*Change `json:"changes"`
	Notes   []string  `json:"notes,omitempty"`
}

// Summarize reads the record and computes the metric.
func Summarize(cfg *config.Config, q Query) (*Report, error) {
	changes, refreshed, err := Load(cfg)
	if err != nil {
		return nil, err
	}
	r := metric(changes, q)
	r.Refreshed = refreshed
	return &r, nil
}

func period(t time.Time, by string) string {
	if by == "week" {
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	}
	return t.Format("2006-01")
}

func metric(changes []*Change, q Query) Report {
	if q.By != "week" {
		q.By = "month"
	}
	r := Report{By: q.By, Repo: q.Repo, Since: q.Since, Rows: []Row{}, Changes: []*Change{}, Total: Row{Period: "total"}}
	rows := map[[2]string]*Row{}
	for _, c := range changes {
		if q.Repo != "" && c.Repo != q.Repo {
			continue
		}
		if c.Merged == nil {
			r.Pending++
			continue
		}
		at := c.Merged.At.Local()
		if !q.Since.IsZero() && at.Before(q.Since) {
			continue
		}
		k := [2]string{period(at, q.By), c.Repo}
		row := rows[k]
		if row == nil {
			row = &Row{Period: k[0], Repo: k[1]}
			rows[k] = row
		}
		for _, x := range []*Row{row, &r.Total} {
			x.Merged++
			if len(c.Reverted) > 0 {
				x.Reverted++
			}
			if len(c.FollowUps) > 0 {
				x.FollowedUp++
			}
			if len(c.Incidents) > 0 {
				x.Incident++
			}
			if c.Bad() {
				x.Bad++
			}
		}
		r.Changes = append(r.Changes, c)
	}
	for _, row := range rows {
		r.Rows = append(r.Rows, *row)
	}
	sort.Slice(r.Rows, func(i, j int) bool {
		if r.Rows[i].Period != r.Rows[j].Period {
			return r.Rows[i].Period > r.Rows[j].Period
		}
		return r.Rows[i].Repo < r.Rows[j].Repo
	})
	sort.SliceStable(r.Changes, func(i, j int) bool { return r.Changes[i].Merged.At.After(r.Changes[j].Merged.At) })
	return r
}

// Rate is bad over merged, as a percentage; "–" with nothing merged.
func (r Row) Rate() string {
	if r.Merged == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(r.Bad)/float64(r.Merged))
}

// ParseSince reads 2026-01-31, or a span back from now: 90d, 12w, 6m, 1y.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n < 0 {
		return time.Time{}, fmt.Errorf("--since: want a date (2026-01-31) or a span (90d, 12w, 6m, 1y), not %q", s)
	}
	switch s[len(s)-1] {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -7*n), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	case 'y':
		return now.AddDate(-n, 0, 0), nil
	}
	return time.Time{}, fmt.Errorf("--since: want a date (2026-01-31) or a span (90d, 12w, 6m, 1y), not %q", s)
}
