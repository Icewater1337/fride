package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The orchestrator subcommands talk to the RUNNING server rather than starting
// an orchestrator of their own.
//
// It is the one part of this tool that cannot be a library call from the CLI.
// The orchestrator owns a background loop and a conversation, and it acts on
// the terminal sessions the served process holds; a second one started for the
// length of a command would supervise nothing, and two of them writing the same
// state file would each undo the other. So this is a thin client, and it says
// so when nothing is listening.
const defaultPort = 7777

func orchPort() int {
	if s := os.Getenv("FRIDE_PORT"); s != "" {
		if p, err := strconv.Atoi(s); err == nil {
			return p
		}
	}
	return defaultPort
}

type orchView struct {
	Mode     string   `json:"mode"`
	Autonomy string   `json:"autonomy"`
	Watching []string `json:"watching"`
	Thinking bool     `json:"thinking"`
	Awaiting bool     `json:"awaiting"`
	Error    string   `json:"error"`
	Messages []struct {
		Role string    `json:"role"`
		Text string    `json:"text"`
		Task string    `json:"task"`
		Time time.Time `json:"time"`
	} `json:"messages"`
	Tasks []struct {
		Slug   string `json:"slug"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"tasks"`
}

func orchCall(method, path string, body any) (*orchView, error) {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/orchestrator%s", orchPort(), path)
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("no fride is serving on :%d — start one with `fride serve`",
			orchPort())
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		// Something is listening but does not know this route, which in practice
		// means the served binary predates the orchestrator. Saying "404" would
		// send you looking for a typo in a path you did not write.
		return nil, fmt.Errorf("the fride serving on :%d has no orchestrator — "+
			"restart it with this build", orchPort())
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return nil, fmt.Errorf("%s", e.Error)
	}
	var v orchView
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func cmdOrch(args []string) {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "status":
		v, err := orchCall(http.MethodGet, "", nil)
		check(err)
		printOrch(v, 12)

	case "say":
		text := strings.TrimSpace(strings.Join(args[1:], " "))
		if text == "" {
			check(fmt.Errorf("fride orch say \"<what to tell it>\""))
		}
		_, err := orchCall(http.MethodPost, "/say", map[string]string{"text": text})
		check(err)
		// The answer arrives on the server's own clock, so this waits for it
		// rather than returning to a prompt with nothing said.
		fmt.Fprintln(os.Stderr, "thinking…")
		v := waitForOrch()
		printOrch(v, 4)

	case "watch":
		if len(args) < 2 {
			check(fmt.Errorf("fride orch watch <task>... [--ask]"))
		}
		autonomy, slugs := "auto", []string{}
		for _, a := range args[1:] {
			if a == "--ask" {
				autonomy = "ask"
				continue
			}
			slugs = append(slugs, a)
		}
		v, err := orchCall(http.MethodPut, "/mode", map[string]any{
			"mode": "orchestrate", "autonomy": autonomy, "watching": slugs,
		})
		check(err)
		printOrch(v, 2)

	case "off":
		v, err := orchCall(http.MethodPut, "/mode", map[string]any{
			"mode": "chat", "autonomy": "auto", "watching": []string{},
		})
		check(err)
		printOrch(v, 2)

	default:
		check(fmt.Errorf("unknown orch command %q (status, say, watch, off)", args[0]))
	}
}

// waitForOrch polls until the turn is over. A turn is one headless agent call,
// which is seconds to a minute; the deadline is there so a wedged one gives the
// terminal back rather than holding it forever.
func waitForOrch() *orchView {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		time.Sleep(2 * time.Second)
		v, err := orchCall(http.MethodGet, "", nil)
		check(err)
		if !v.Thinking || time.Now().After(deadline) {
			return v
		}
	}
}

func printOrch(v *orchView, tail int) {
	line := "mode " + v.Mode
	if v.Mode == "orchestrate" {
		line += " over " + strings.Join(v.Watching, ", ")
		if v.Autonomy == "ask" {
			line += " (asks before acting)"
		}
	}
	switch {
	case v.Thinking:
		line += " · thinking"
	case v.Awaiting:
		line += " · waiting for your answer"
	}
	fmt.Println(line)
	if v.Error != "" {
		fmt.Fprintln(os.Stderr, "last error: "+v.Error)
	}
	msgs := v.Messages
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
