package server

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A screenshot from the chat is served back for its thumbnail, refused when it
// is not an image, and given to a task's agent as a copy in its workspace.
func TestChatScreenshots(t *testing.T) {
	s, _, _ := testServer(t)
	h := s.guard(s.Routes())

	if w := do(t, h, "POST", "/api/orchestrator/say",
		`{"text":"look","images":["data:image/png;base64,`+base64.StdEncoding.EncodeToString([]byte("plain text"))+`"]}`); w.Code != 400 {
		t.Fatalf("a non-image was accepted: %d %s", w.Code, w.Body)
	}
	if left, _ := os.ReadDir(filepath.Join(s.cfg.StateDir, "orchestrator", "attachments")); len(left) != 0 {
		t.Fatalf("a refused upload left files behind: %v", left)
	}

	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	name, err := s.orch.SaveAttachment(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", "/api/orchestrator/attachments/"+name, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), b.Bytes()) {
		t.Fatalf("the screenshot was not served: %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/orchestrator/attachments/..%2Forchestrator.json", ""); w.Code != 404 {
		t.Fatalf("a path outside the screenshots was served: %d", w.Code)
	}

	if err := os.MkdirAll(s.cfg.WorkspaceDir("t1"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := s.orch.AttachmentPath(name)
	host := orchHost{s}
	out, err := host.carryAttachments("t1", "Fix the layout shown in "+src+".")
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(s.cfg.WorkspaceDir("t1"), ".fride", "attachments", name)
	if out != "Fix the layout shown in "+copied+"." {
		t.Fatalf("the text does not point at the workspace's copy: %q", out)
	}
	if got, _ := os.ReadFile(copied); !bytes.Equal(got, b.Bytes()) {
		t.Fatal("the screenshot was not copied into the workspace")
	}
	if out, _ := host.carryAttachments("t1", "nothing attached"); !strings.HasPrefix(out, "nothing") {
		t.Fatalf("text without a screenshot changed: %q", out)
	}
}
