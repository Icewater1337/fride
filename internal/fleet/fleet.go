// Package fleet links the computers a person works on into one FrIDE.
//
// Every computer runs its own `fride serve` and stays the owner of its tasks —
// worktrees, terminals, stacks. Nothing is centralised: the UI on any of them
// lists the tasks of all of them and routes each action to the computer that
// owns the task. What this package provides is the part underneath that: how
// nodes find and reach each other, and who is calling.
//
// The network is a Tailscale tailnet, joined by FrIDE itself through tsnet: each
// `fride serve` is a device of its own, signed in once with the user's account
// (Google, or whatever the tailnet uses). That gives encrypted connections that
// work through NAT and across the internet, and — the part that matters most
// for a tool whose API opens terminals — a verified identity on every
// connection. A node only ever answers devices of the SAME user: this is a
// fleet of one person's computers, not a sharing mechanism.
//
// A loopback network implements the same interface over 127.0.0.1, so a fleet
// of FrIDE processes on one machine can be run and tested without a tailnet.
package fleet

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Port is where a node's fleet API listens on the network. On a tailnet every
// node is its own device, so they can all use the same port.
const Port = 7777

// Prefix marks a tailnet device as a FrIDE node: its hostname is Prefix+name.
const Prefix = "fride-"

// Identity is who is behind a connection.
type Identity struct {
	UserID    string `json:"userId"`
	LoginName string `json:"loginName"`
	Node      string `json:"node,omitempty"`
}

// Peer is another node of the fleet.
type Peer struct {
	Name     string    `json:"name"`
	Addr     string    `json:"-"` // host:port to dial
	Online   bool      `json:"online"`
	OS       string    `json:"os,omitempty"`
	LastSeen time.Time `json:"lastSeen,omitempty"`
}

// State is how far joining the network has got.
type State struct {
	// Phase: "starting", "needs-login", "running", "stopped" or "error".
	Phase string `json:"phase"`
	// AuthURL is where the user signs this node in, while Phase is needs-login.
	AuthURL string `json:"authUrl,omitempty"`
	// Self is who this node runs as, once running.
	Self  Identity `json:"self"`
	Error string   `json:"error,omitempty"`
	// Kind is "tailnet" or "loopback".
	Kind string `json:"kind"`
}

// Network is how nodes reach each other.
type Network interface {
	// Listen accepts connections from other nodes.
	Listen(port int) (net.Listener, error)
	// DialContext reaches another node's address (from Peer.Addr).
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// WhoIs identifies the user behind an accepted connection's remote address.
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
	// Peers lists the other FrIDE nodes of the same user.
	Peers(ctx context.Context) ([]Peer, error)
	// State reports how far joining has got.
	State() State
	Close() error
}

// ErrNotRunning is returned while the node has not joined yet.
var ErrNotRunning = errors.New("this computer has not joined the fleet yet")

var nameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// CleanName turns a machine name into a node name: lowercase letters, digits
// and dashes, as DNS labels allow.
func CleanName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".local")
	s = nameRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "computer"
	}
	return s
}

// Guard admits only connections from devices of the same user as this node,
// and puts the caller's identity on the request context.
//
// Every node checks this itself, for every request. A node never trusts
// another node's word about who is asking: the identity comes from the network
// layer, which on a tailnet is cryptographic.
func Guard(n Network, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := n.State()
		if st.Phase != "running" {
			http.Error(w, "fleet: this node is not running", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		id, err := n.WhoIs(ctx, r.RemoteAddr)
		cancel()
		if err != nil {
			http.Error(w, "fleet: cannot identify the caller", http.StatusForbidden)
			return
		}
		if id.UserID == "" || id.UserID != st.Self.UserID {
			http.Error(w, "fleet: only devices of "+st.Self.LoginName+" may use this computer", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, id)))
	})
}

type callerKey struct{}

// Caller is the identity Guard established for a request, if any.
func Caller(r *http.Request) (Identity, bool) {
	id, ok := r.Context().Value(callerKey{}).(Identity)
	return id, ok
}

// HTTPClient talks to other nodes over the network.
func HTTPClient(n Network, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:         n.DialContext,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     60 * time.Second,
		},
	}
}
