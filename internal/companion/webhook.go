package companion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Icewater1337/fride/internal/settings"
)

// Events a webhook carries.
const (
	// EventStarted: the message was read and handed to a task; work is
	// under way (sent once per thread).
	EventStarted = "started"
	// EventUpdate: any other news — an instruction reaching its agent, what
	// the agent reported, a failed attempt that will be retried.
	EventUpdate = "update"
	// EventQuestion: the thread waits for an answer from its sender; reply
	// with POST /api/companion/threads/<id>/reply.
	EventQuestion = "question"
	// EventDone: the thread is finished — answered, or every task it went to
	// was closed.
	EventDone = "done"
	// EventFailed: the message could not be read, even after retrying.
	EventFailed = "failed"
)

// Event is one webhook delivery. Its JSON is the contract with the companion;
// see docs/COMPANION.md.
type Event struct {
	Event    string `json:"event"`
	ThreadID int    `json:"thread_id"`
	// TaskID is the task the news is about; "" when the thread has none.
	TaskID string `json:"task_id"`
	// Text is a summary ready to show a person.
	Text string `json:"text"`
	// Status is the thread's status in FrIDE's own words (queued, active,
	// needs_reply, answered, done, closed, failed).
	Status        string    `json:"status"`
	Branch        string    `json:"branch,omitempty"`
	RationalePath string    `json:"rationale_path,omitempty"`
	TS            time.Time `json:"ts"`
}

// Sender delivers events to the companion's webhook, one at a time and in
// order, retrying with backoff. Send never blocks: the orchestrator hands news
// over and carries on, whatever the companion is doing.
type Sender struct {
	// URL is read at each delivery, so a reloaded config.json applies.
	URL func() string
	// Key is the signing key; nil means not paired, and the event is dropped.
	Key func() []byte
	// Backoff is the wait before each retry; its length is the number of
	// retries.
	Backoff []time.Duration
	Client  *http.Client

	queue    chan Event
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	// down is set when an event used up its retries; while set, events get a
	// single attempt. Only the loop goroutine touches it.
	down bool
}

// DefaultBackoff retries for about two minutes in all: long enough for a
// companion that is restarting, short enough that news is not held behind a
// companion that is gone: once an event has used these up, later events get a
// single attempt each until one is delivered.
var DefaultBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second}

// NewSender makes a sender; Start runs it.
func NewSender(url func() string, key func() []byte) *Sender {
	return &Sender{
		URL: url, Key: key, Backoff: DefaultBackoff,
		Client: &http.Client{
			Timeout: 10 * time.Second,
			// Never through a proxy, and never followed elsewhere: the URL was
			// checked to be loopback, and a redirect would undo that check.
			Transport:     &http.Transport{Proxy: nil, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue: make(chan Event, 256),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Start runs the delivery loop until Close.
func (s *Sender) Start() { go s.loop() }

// Close stops the loop; events still queued are dropped.
func (s *Sender) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

// Send queues an event. It reports false when the queue is full and the event
// was dropped.
func (s *Sender) Send(e Event) bool {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	select {
	case s.queue <- e:
		return true
	default:
		log.Printf("companion: webhook queue full, dropped %s for thread #%d", e.Event, e.ThreadID)
		return false
	}
}

func (s *Sender) loop() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case e := <-s.queue:
			s.deliverWithRetry(e)
		}
	}
}

func (s *Sender) deliverWithRetry(e Event) {
	for attempt := 0; ; attempt++ {
		retry, err := s.Deliver(e)
		if err == nil {
			s.down = false
			return
		}
		if !retry || attempt >= len(s.Backoff) || s.down {
			s.down = s.down || retry
			log.Printf("companion: webhook %s for thread #%d not delivered: %v", e.Event, e.ThreadID, err)
			return
		}
		select {
		case <-s.stop:
			return
		case <-time.After(s.Backoff[attempt]):
		}
	}
}

// Deliver makes one attempt. retry says whether another attempt could go
// better: a network error or a 5xx, 408 or 429 could; a refusal (any other
// 4xx), a URL that is not loopback, or no pairing cannot.
func (s *Sender) Deliver(e Event) (retry bool, err error) {
	url := ""
	if s.URL != nil {
		url = s.URL()
	}
	if url == "" {
		return false, fmt.Errorf("no webhook URL")
	}
	if err := settings.LoopbackURL(url); err != nil {
		return false, fmt.Errorf("webhook URL %s %w", url, err)
	}
	var key []byte
	if s.Key != nil {
		key = s.Key()
	}
	if len(key) == 0 {
		return false, fmt.Errorf("no companion is paired")
	}
	body, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fride-companion-webhook")
	req.Header.Set("X-Fride-Event", e.Event)
	req.Header.Set("X-Fride-Signature", Sign(key, body))
	resp, err := s.Client.Do(req)
	if err != nil {
		return true, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusTooManyRequests:
		return true, fmt.Errorf("%s", resp.Status)
	default:
		return false, fmt.Errorf("refused: %s", resp.Status)
	}
}

// EventFor maps a thread's status, as a note about it arrives, to the event
// the companion gets. started says whether the thread has had its started
// event already.
func EventFor(status string, started bool) string {
	switch status {
	case "needs_reply":
		return EventQuestion
	case "failed":
		return EventFailed
	case "done", "answered":
		return EventDone
	case "active":
		if !started {
			return EventStarted
		}
	}
	return EventUpdate
}
