package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Icewater1337/fride/internal/config"
)

// Ids come from the URL and the command line, and become a folder under the
// state directory and a ref: nothing that leaves either is accepted.
func TestIDsStayInside(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	for _, id := range []string{"", ".", "..", "../state", "a/b", "x y", "a\\b"} {
		if _, err := Load(cfg, id); err == nil {
			t.Errorf("Load(%q) accepted", id)
		}
		if err := Purge(cfg, id); err == nil {
			t.Errorf("Purge(%q) accepted", id)
		}
	}
}

// Expiry keeps what it cannot read — nothing says which refs it holds — and
// clears the empty folder an interrupted save leaves, once it is old.
func TestExpireKeepsWhatItCannotRead(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	torn := filepath.Join(Dir(cfg), "torn")
	empty := filepath.Join(Dir(cfg), "empty")
	for _, d := range []string{torn, empty} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(torn, manifest), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * Keep)
	for _, d := range []string{torn, empty} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := Expire(cfg, time.Now())
	if err != nil || len(gone) != 0 {
		t.Fatalf("Expire = %v, %v", gone, err)
	}
	if _, err := os.Stat(filepath.Join(torn, manifest)); err != nil {
		t.Error("an unreadable entry was deleted")
	}
	if _, err := os.Stat(empty); err == nil {
		t.Error("the old empty folder is still there")
	}
	if list, err := List(cfg); err != nil || len(list) != 0 {
		t.Errorf("List = %v, %v", list, err)
	}
}
