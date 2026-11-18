package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// The client of the `fride serve` running on this computer, shared by the
// commands that act through it: `fride orch`, `fride in`, `fride fleet` and
// `task move`. What they act on lives in that process (the orchestrator, the
// inbox's queue, the fleet's network connection and the terminals), so they
// are clients of it rather than library calls.

const defaultServePort = 7777

// servePort is the port `fride serve` listens on and its clients call.
func servePort() int {
	if s := os.Getenv("FRIDE_PORT"); s != "" {
		if p, err := strconv.Atoi(s); err == nil {
			return p
		}
	}
	return defaultServePort
}

// serverCall sends in as JSON to path and decodes the answer into out. An
// error reply is a *serverError.
func serverCall(method, path string, in, out any, timeout time.Duration) error {
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", servePort(), path), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("no fride is serving on :%d — start one with `fride serve`", servePort())
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		if e.Error == "" {
			return &serverError{status: res.StatusCode, bare: true, text: res.Status}
		}
		return &serverError{status: res.StatusCode, text: e.Error}
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// serverError is an error reply of the server. It reads as the reply's
// {"error": …}, or as its status line when the reply is bare, without one.
type serverError struct {
	status int
	bare   bool
	text   string
}

func (e *serverError) Error() string { return e.text }

// replyStatus is the status of an error reply and whether it was bare; 0 for
// an error that is not a reply.
func replyStatus(err error) (status int, bare bool) {
	var e *serverError
	if errors.As(err, &e) {
		return e.status, e.bare
	}
	return 0, false
}

// chatMessage is one message of the orchestrator's conversation or of an
// inbox thread.
type chatMessage struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	Task string    `json:"task"`
	Time time.Time `json:"time"`
}

// printMessages prints the last error, if any, and the last tail messages
// (all of them when tail is 0).
func printMessages(errText string, msgs []chatMessage, tail int) {
	if errText != "" {
		fmt.Fprintln(os.Stderr, "last error: "+errText)
	}
	if tail > 0 && len(msgs) > tail {
		msgs = msgs[len(msgs)-tail:]
	}
	for _, m := range msgs {
		who := m.Role
		if m.Task != "" {
			who += " " + m.Task
		}
		fmt.Printf("\n%s %s\n%s\n", m.Time.Format("15:04"), who, m.Text)
	}
}
