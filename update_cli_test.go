package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// An update installs the release's binary, does nothing when fride is that
// release already, and keeps the old binary when the download does not run.
func TestSelfUpdate(t *testing.T) {
	bins := map[string]string{
		"/ok":  "#!/bin/sh\necho fride v2\n",
		"/bad": "#!/bin/sh\nexit 1\n",
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := bins[r.URL.Path]; ok {
			io.WriteString(w, b)
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v2","assets":[{"name":"fride_%s_%s","browser_download_url":"%s%s"}]}`,
			runtime.GOOS, runtime.GOARCH, srv.URL, r.URL.Query().Get("bin"))
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "fride")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed := func() string { b, _ := os.ReadFile(exe); return string(b) }

	if tag, err := selfUpdate(srv.URL+"?bin=/ok", exe, "v2"); tag != "" || err != nil || installed() != "old" {
		t.Fatalf("already on v2: tag %q, err %v, binary %q", tag, err, installed())
	}
	if _, err := selfUpdate(srv.URL+"?bin=/bad", exe, "v1"); err == nil || installed() != "old" {
		t.Fatalf("a download that does not run: err %v, binary %q", err, installed())
	}
	if tag, err := selfUpdate(srv.URL+"?bin=/ok", exe, "v1"); tag != "v2" || err != nil || installed() != bins["/ok"] {
		t.Fatalf("v1 → v2: tag %q, err %v, binary %q", tag, err, installed())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".fride-update-*")); len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
