package fleet

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Tailnet is a node joined to the user's tailnet through tsnet: a device of
// its own named fride-<name>, with its state (keys) under dir.
type Tailnet struct {
	srv *tsnet.Server

	mu    sync.Mutex
	state State
	lc    *local.Client
}

// JoinTailnet starts joining in the background. State reports progress; while
// it is "needs-login", State().AuthURL is where the user signs this node in.
// ephemeral nodes disappear from the tailnet when they go offline — for
// throwaway setups.
func JoinTailnet(dir, name string, ephemeral bool) (*Tailnet, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	t := &Tailnet{state: State{Phase: "starting", Kind: "tailnet"}}
	t.srv = &tsnet.Server{
		Dir:       dir,
		Hostname:  Prefix + CleanName(name),
		Ephemeral: ephemeral,
		Logf:      func(string, ...any) {}, // tsnet is chatty; State carries what matters
		UserLogf:  func(string, ...any) {},
	}
	if err := t.srv.Start(); err != nil {
		return nil, fmt.Errorf("starting the tailnet node: %w", err)
	}
	lc, err := t.srv.LocalClient()
	if err != nil {
		return nil, err
	}
	t.lc = lc
	go t.watch()
	return t, nil
}

// watch keeps State current: quickly while joining, slowly once running, so a
// node that is logged out later (key expiry) says so.
func (t *Tailnet) watch() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := t.lc.StatusWithoutPeers(ctx)
		cancel()
		t.mu.Lock()
		if t.state.Phase == "stopped" {
			t.mu.Unlock()
			return
		}
		switch {
		case err != nil:
			t.state.Phase, t.state.Error = "starting", err.Error()
		case st.BackendState == "NeedsLogin" || st.BackendState == "NeedsMachineAuth":
			t.state.Phase, t.state.AuthURL, t.state.Error = "needs-login", st.AuthURL, ""
			if st.BackendState == "NeedsMachineAuth" {
				t.state.Error = "an admin of the tailnet has to approve this device"
			}
		case st.BackendState == "Running":
			t.state.Phase, t.state.AuthURL, t.state.Error = "running", "", ""
			t.state.Self = selfIdentity(st)
		default:
			t.state.Phase = "starting"
		}
		running := t.state.Phase == "running"
		t.mu.Unlock()
		if running {
			time.Sleep(15 * time.Second)
		} else {
			time.Sleep(time.Second)
		}
	}
}

func selfIdentity(st *ipnstate.Status) Identity {
	id := Identity{}
	if st.Self == nil {
		return id
	}
	id.UserID = strconv.FormatInt(int64(st.Self.UserID), 10)
	if u, ok := st.User[st.Self.UserID]; ok {
		id.LoginName = u.LoginName
	}
	id.Node = nodeName(st.Self)
	return id
}

// nodeName is the node's FrIDE name. The DNS name is used rather than the
// hostname because the tailnet keeps it unique (fride-mac, fride-mac-1).
func nodeName(p *ipnstate.PeerStatus) string {
	label := strings.SplitN(strings.TrimSuffix(p.DNSName, "."), ".", 2)[0]
	if label == "" {
		label = p.HostName
	}
	return strings.TrimPrefix(label, Prefix)
}

func (t *Tailnet) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *Tailnet) Listen(port int) (net.Listener, error) {
	return t.srv.Listen("tcp", ":"+strconv.Itoa(port))
}

func (t *Tailnet) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.srv.Dial(ctx, network, addr)
}

func (t *Tailnet) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	w, err := t.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{}
	if w.UserProfile != nil {
		id.UserID = strconv.FormatInt(int64(w.UserProfile.ID), 10)
		id.LoginName = w.UserProfile.LoginName
	}
	// A tagged device belongs to the tailnet, not to a person; it never counts
	// as the user, whatever profile it reports.
	if w.Node != nil && len(w.Node.Tags) > 0 {
		id.UserID = ""
	}
	if w.Node != nil {
		id.Node = strings.TrimPrefix(strings.SplitN(w.Node.Name, ".", 2)[0], Prefix)
	}
	return id, nil
}

func (t *Tailnet) Peers(ctx context.Context) ([]Peer, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	if st.Self == nil {
		return nil, ErrNotRunning
	}
	var out []Peer
	for _, p := range st.Peer {
		if p == nil || p.UserID != st.Self.UserID || !strings.HasPrefix(p.HostName, Prefix) {
			continue
		}
		if p.Tags != nil && p.Tags.Len() > 0 {
			continue
		}
		var ip string
		for _, a := range p.TailscaleIPs {
			if a.Is4() {
				ip = a.String()
				break
			}
		}
		if ip == "" && len(p.TailscaleIPs) > 0 {
			ip = p.TailscaleIPs[0].String()
		}
		if ip == "" {
			continue
		}
		out = append(out, Peer{
			Name: nodeName(p), Addr: net.JoinHostPort(ip, strconv.Itoa(Port)),
			Online: p.Online, OS: p.OS, LastSeen: p.LastSeen,
		})
	}
	return out, nil
}

func (t *Tailnet) Close() error {
	t.mu.Lock()
	t.state.Phase = "stopped"
	t.mu.Unlock()
	return t.srv.Close()
}
