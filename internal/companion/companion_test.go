package companion

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPairingCodeIsSingleUse(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	code, _, err := NewCode(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalizeCode(code)) != 16 {
		t.Fatalf("code %q", code)
	}
	// Retyped in lower case and without dashes still works — once.
	token, err := Redeem(dir, strings.ToLower(strings.ReplaceAll(code, "-", "")), "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(dir, token) {
		t.Fatal("the issued token does not verify")
	}
	if _, err := Redeem(dir, code, "", now); !errors.Is(err, ErrNoCode) {
		t.Fatalf("second redeem = %v, want ErrNoCode", err)
	}
	// Concurrent redeems of one code: exactly one wins.
	code, _, _ = NewCode(dir, now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Redeem(dir, code, "", now); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent redeems succeeded, want 1", wins)
	}
	// The newer pairing replaced the first token.
	if Verify(dir, token) {
		t.Fatal("pairing again should replace the old token")
	}
}

func TestPairingCodeExpiresAndBurns(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	code, _, _ := NewCode(dir, now)
	if _, err := Redeem(dir, code, "", now.Add(CodeTTL+time.Second)); !errors.Is(err, ErrNoCode) {
		t.Fatalf("expired code = %v", err)
	}
	code, _, _ = NewCode(dir, now)
	for i := 0; i < maxTries; i++ {
		if _, err := Redeem(dir, "AAAA-AAAA-AAAA-AAAA", "", now); !errors.Is(err, ErrBadCode) {
			t.Fatalf("wrong code #%d = %v", i, err)
		}
	}
	// Burnt after too many wrong guesses: the right one no longer works.
	if _, err := Redeem(dir, code, "", now); !errors.Is(err, ErrNoCode) {
		t.Fatalf("after %d wrong guesses = %v, want ErrNoCode", maxTries, err)
	}
}

func TestTokenIsStoredHashedOnly(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := NewCode(dir, time.Now())
	token, err := Redeem(dir, code, "http://127.0.0.1:9999/hook", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), token) || strings.Contains(string(b), code) {
			t.Errorf("%s holds the token or the code in clear", p)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", p, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := Current(dir)
	sum := sha256.Sum256([]byte(token))
	if p == nil || p.Hash != hex.EncodeToString(sum[:]) || p.Webhook != "http://127.0.0.1:9999/hook" {
		t.Fatalf("pairing = %+v", p)
	}
	if string(p.Key()) != string(KeyForToken(token)) {
		t.Fatal("the stored key and the companion's derived key differ")
	}
	for _, bad := range []string{"", token + "x", "frc_" + strings.Repeat("A", 43), "Bearer " + token} {
		if Verify(dir, bad) {
			t.Errorf("%q verified", bad)
		}
	}
	if Bearer("Bearer "+token) != token || Bearer("bearer "+token) != token || Bearer(token) != "" {
		t.Error("Bearer parsing")
	}
}

