package settings

import (
	"fmt"
	"regexp"
	"strings"
)

// Templates are how a config refers to things that only exist per task: the
// task's ports, its worktrees, other services. The syntax is deliberately tiny
// — {{name}} and {{namespace.name}}, nothing else — because the files are
// written by an agent and read by a human, and both should be able to predict
// the result without running it.
//
//	{{port}}            this service's port in this task
//	{{slot}}            the task's slot number (1..maxSlots)
//	{{task}}            the task slug
//	{{workspace}}       the task's workspace folder
//	{{dir}}             this service's working directory
//	{{home}}            the user's home directory
//	{{ports.<svc>}}     another service's port, as THIS task sees it
//	{{url.<svc>}}       another service's URL
//	{{repo.<name>}}     a repository's worktree in this task
//	{{main.<name>}}     a repository's main checkout
//	{{env.<NAME>}}      an environment variable of the FrIDE process
var tmplRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)

// Unavailable is returned when a template refers to something this task does
// not have: a service that is absent, a repo that is not checked out, an unset
// variable. Callers that write configuration treat it as "remove this key".
type Unavailable struct{ Var string }

func (u *Unavailable) Error() string { return "{{" + u.Var + "}} is not available in this task" }

// Lookup resolves one variable. ok=false means unavailable; an error means the
// name itself is wrong (unknown namespace) and is a config bug.
type Lookup func(name string) (value string, ok bool, err error)

// Expand renders a template.
func Expand(s string, look Lookup) (string, error) {
	var firstErr error
	out := tmplRe.ReplaceAllStringFunc(s, func(m string) string {
		if firstErr != nil {
			return m
		}
		name := tmplRe.FindStringSubmatch(m)[1]
		v, ok, err := look(name)
		if err != nil {
			firstErr = err
			return m
		}
		if !ok {
			firstErr = &Unavailable{Var: name}
			return m
		}
		return v
	})
	return out, firstErr
}

// Vars lists the variables a template uses.
func Vars(s string) []string {
	var out []string
	for _, m := range tmplRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// plain are the variables without a namespace.
var plain = map[string]bool{"port": true, "slot": true, "task": true, "workspace": true, "dir": true, "home": true}

// namespaces are the dotted prefixes.
var namespaces = map[string]bool{"ports": true, "url": true, "repo": true, "main": true, "env": true}

// SplitVar separates "ports.api" into ("ports", "api"); a plain variable has an
// empty namespace. Unknown names are an error.
func SplitVar(name string) (ns, key string, err error) {
	if plain[name] {
		return "", name, nil
	}
	ns, key, found := strings.Cut(name, ".")
	if !found || !namespaces[ns] || key == "" {
		return "", "", fmt.Errorf("unknown template variable {{%s}}", name)
	}
	return ns, key, nil
}

// CompileFact compiles a facts regex. The last match in the log is the fact.
func CompileFact(re string) (*regexp.Regexp, error) { return compileFact(re) }

func compileFact(re string) (*regexp.Regexp, error) {
	if strings.TrimSpace(re) == "" {
		return nil, fmt.Errorf("empty regex")
	}
	return regexp.Compile(re)
}
