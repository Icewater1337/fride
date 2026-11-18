package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/quality"
	"github.com/Icewater1337/fride/internal/settings"
)

// The quality record (internal/quality): what became of the tasks' changes
// after they merged, and the eval replay of the AI review.
//
//	GET  /api/quality?repo=&since=&by=   the metric, the changes behind it, the eval trend
//	POST /api/quality/refresh            fetch the bases and look again (sync)
//	POST /api/quality/incidents          {target, repo?, path?, why}: record an incident
//	POST /api/quality/evals              {max?}: replay the eval set (202, background)

// qualityJobs says what runs, for the view and to refuse a second click.
// The files themselves are locked across processes by the quality package.
var qualityJobs struct {
	sync.Mutex
	refreshing, evaluating bool
}

func (s *Server) handleQuality(w http.ResponseWriter, r *http.Request) {
	switch rest := strings.TrimPrefix(r.URL.Path, "/api/quality"); {
	case rest == "" && r.Method == http.MethodGet:
		s.qualityView(w, r)
	case rest == "/refresh" && r.Method == http.MethodPost:
		res, err := s.refreshQuality()
		if errors.Is(err, errBusy) {
			writeErr(w, 409, err)
			return
		}
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, res)
	case rest == "/incidents" && r.Method == http.MethodPost:
		var in struct{ Target, Repo, Path, Why string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeErr(w, 400, err)
			return
		}
		c, err := quality.RecordIncident(s.cfg, in.Target, in.Repo, in.Path, in.Why)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 201, c)
	case rest == "/evals" && r.Method == http.MethodPost:
		var in struct{ Max int }
		_ = json.NewDecoder(r.Body).Decode(&in) // an empty body is the configured cap
		if in.Max <= 0 {
			in.Max = s.cfg.Settings.QualitySchedule().EvalMax
		}
		if err := s.startEvals(in.Max); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 202, map[string]any{"started": true, "max": in.Max})
	default:
		writeErr(w, 405, fmt.Errorf("method not allowed"))
	}
}

// runView is one eval run, without its per-case detail.
type runView struct {
	Started   time.Time `json:"started"`
	Cases     int       `json:"cases"`
	Caught    int       `json:"caught"`
	Missed    int       `json:"missed"`
	Errors    int       `json:"errors"`
	Left      int       `json:"left"`
	Recall    float64   `json:"recall"`    // -1: no outcome case ran
	Precision float64   `json:"precision"` // -1: no voted finding came up
}

func (s *Server) qualityView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := quality.ParseSince(q.Get("since"), time.Now())
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	rep, err := quality.Summarize(s.cfg, quality.Query{Repo: q.Get("repo"), Since: since, By: q.Get("by")})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	cases, _ := quality.Cases(s.cfg)
	byKind := map[string]int{}
	for _, c := range cases {
		byKind[c.Kind]++
	}
	runs, _ := quality.Runs(s.cfg)
	views := []runView{}
	for i := len(runs) - 1; i >= 0 && len(views) < 20; i-- {
		r := runs[i]
		views = append(views, runView{Started: r.Started, Cases: len(r.Cases), Caught: r.Caught, Missed: r.Missed,
			Errors: r.Errors, Left: r.Left, Recall: r.Recall(), Precision: r.Precision()})
	}
	qualityJobs.Lock()
	refreshing, evaluating := qualityJobs.refreshing, qualityJobs.evaluating
	qualityJobs.Unlock()
	writeJSON(w, 200, struct {
		*quality.Report
		Cases      map[string]int   `json:"cases"`
		Runs       []runView        `json:"runs"`
		Schedule   settings.Quality `json:"schedule"`
		Refreshing bool             `json:"refreshing"`
		Evaluating bool             `json:"evaluating"`
	}{rep, byKind, views, s.cfg.Settings.QualitySchedule(), refreshing, evaluating})
}

var errBusy = errors.New("already running")

func (s *Server) refreshQuality() (*quality.Result, error) {
	qualityJobs.Lock()
	if qualityJobs.refreshing {
		qualityJobs.Unlock()
		return nil, fmt.Errorf("a refresh is %w", errBusy)
	}
	qualityJobs.refreshing = true
	qualityJobs.Unlock()
	defer func() {
		qualityJobs.Lock()
		qualityJobs.refreshing = false
		qualityJobs.Unlock()
	}()
	return quality.Refresh(s.cfg, s.st, quality.Options{Fetch: true,
		FollowUpDays: s.cfg.Settings.QualitySchedule().FollowUpDays})
}

// startEvals replays the eval set in the background: a run takes minutes.
func (s *Server) startEvals(max int) error {
	qualityJobs.Lock()
	defer qualityJobs.Unlock()
	if qualityJobs.evaluating {
		return quality.ErrEvalRunning
	}
	qualityJobs.evaluating = true
	go func() {
		defer func() {
			qualityJobs.Lock()
			qualityJobs.evaluating = false
			qualityJobs.Unlock()
		}()
		run, err := quality.RunEvals(context.Background(), s.cfg, s.st,
			quality.EvalOptions{Bin: s.cfg.ClaudeBin, Max: max})
		if err != nil {
			log.Printf("quality: eval replay: %v", err)
			return
		}
		log.Printf("quality: eval replay caught %d of %d, %d error(s)", run.Caught, run.Caught+run.Missed, run.Errors)
	}()
	return nil
}

// qualityTick is the schedule config.json's quality section sets: refresh
// and replay when their interval has passed since the last one, wherever
// that ran (the CLI counts too).
func (s *Server) qualityTick(now time.Time) {
	if !s.cfg.Configured() {
		return
	}
	q := s.cfg.Settings.QualitySchedule()
	if q.RefreshHours > 0 {
		if _, last, err := quality.Load(s.cfg); err == nil && now.Sub(last) >= time.Duration(q.RefreshHours)*time.Hour {
			if res, err := s.refreshQuality(); err != nil && !errors.Is(err, errBusy) {
				log.Printf("quality: refresh: %v", err)
			} else if res != nil {
				for _, n := range res.Notes {
					log.Printf("quality: %s", n)
				}
			}
		}
	}
	if q.EvalHours > 0 && now.Sub(quality.LastRun(s.cfg)) >= time.Duration(q.EvalHours)*time.Hour {
		_ = s.startEvals(q.EvalMax)
	}
}

// qualityLoop runs the schedule. It looks every ten minutes, which is what
// an interval in hours needs; a tick with nothing due reads two small files.
func (s *Server) qualityLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for now := range t.C {
		s.qualityTick(now)
	}
}
