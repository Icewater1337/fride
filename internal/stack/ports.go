package stack

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Icewater1337/fride/internal/infra"
)

// killPort frees a port for real. Signalling the recorded launcher is not
// enough: build tools fork the real server from a daemon, and dev servers fork
// children of their own, so neither is in the launcher's process group — and a
// down/up cycle would silently keep serving the previous build.
func killPort(port int) error {
	pids := infra.Holders(port)
	if len(pids) == 0 {
		return nil
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	for i := 0; i < 30; i++ {
		if !infra.Held(port) {
			return nil
		}
		time.Sleep(time.Second)
	}
	for _, pid := range infra.Holders(port) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(time.Second)
	if pids := infra.Holders(port); len(pids) > 0 {
		return fmt.Errorf(":%d is still held by %v", port, pids)
	}
	return nil
}

// setKV replaces-or-appends a key in a KEY=VALUE file (dotenv, .properties).
// It never logs the value: these files hold secrets. A missing file is created.
func setKV(path, key, value string) error {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(b), "\n")
	if len(b) == 0 {
		lines = nil
	}
	found := false
	for i, l := range lines {
		if keyOf(l) == key {
			lines[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+value, "")
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// keyOf is the key of a KEY=VALUE line, tolerating `export KEY=` and spaces
// around the equals sign; "" for comments and blank lines.
func keyOf(line string) string {
	l := strings.TrimSpace(line)
	if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "!") {
		return ""
	}
	l = strings.TrimPrefix(l, "export ")
	i := strings.IndexAny(l, "=:")
	if i <= 0 {
		return ""
	}
	return strings.TrimSpace(l[:i])
}

func delKey(path, key string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if keyOf(l) == key {
			continue
		}
		out = append(out, l)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o600)
}

func getKV(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if keyOf(sc.Text()) == key {
			_, v, _ := strings.Cut(sc.Text(), "=")
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// bootLogPrefix is how much of a service log the status read looks at.
//
// Everything it wants is written while the app boots, and the log is truncated
// on every start, so the boot is always at the beginning of the file. A service
// stuck in a reconnect loop can write gigabytes, and reading — and regex-
// scanning — all of it on every status poll was once most of what the machine
// was doing.
const bootLogPrefix = 8 << 20

// readBootPrefix reads the head of a log, never more than the file holds: it
// runs for every source service on every status poll, and a fixed 8 MiB
// buffer per call was most of what a board refresh allocated.
func readBootPrefix(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b := make([]byte, min(fi.Size(), bootLogPrefix))
	n, err := io.ReadFull(f, b)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return b[:n], nil
}

// Tail returns the last n lines of a file.
func Tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	const window = 64 << 10
	off := fi.Size() - window
	if off < 0 {
		off = 0
	}
	b := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
