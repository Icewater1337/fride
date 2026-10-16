package fleet

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Loopback is a fleet of FrIDE processes on ONE machine: nodes listen on
// 127.0.0.1 and find each other through files in a shared directory. Every
// caller is the same user, which is honest here — only local processes can
// reach 127.0.0.1, and those can already drive any node's local API.
//
// It exists so that everything above the network — the fleet board, remote
// terminals, moving a task — can be run and tested without a tailnet. It is
// selected with FRIDE_FLEET_LOOPBACK=<dir>.
type Loopback struct {
	dir, name string
	ln        net.Listener
}

const loopbackUser = "loopback"

func JoinLoopback(dir, name string) (*Loopback, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Loopback{dir: dir, name: CleanName(name)}, nil
}

type loopbackEntry struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

func (l *Loopback) Listen(int) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(loopbackEntry{Addr: ln.Addr().String(), PID: os.Getpid()})
	if err := os.WriteFile(filepath.Join(l.dir, l.name+".json"), b, 0o644); err != nil {
		ln.Close()
		return nil, err
	}
	l.ln = ln
	return ln, nil
}

func (l *Loopback) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func (l *Loopback) WhoIs(_ context.Context, remoteAddr string) (Identity, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return Identity{}, nil // not ours: an empty identity is refused by Guard
	}
	return Identity{UserID: loopbackUser, LoginName: "this machine"}, nil
}

func (l *Loopback) Peers(context.Context) ([]Peer, error) {
	files, _ := filepath.Glob(filepath.Join(l.dir, "*.json"))
	var out []Peer
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if name == l.name {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e loopbackEntry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		c, err := net.DialTimeout("tcp", e.Addr, 300*time.Millisecond)
		online := err == nil
		if c != nil {
			c.Close()
		}
		out = append(out, Peer{Name: name, Addr: e.Addr, Online: online, OS: "loopback"})
	}
	return out, nil
}

func (l *Loopback) State() State {
	return State{Phase: "running", Kind: "loopback",
		Self: Identity{UserID: loopbackUser, LoginName: "this machine", Node: l.name}}
}

func (l *Loopback) Close() error {
	_ = os.Remove(filepath.Join(l.dir, l.name+".json"))
	if l.ln != nil {
		return l.ln.Close()
	}
	return nil
}
