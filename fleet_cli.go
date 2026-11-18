package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/fleet"
	"github.com/Icewater1337/fride/internal/server"
	"github.com/Icewater1337/fride/internal/stack"
	"github.com/Icewater1337/fride/internal/store"
)

// joinFleet connects a serving FrIDE to the user's fleet. The network is the
// tailnet, unless FRIDE_FLEET_LOOPBACK names a directory, which makes a fleet
// of FrIDE processes on this one machine (for tests and trying it out).
func joinFleet(cfg *config.Config, srv *server.Server, name string) {
	if name == "" {
		if cfg.Settings != nil && cfg.Settings.Fleet != nil && cfg.Settings.Fleet.Name != "" {
			name = cfg.Settings.Fleet.Name
		} else {
			h, _ := os.Hostname()
			name = h
		}
	}
	name = fleet.CleanName(name)
	var n fleet.Network
	var err error
	if dir := os.Getenv("FRIDE_FLEET_LOOPBACK"); dir != "" {
		n, err = fleet.JoinLoopback(dir, name)
	} else {
		ephemeral := cfg.Settings != nil && cfg.Settings.Fleet != nil && cfg.Settings.Fleet.Ephemeral
		n, err = fleet.JoinTailnet(filepath.Join(cfg.StateDir, "tailnet"), name, ephemeral)
	}
	check(err)
	check(srv.StartFleet(n, name))
	// Say where to sign in, once, in the terminal too — the UI shows it as well.
	go func() {
		said := ""
		for i := 0; i < 600; i++ {
			st := n.State()
			switch {
			case st.Phase == "needs-login" && st.AuthURL != "" && st.AuthURL != said:
				fmt.Printf("\nfleet: sign this computer (%s) in to your tailnet: %s\n\n", name, st.AuthURL)
				said = st.AuthURL
			case st.Phase == "running":
				fmt.Printf("fleet: %s joined as %s\n", name, st.Self.LoginName)
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

func cmdFleet(args []string) {
	var v struct {
		Enabled bool         `json:"enabled"`
		Self    string       `json:"self"`
		State   fleet.State  `json:"state"`
		Peers   []fleet.Peer `json:"peers"`
		Mirrors []fleet.Mirror
	}
	check(serverCall("GET", "/api/fleet", nil, &v, 15*time.Second))
	if !v.Enabled {
		fmt.Println("this computer is not part of a fleet — start it with `fride serve --fleet`")
		return
	}
	fmt.Printf("this computer: %s (%s, %s)\n", v.Self, v.State.Phase, v.State.Kind)
	if v.State.AuthURL != "" {
		fmt.Printf("  sign it in: %s\n", v.State.AuthURL)
	}
	if v.State.Self.LoginName != "" {
		fmt.Printf("  account:     %s\n", v.State.Self.LoginName)
	}
	if v.State.Error != "" {
		fmt.Printf("  ! %s\n", v.State.Error)
	}
	fmt.Println()
	if len(v.Peers) == 0 {
		fmt.Println("no other computers yet — run `fride serve --fleet` on another one, signed in to the same account")
		return
	}
	w := tw()
	fmt.Fprintln(w, "COMPUTER\tSTATE\tOS")
	for _, p := range v.Peers {
		state := "offline"
		if p.Online {
			state = "online"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, state, p.OS)
	}
	w.Flush()
}

func cmdTaskMove(cfg *config.Config, st *store.Store, args []string) {
	noAgent, args := flagBool(args, "--no-agent")
	startStack, args := flagBool(args, "--stack")
	if len(args) < 2 {
		check(fmt.Errorf("usage: fride task move <task> <computer> [--stack] [--no-agent]"))
	}
	t := resolveTask(cfg, st, args[:1])
	to := args[1]
	fmt.Printf("moving %s to %s — its agent and stack stop here…\n", t.Slug, to)
	var res struct {
		Node  string   `json:"node"`
		Moved string   `json:"moved"`
		Notes []string `json:"notes"`
	}
	check(serverCall("POST", "/api/tasks/"+t.Slug+"/move",
		map[string]any{"to": to, "startAgent": !noAgent, "startStack": startStack}, &res, 30*time.Minute))
	fmt.Printf("%s is now on %s: %s\n", t.Slug, res.Node, res.Moved)
	for _, n := range res.Notes {
		fmt.Printf("  %s\n", n)
	}
}

// fleetSlotsFromServer lets a CLI `stack up` avoid slots the user's other
// computers hold, when a fleet-enabled server is running here to ask.
func fleetSlotsFromServer() {
	var held map[string]string
	if err := serverCall("GET", "/api/fleet/slots", nil, &held, 3*time.Second); err != nil || len(held) == 0 {
		return
	}
	busy := map[int]bool{}
	for k := range held {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err == nil {
			busy[n] = true
		}
	}
	stack.FleetBusy = func(k int) bool { return busy[k] }
}
