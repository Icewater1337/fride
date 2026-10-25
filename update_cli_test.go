package main

import (
	"crypto/sha256"
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
// release already, and keeps the old binary when the download does not run,
// does not match SHA256SUMS, or the release has no SHA256SUMS.
func TestSelfUpdate(t *testing.T) {
	name := fmt.Sprintf("fride_%s_%s", runtime.GOOS, runtime.GOARCH)
	bins := map[string]string{
		"/ok":  "#!/bin/sh\necho fride v2\n",
		"/bad": "#!/bin/sh\nexit 1\n",
	}
	sumOf := func(b string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(b))) }
	sums := map[string]string{
		"/sums-ok":       sumOf(bins["/ok"]) + "  " + name + "\n" + sumOf("x") + "  fride_other_os\n",
		"/sums-bad":      sumOf(bins["/bad"]) + " *" + name + "\n",
		"/sums-tampered": sumOf("something else") + "  " + name + "\n",
		"/sums-other":    sumOf(bins["/ok"]) + "  fride_other_os\n",
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := bins[r.URL.Path]; ok {
			io.WriteString(w, b)
			return
		}
		if b, ok := sums[r.URL.Path]; ok {
			io.WriteString(w, b)
			return
		}
		q := r.URL.Query()
		sumsAsset := ""
		if q.Get("sums") != "" {
			sumsAsset = fmt.Sprintf(`,{"name":"SHA256SUMS","browser_download_url":"%s%s"}`, srv.URL, q.Get("sums"))
		}
		fmt.Fprintf(w, `{"tag_name":"v2","assets":[{"name":"%s","browser_download_url":"%s%s"}%s]}`,
			name, srv.URL, q.Get("bin"), sumsAsset)
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "fride")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed := func() string { b, _ := os.ReadFile(exe); return string(b) }

	if tag, err := selfUpdate(srv.URL+"?bin=/ok&sums=/sums-ok", exe, "v2"); tag != "" || err != nil || installed() != "old" {
		t.Fatalf("already on v2: tag %q, err %v, binary %q", tag, err, installed())
	}
	for _, refused := range []string{
		"?bin=/bad&sums=/sums-bad",     // does not run
		"?bin=/ok&sums=/sums-tampered", // hash differs
		"?bin=/ok&sums=/sums-other",    // this platform not listed
		"?bin=/ok",                     // no SHA256SUMS at all
	} {
		if _, err := selfUpdate(srv.URL+refused, exe, "v1"); err == nil || installed() != "old" {
			t.Fatalf("%s: err %v, binary %q", refused, err, installed())
		}
	}
	if tag, err := selfUpdate(srv.URL+"?bin=/ok&sums=/sums-ok", exe, "v1"); tag != "v2" || err != nil || installed() != bins["/ok"] {
		t.Fatalf("v1 → v2: tag %q, err %v, binary %q", tag, err, installed())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".fride-update-*")); len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
