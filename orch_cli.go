package main

import (
	"fmt"
	"net/http"
	"os"
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

type orchView struct {
	Mode     string        `json:"mode"`
	Autonomy string        `json:"autonomy"`
	Watching []string      `json:"watching"`
	Thinking bool          `json:"thinking"`
	Awaiting bool          `json:"awaiting"`
	Error    string        `json:"error"`
	Messages []chatMessage `json:"messages"`
	Tasks    []struct {
		Slug   string `json:"slug"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"tasks"`
}

func orchCall(method, path string, body any) (*orchView, error) {
	var v orchView
	err := serverCall(method, "/api/orchestrator"+path, body, &v, 30*time.Second)
	if status, _ := replyStatus(err); status == http.StatusNotFound {
		// Something is listening but does not know this route, which in practice
		// means the served binary predates the orchestrator. Saying "404" would
		// send you looking for a typo in a path you did not write.
		return nil, fmt.Errorf("the fride serving on :%d has no orchestrator — "+
			"restart it with this build", servePort())
	}
	if err != nil {
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
	printMessages(v.Error, v.Messages, tail)
}