func TestPairingRefusesARemoteWebhook(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := NewCode(dir, time.Now())
	for _, u := range []string{"http://example.com/hook", "http://10.0.0.2:80/", "ftp://127.0.0.1/", "http://user:pw@127.0.0.1/"} {
		if _, err := Redeem(dir, code, u, time.Now()); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	// The refusals did not use up the code.
	if _, err := Redeem(dir, code, "http://[::1]:8080/x", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookSignsAndRetries(t *testing.T) {
	key := KeyForToken("frc_test")
	var mu sync.Mutex
	var got []Event
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, key)
		m.Write(body)
		if r.Header.Get("X-Fride-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			t.Errorf("bad signature %q", r.Header.Get("X-Fride-Signature"))
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			w.WriteHeader(503) // the companion is restarting
			return
		}
		var e Event
		if err := json.Unmarshal(body, &e); err != nil {
			t.Error(err)
		}
		if r.Header.Get("X-Fride-Event") != e.Event {
			t.Errorf("X-Fride-Event = %q", r.Header.Get("X-Fride-Event"))
		}
		got = append(got, e)
	}))
	defer srv.Close()

	s := NewSender(func() string { return srv.URL + "/hook" }, func() []byte { return key })
	s.Backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	s.Start()
	defer s.Close()
	if !s.Send(Event{Event: EventQuestion, ThreadID: 7, TaskID: "proj-42", Text: "which one?", Status: "needs_reply"}) {
		t.Fatal("Send refused")
	}
	s.Send(Event{Event: EventDone, ThreadID: 7, TaskID: "proj-42", Branch: "feat/proj-42", RationalePath: "/x.md"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Event != EventQuestion || got[1].Event != EventDone || got[1].Branch != "feat/proj-42" {
		t.Fatalf("delivered %+v after %d calls", got, calls)
	}
	if got[0].TS.IsZero() || got[0].ThreadID != 7 || got[0].TaskID != "proj-42" {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestWebhookDoesNotRetryARefusal(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(400)
	}))
	defer srv.Close()
	s := NewSender(func() string { return srv.URL }, func() []byte { return []byte("k") })
	if retry, err := s.Deliver(Event{Event: EventDone}); err == nil || retry {
		t.Fatalf("400: retry=%v err=%v", retry, err)
	}
	// Not loopback, or not paired: never sent at all.
	s.URL = func() string { return "http://example.com/hook" }
	if retry, err := s.Deliver(Event{}); err == nil || retry {
		t.Fatalf("remote URL: retry=%v err=%v", retry, err)
	}
	s.URL, s.Key = func() string { return srv.URL }, func() []byte { return nil }
	if _, err := s.Deliver(Event{}); err == nil {
		t.Fatal("delivered without a pairing")
	}
	if calls != 1 {
		t.Fatalf("%d calls, want 1", calls)
	}
}

// A companion that never answers must not hold up whoever sends news.
func TestWebhookSendNeverBlocks(t *testing.T) {
	s := NewSender(func() string { return "http://127.0.0.1:1/" }, func() []byte { return []byte("k") })
	// Not started: nothing drains the queue.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.Send(Event{Event: EventUpdate, ThreadID: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on a full queue")
	}
}

func TestEventFor(t *testing.T) {
	for _, c := range []struct {
		status  string
		started bool
		want    string
	}{
		{"active", false, EventStarted}, {"active", true, EventUpdate},
		{"needs_reply", false, EventQuestion}, {"needs_reply", true, EventQuestion},
		{"done", true, EventDone}, {"answered", false, EventDone},
		{"failed", false, EventFailed}, {"queued", true, EventUpdate}, {"queued", false, EventUpdate},
	} {
		if got := EventFor(c.status, c.started); got != c.want {
			t.Errorf("EventFor(%s, %v) = %s, want %s", c.status, c.started, got, c.want)
		}
	}
}

func TestWebhookDownCompanionGetsOneAttemptPerEvent(t *testing.T) {
	var calls atomic.Int32
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !up.Load() {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	s := NewSender(func() string { return srv.URL + "/hook" }, func() []byte { return []byte("k") })
	s.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	s.Start()
	defer s.Close()
	wait := func(n int32) {
		for deadline := time.Now().Add(5 * time.Second); calls.Load() < n && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond) // any extra attempt would show now
		if got := calls.Load(); got != n {
			t.Fatalf("calls = %d, want %d", got, n)
		}
	}
	for i := 0; i < 3; i++ {
		s.Send(Event{Event: EventUpdate, ThreadID: i})
	}
	wait(3 + 1 + 1) // first event: 1 try + 2 retries; the others one try each
	up.Store(true)
	s.Send(Event{Event: EventUpdate})
	wait(6)
	up.Store(false)
	s.Send(Event{Event: EventUpdate})
	wait(9) // down was cleared by the success, so retries are back
}
