package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// `fride in` is the command-line end of the inbox: how a script, a chat bridge
// or an assistant hands this computer's orchestrator a message, and reads what
// came of it. Like `fride orch`, it is a client of the running server, which
// owns the queue and the orchestrator that reads it.

type inThread struct {
	ID       int       `json:"id"`
	Source   string    `json:"source"`
	Ref      string    `json:"ref"`
	Text     string    `json:"text"`
	Status   string    `json:"status"`
	Tasks    []string  `json:"tasks"`
	Finished []string  `json:"finished"`
	Error    string    `json:"error"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Position int       `json:"position"`
	Messages []struct {
		Role string    `json:"role"`
		Text string    `json:"text"`
		Task string    `json:"task"`
		Time time.Time `json:"time"`
	} `json:"messages"`
}

func inCall(method, path string, body any) (*inThread, error) {
	var v inThread
	if err := inboxCall(method, path, body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// inboxCall is serverCall with one case made legible: a bare 404 means the
// served binary has no inbox at all — an unknown thread carries an error of its
// own — and saying "404" would send you looking for a typo you did not make.
func inboxCall(method, path string, body, out any) error {
	err := serverCall(method, "/api/inbox"+path, body, out, 30*time.Second)
	if err != nil && err.Error() == "404 Not Found" {
		return fmt.Errorf("the fride serving on :%d has no inbox — restart it with this build", orchPort())
	}
	return err
}

func cmdIn(args []string) {
	flags, words := inFlags(args)
	if len(words) > 0 {
		switch words[0] {
		case "ls":
			var all []inThread
			check(inboxCall(http.MethodGet, "", nil, &all))
			if flags.json {
				printJSON(all)
				return
			}
			if len(all) == 0 {
				fmt.Println("the inbox is empty — send something with: fride in \"<message>\"")
				return
			}
			for _, t := range all {
				fmt.Printf("#%-4d %-12s %-10s %-24s %s\n", t.ID, t.Status, clipLine(t.Source, 10),
					clipLine(strings.Join(t.Tasks, ","), 24), clipLine(t.Text, 60))
			}
			return
		case "status":
			v, err := inCall(http.MethodGet, "/"+inID(words, "status"), nil)
			check(err)
			printThread(v, flags.json, 0)
			return
		case "reply":
			id := inID(words, "reply <id> \"<message>\"")
			text := inText(words[2:])
			v, err := inCall(http.MethodPost, "/"+id+"/reply", map[string]string{"text": text})
			check(err)
			if flags.wait {
				v = waitForThread(id)
			}
			printThread(v, flags.json, 4)
			return
		case "close":
			v, err := inCall(http.MethodPost, "/"+inID(words, "close")+"/close", nil)
			check(err)
			printThread(v, flags.json, 1)
			return
		}
	}

	text := inText(words)
	v, err := inCall(http.MethodPost, "", map[string]string{
		"text": text, "source": flags.source, "ref": flags.ref,
	})
	check(err)
	if flags.wait {
		v = waitForThread(strconv.Itoa(v.ID))
	}
	printThread(v, flags.json, 4)
}

type inOpts struct {
	source, ref string
	wait, json  bool
}

// inFlags takes the flags out of args, wherever they are, and returns the rest
// as the words of the command or the message.
func inFlags(args []string) (inOpts, []string) {
	o := inOpts{source: "cli"}
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() string {
			if i+1 >= len(args) {
				check(fmt.Errorf("%s needs a value", a))
			}
			i++
			return args[i]
		}
		switch a {
		case "--from", "--source":
			o.source = value()
		case "--ref":
			o.ref = value()
		case "--wait":
			o.wait = true
		case "--json":
			o.json = true
		default:
			words = append(words, a)
		}
	}
	return o, words
}

// inText is the message: the words given, or standard input when there are
// none or the only one is "-". Standard input is what makes a long message —
// a transcript excerpt — safe to send from a script without quoting it.
func inText(words []string) string {
	if len(words) == 0 || (len(words) == 1 && words[0] == "-") {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && len(words) == 0 {
			check(fmt.Errorf(`fride in "<message>" [--from <source>] [--ref <id>] [--wait] [--json]` +
				"\n       (or pipe the message in on standard input)"))
		}
		b, err := io.ReadAll(os.Stdin)
		check(err)
		return strings.TrimSpace(string(b))
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

func inID(words []string, usage string) string {
	if len(words) < 2 {
		check(fmt.Errorf("fride in %s", usage))
	}
	id := strings.TrimPrefix(words[1], "#")
	if n, err := strconv.Atoi(id); err != nil || n <= 0 {
		check(fmt.Errorf("%q is not a thread number", words[1]))
	}
	return id
}

// waitForThread polls until the thread's message has been read. A turn is one
// headless agent call, seconds to a minute; the deadline gives the terminal
// back from a wedged one.
func waitForThread(id string) *inThread {
	deadline := time.Now().Add(5 * time.Minute)
	fmt.Fprintln(os.Stderr, "waiting for the orchestrator…")
	for {
		time.Sleep(2 * time.Second)
		v, err := inCall(http.MethodGet, "/"+id, nil)
		check(err)
		pending := v.Status == "queued" || v.Status == "routing"
		if !pending || time.Now().After(deadline) {
			return v
		}
	}
}

func printThread(v *inThread, asJSON bool, tail int) {
	if asJSON {
		printJSON(v)
		return
	}
	line := fmt.Sprintf("inbox #%d · %s · from %s", v.ID, strings.ReplaceAll(v.Status, "_", " "), v.Source)
	if v.Position > 0 {
		line += fmt.Sprintf(" · %d in the queue", v.Position)
	}
	if len(v.Tasks) > 0 {
		line += " · tasks: " + strings.Join(v.Tasks, ", ")
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

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	check(err)
	fmt.Println(string(b))
}

func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}
