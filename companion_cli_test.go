package main

import (
	"testing"

	"github.com/Icewater1337/fride/internal/config"
	"github.com/Icewater1337/fride/internal/settings"
)

// Without companion mode, serve joins the fleet exactly as it always did:
// from --fleet, or from fleet.enabled in config.json. Companion mode, from the
// flag, the environment or config.json, keeps the fleet off.
func TestServeModes(t *testing.T) {
	noEnv := func(string) string { return "" }
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "FRIDE_UNDER_FRIDOLIN" {
				return v
			}
			return ""
		}
	}
	plain := &config.Config{Settings: &settings.Settings{}}
	fleetCfg := &config.Config{Settings: &settings.Settings{Fleet: &settings.Fleet{Enabled: true, Name: "studio"}}}
	compCfg := &config.Config{Settings: &settings.Settings{Companion: &settings.Companion{Enabled: true}}}
	none := &config.Config{}

	for _, c := range []struct {
		name           string
		cfg            *config.Config
		args           []string
		getenv         func(string) string
		companion, flt bool
		fleetName      string
	}{
		{"nothing", plain, nil, noEnv, false, false, ""},
		{"not set up", none, nil, noEnv, false, false, ""},
		{"--fleet", plain, []string{"--fleet", "--name", "mini"}, noEnv, false, true, "mini"},
		{"fleet.enabled", fleetCfg, nil, noEnv, false, true, ""},
		{"--under-fridolin", fleetCfg, []string{"--under-fridolin"}, noEnv, true, false, ""},
		{"--under-fridolin --fleet", plain, []string{"--fleet", "--under-fridolin"}, noEnv, true, false, ""},
		{"env", fleetCfg, nil, env("1"), true, false, ""},
		{"env off", fleetCfg, nil, env("0"), false, true, ""},
		{"companion.enabled", compCfg, []string{"--fleet"}, noEnv, true, false, ""},
	} {
		m := serveMode(c.cfg, c.args, c.getenv)
		if m.companion != c.companion || m.fleet != c.flt || m.name != c.fleetName {
			t.Errorf("%s: got %+v, want companion=%v fleet=%v name=%q", c.name, m, c.companion, c.flt, c.fleetName)
		}
	}
}
