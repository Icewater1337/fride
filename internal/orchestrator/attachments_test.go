package orchestrator

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A screenshot sent with a message is a file the brain may read, named in what
// it is told, and shown with the user's message.
func TestAScreenshotReachesTheBrain(t *testing.T) {
	o, st := newStubOrch(t, newFakeHost(), Reply{Say: "I see it"})
	if _, err := o.SaveAttachment([]byte("not an image, whatever the browser said")); err == nil {
		t.Error("something other than an image was saved")
	}
	name, err := o.SaveAttachment(pngBytes(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../orchestrator.json", "x.png", name + "/../x", ""} {
		if o.AttachmentPath(bad) != "" {
			t.Errorf("%q resolved to a path", bad)
		}
	}
	if err := o.Say("what is wrong here?", "20260101-000000-deadbeef.png"); err == nil {
		t.Error("a message with an attachment that does not exist was accepted")
	}

	if err := o.Say("", name); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !o.Snapshot().Thinking && st.count(t) == 1 })
	in, _ := os.ReadFile(st.stdin)
	if !strings.Contains(string(in), o.AttachmentPath(name)) {
		t.Fatalf("the brain was not given the screenshot's path:\n%s", in)
	}
	argv, _ := os.ReadFile(st.argv)
	if !strings.Contains(string(argv), filepath.Dir(o.AttachmentPath(name))) {
		t.Fatalf("the brain may not read the screenshots' folder:\n%s", argv)
	}
	if msgs := o.Snapshot().Messages; len(msgs) == 0 || msgs[0].Role != RoleUser || len(msgs[0].Images) != 1 || msgs[0].Images[0] != name {
		t.Fatalf("the user's message does not carry the screenshot: %+v", msgs)
	}
}
