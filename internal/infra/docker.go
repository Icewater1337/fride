package infra

import (
	"fmt"
	"os/exec"
	"strings"
)

// dockerOut runs a docker command and returns its stdout. Failures carry
// docker's own stderr, which is the only useful part: "port is already
// allocated" or "no such volume" say exactly what to do next, whereas
// "exit status 1" does not.
func dockerOut(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return string(out), nil
}

// Available reports whether the docker daemon is reachable. A profile with
// containers depends on it entirely, so failing here with a clear message beats
// a compose error.
func Available() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not on PATH — this profile uses containers")
	}
	if _, err := dockerOut("info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("the docker daemon is not reachable — start Docker (Desktop, colima, …), or pick a profile without containers")
	}
	return nil
}
