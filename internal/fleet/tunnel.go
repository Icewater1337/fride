package fleet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Tunnels make a stack running on one computer usable from another.
//
// The viewing node listens on the SAME port on its own localhost and relays each
// connection over the network to the owning node, which connects it to its own
// localhost. The browser on the viewing computer therefore opens exactly the URL
// it would on the owning one — http://localhost:4181 — and everything that
// depends on that keeps working: OAuth redirect URIs registered for localhost
// ports, cookies, CORS, absolute URLs, dev-server websockets. Exposing the
// owning computer's hostname instead would break all of them.

const tunnelProto = "fride-tunnel"

// TunnelHandler is the owning side: GET /fleet/tunnel?port=P with Upgrade:
// fride-tunnel. allowed decides which local ports may be reached — only ports
// of this node's own running tasks, never an arbitrary local service.
func TunnelHandler(allowed func(port int) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.Atoi(r.URL.Query().Get("port"))
		if err != nil || port <= 0 || port > 65535 {
			http.Error(w, "bad port", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Upgrade") != tunnelProto {
			http.Error(w, "expected Upgrade: "+tunnelProto, http.StatusBadRequest)
			return
		}
		if !allowed(port) {
			http.Error(w, fmt.Sprintf("port %d is not a port of a task running here", port), http.StatusForbidden)
			return
		}
		local, err := dialLocal(port)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			local.Close()
			http.Error(w, "cannot hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			local.Close()
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + tunnelProto + "\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		// Bytes the client sent right after the request are already buffered.
		var from io.Reader = conn
		if n := buf.Reader.Buffered(); n > 0 {
			pre, _ := buf.Reader.Peek(n)
			from = io.MultiReader(bytesReader(pre), conn)
		}
		pipe(conn, from, local)
	})
}

func bytesReader(b []byte) io.Reader {
	c := make([]byte, len(b))
	copy(c, b)
	return &sliceReader{b: c}
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// dialLocal reaches a port on this machine, IPv4 first: dev servers bind
// either family, and some only one.
func dialLocal(port int) (net.Conn, error) {
	var last error
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, fmt.Errorf("nothing answers on port %d here: %v", port, last)
}

// pipe copies both ways until either side is done, then closes both.
func pipe(a net.Conn, aReader io.Reader, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, aReader); closeWrite(b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); closeWrite(a); done <- struct{}{} }()
	<-done
	<-done
	a.Close()
	b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// openTunnel is the viewing side of one connection.
func openTunnel(ctx context.Context, n Network, addr string, port int) (net.Conn, *bufio.Reader, error) {
	c, err := n.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	req := fmt.Sprintf("GET /fleet/tunnel?port=%d HTTP/1.1\r\nHost: %s\r\nUpgrade: %s\r\nConnection: Upgrade\r\n\r\n", port, Host, tunnelProto)
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.Close()
		return nil, nil, fmt.Errorf("%s: %s", resp.Status, body)
	}
	return c, br, nil
}

// Mirror is one remote port served on this machine's localhost.
type Mirror struct {
	Node  string `json:"node"`
	Task  string `json:"task"`
	Port  int    `json:"port"`
	Conns int    `json:"connections"`
	ln    net.Listener
	live  map[net.Conn]struct{} // both ends of every open connection, for Close
	shut  bool
}

// Mirrors manages the ports this node mirrors from others.
type Mirrors struct {
	net Network
	mu  sync.Mutex
	m   map[int]*Mirror
}

func NewMirrors(n Network) *Mirrors { return &Mirrors{net: n, m: map[int]*Mirror{}} }

// Open mirrors each port of a remote task onto the same local port. A port
// already in use here (this machine's own task in the same slot, say) is
// reported, not taken over.
func (ms *Mirrors) Open(node, addr, task string, ports []int) (opened []int, problems []string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, p := range ports {
		if m, ok := ms.m[p]; ok {
			if m.Node == node && m.Task == task {
				opened = append(opened, p)
			} else {
				problems = append(problems, fmt.Sprintf(":%d already mirrors %s on %s", p, m.Task, m.Node))
			}
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			problems = append(problems, fmt.Sprintf(":%d is in use on this computer", p))
			continue
		}
		m := &Mirror{Node: node, Task: task, Port: p, ln: ln, live: map[net.Conn]struct{}{}}
		ms.m[p] = m
		go ms.serve(m, addr)
		opened = append(opened, p)
	}
	return opened, problems
}

func (ms *Mirrors) serve(m *Mirror, addr string) {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			if !ms.track(m, c, true) {
				c.Close()
				return
			}
			defer ms.untrack(m, c, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			remote, br, err := openTunnel(ctx, ms.net, addr, m.Port)
			cancel()
			if err != nil {
				_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\nFrIDE could not reach "+m.Node+": "+err.Error()+"\n")
				c.Close()
				return
			}
			if !ms.track(m, remote, false) {
				remote.Close()
				c.Close()
				return
			}
			defer ms.untrack(m, remote, false)
			pipe(c, c, &bufferedConn{Conn: remote, r: br})
		}()
	}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite passes a half-close on; embedding net.Conn would hide it.
func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// track registers an open connection; false once the mirror is closed.
func (ms *Mirrors) track(m *Mirror, c net.Conn, client bool) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if m.shut {
		return false
	}
	m.live[c] = struct{}{}
	if client {
		m.Conns++
	}
	return true
}

func (ms *Mirrors) untrack(m *Mirror, c net.Conn, client bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	delete(m.live, c)
	if client {
		m.Conns--
	}
}

// Close stops mirroring a task's ports (all of a node's when task is "", all
// of them when node is "" too).
func (ms *Mirrors) Close(node, task string) int {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	n := 0
	for p, m := range ms.m {
		if (node == "" || m.Node == node) && (task == "" || m.Task == task) {
			m.ln.Close()
			m.shut = true
			for c := range m.live {
				c.Close() // else a tunnel opened earlier keeps relaying
			}
			delete(ms.m, p)
			n++
		}
	}
	return n
}

// List reports what is mirrored, by port.
func (ms *Mirrors) List() []Mirror {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	out := make([]Mirror, 0, len(ms.m))
	for _, m := range ms.m {
		out = append(out, Mirror{Node: m.Node, Task: m.Task, Port: m.Port, Conns: m.Conns})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}
