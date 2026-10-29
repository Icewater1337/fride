package fleet

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// A connection through the tunnel reaches the owning node's local port, and
// only ports the node allows.
func TestTunnelRelaysToAllowedPortsOnly(t *testing.T) {
	// The "service" on the owning node: answers each line with "echo: ".
	svc, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	port := svc.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := svc.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = io.WriteString(c, "echo: "+line)
				}
			}(c)
		}
	}()

	n, err := JoinLoopback(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ln, err := n.Listen(Port)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel", TunnelHandler(func(p int) bool { return p == port }))
	go http.Serve(ln, Guard(n, mux))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, br, err := openTunnel(ctx, n, ln.Addr().String(), port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "hello\n"); err != nil {
		t.Fatal(err)
	}
	got, err := br.ReadString('\n')
	if err != nil || got != "echo: hello\n" {
		t.Fatalf("through the tunnel: %q, %v", got, err)
	}

	// A port that is not one of the node's tasks is refused.
	if _, _, err := openTunnel(ctx, n, ln.Addr().String(), port+1); err == nil {
		t.Fatal("a port outside the allowed set was tunnelled")
	}
}

func TestGuardRefusesStrangers(t *testing.T) {
	n, _ := JoinLoopback(t.TempDir(), "x")
	h := Guard(n, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	rec := &recorder{h: http.Header{}}
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.10:5555" // not loopback: no identity
	h.ServeHTTP(rec, req)
	if rec.code != http.StatusForbidden {
		t.Fatalf("stranger got %d", rec.code)
	}
	req.RemoteAddr = "127.0.0.1:5555"
	rec = &recorder{h: http.Header{}}
	h.ServeHTTP(rec, req)
	if rec.code != 204 {
		t.Fatalf("own device got %d", rec.code)
	}
	_ = os.Getpid
}

type recorder struct {
	h    http.Header
	code int
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{"Mathias’s Mac mini": "mathias-s-mac-mini", "devbox.local": "devbox", "": "computer"} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A half-close on one side of a mirrored connection reaches the other side
// through bufferedConn, so the owner sees EOF.
func TestBufferedConnPassesHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- err
			return
		}
		defer c.Close()
		_, err = io.Copy(io.Discard, c) // returns once the peer half-closes
		got <- err
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := &bufferedConn{Conn: c, r: bufio.NewReader(c)}
	closeWrite(b)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the far side never saw the half-close")
	}
}

// Closing a mirror ends the tunnels already open through it.
func TestMirrorsCloseEndsOpenTunnels(t *testing.T) {
	n, err := JoinLoopback(t.TempDir(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ln, err := n.Listen(Port)
	if err != nil {
		t.Fatal(err)
	}
	// A stand-in owner: accepts the upgrade, then echoes.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+tunnelProto+"\r\nConnection: Upgrade\r\n\r\n")
				_, _ = io.Copy(c, br)
			}()
		}
	}()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	free.Close()

	ms := NewMirrors(n)
	if opened, problems := ms.Open("owner", ln.Addr().String(), "t", []int{port}); len(opened) != 1 {
		t.Skipf("mirror port not free: %v", problems)
	}
	ms.mu.Lock()
	m := ms.m[port]
	ms.mu.Unlock()
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "hi\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := bufio.NewReader(c).ReadString('\n'); err != nil || got != "hi\n" {
		t.Fatalf("through the mirror: %q, %v", got, err)
	}
	if ms.Close("", "") != 1 {
		t.Fatal("mirror not closed")
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("open connection survived Close: %v", err)
	}
	for i := 0; ; i++ {
		ms.mu.Lock()
		n := m.Conns
		ms.mu.Unlock()
		if n == 0 {
			break
		}
		if i > 100 {
			t.Fatalf("after Close: %d conns", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
