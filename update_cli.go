package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// latestRelease is where `fride update` looks. The release workflow
// (.github/workflows/release.yml) publishes one binary per platform there.
const latestRelease = "https://api.github.com/repos/Icewater1337/fride/releases/latest"

func cmdUpdate() {
	exe, err := os.Executable()
	check(err)
	// fride is usually reached through a symlink on PATH: replace the file it
	// points to, not the link.
	exe, err = filepath.EvalSymlinks(exe)
	check(err)
	tag, err := selfUpdate(latestRelease, exe, version)
	check(err)
	if tag == "" {
		fmt.Println("fride " + version + " is the latest version")
		return
	}
	fmt.Printf("fride %s → %s\n", version, tag)
	fmt.Println("a running `fride serve` keeps the old version until it is restarted")
}

// selfUpdate replaces exe with the latest release's binary for this platform,
// unless current is that release already. It returns the new tag, or "" when
// there was nothing to do.
func selfUpdate(api, exe, current string) (string, error) {
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := httpGet(api, func(r io.Reader) error { return json.NewDecoder(r).Decode(&rel) }); err != nil {
		return "", err
	}
	if rel.Tag == current {
		return "", nil
	}
	want := "fride_" + runtime.GOOS + "_" + runtime.GOARCH
	url, sumsURL := "", ""
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			url = a.URL
		case "SHA256SUMS":
			sumsURL = a.URL
		}
	}
	if url == "" {
		return "", fmt.Errorf("release %s has no %s", rel.Tag, want)
	}
	// Refuse a release without checksums rather than skip the check: every
	// release the workflow publishes has them, and an asset that went missing
	// must not switch verification off.
	if sumsURL == "" {
		return "", fmt.Errorf("release %s has no SHA256SUMS", rel.Tag)
	}
	sum := ""
	err := httpGet(sumsURL, func(r io.Reader) error {
		b, err := io.ReadAll(io.LimitReader(r, 1<<20))
		for _, line := range bytes.Split(b, []byte("\n")) {
			// sha256sum prints "<hex>  <name>", or "<hex> *<name>" in binary mode.
			if f := strings.Fields(string(line)); len(f) == 2 && strings.TrimPrefix(f[1], "*") == want {
				sum = f[0]
			}
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if sum == "" {
		return "", fmt.Errorf("SHA256SUMS of release %s lists no %s", rel.Tag, want)
	}

	// Download next to exe and rename over it. The rename is atomic, so a
	// failed update never leaves a broken fride behind, and a running
	// `fride serve` keeps the file it has open. Writing into exe in place
	// would also trip macOS, which kills a binary whose signed pages change.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".fride-update-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // nothing left to remove after the rename
	h := sha256.New()
	err = httpGet(url, func(r io.Reader) error { _, err := io.Copy(io.MultiWriter(tmp, h), r); return err })
	if got := hex.EncodeToString(h.Sum(nil)); err == nil && !strings.EqualFold(got, sum) {
		err = fmt.Errorf("the downloaded %s does not match SHA256SUMS: %s, want %s", want, got, sum)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err != nil {
		return "", err
	}
	// Run it before trusting it: a cut-off download or a binary this machine
	// cannot execute fails here, while the old fride is still in place.
	out, err := exec.Command(tmp.Name(), "version").Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != "fride "+rel.Tag {
		return "", fmt.Errorf("the downloaded %s does not run here (%v): %q", rel.Tag, err, got)
	}
	return rel.Tag, os.Rename(tmp.Name(), exe)
}

func httpGet(url string, read func(io.Reader) error) error {
	res, err := http.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	return read(res.Body)
}
