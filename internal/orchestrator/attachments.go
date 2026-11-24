package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Screenshots the user attaches to a chat message.
//
// The orchestrator thinks in a headless session whose only tools read files, and
// Claude Code's Read opens images. So an attachment is a file in a folder the
// brain may read (AttachmentsDir, one of its add-dirs), and the user's message
// names its path. To pass one on, the brain puts that path in what it sends a
// task's agent; the host copies the file into the task's workspace on the way,
// where the agent can open it without a permission prompt.

// MaxAttachment bounds one image, and MaxAttachments how many go with a message.
// A screenshot of a whole screen is a few MB as PNG.
const (
	MaxAttachment  = 10 << 20
	MaxAttachments = 8
)

// imageExt is the image types Claude Code reads, by what their bytes say
// (http.DetectContentType), never by what the browser called them.
var imageExt = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
}

// AttachmentPattern matches a name SaveAttachment gives out; nothing else is
// served or read from the folder, or copied out of it.
const AttachmentPattern = `[0-9]{8}-[0-9]{6}-[0-9a-f]{8}\.(?:png|jpg|gif|webp)`

var attachmentName = regexp.MustCompile(`^` + AttachmentPattern + `$`)

// AttachmentsDir is where the chat's screenshots are kept.
func AttachmentsDir(stateDir string) string {
	return filepath.Join(stateDir, "orchestrator", "attachments")
}

// SaveAttachment stores one image and returns its name.
func (o *Orchestrator) SaveAttachment(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("an attachment is empty")
	}
	if len(data) > MaxAttachment {
		return "", fmt.Errorf("an attachment is %d MB; at most %d MB each", len(data)>>20, MaxAttachment>>20)
	}
	ext, ok := imageExt[http.DetectContentType(data)]
	if !ok {
		return "", fmt.Errorf("only PNG, JPEG, GIF and WebP images can be attached")
	}
	var r [4]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	name := time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(r[:]) + ext
	if err := os.MkdirAll(o.attachDir, 0o700); err != nil {
		return "", err
	}
	return name, os.WriteFile(filepath.Join(o.attachDir, name), data, 0o600)
}

// AttachmentPath is where an attachment of that name is, or "" for a name
// SaveAttachment would not have given: the name comes from a URL.
func (o *Orchestrator) AttachmentPath(name string) string {
	if !attachmentName.MatchString(name) {
		return ""
	}
	return filepath.Join(o.attachDir, name)
}

// attachedLead is the part of the user's message that hands the brain its
// screenshots: their paths, to Read, and how to pass one on.
func (o *Orchestrator) attachedLead(names []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("THE USER ATTACHED " + fmt.Sprint(len(names)) + " IMAGE(S). Open each with Read before you answer:\n")
	for _, n := range names {
		b.WriteString("- " + filepath.Join(o.attachDir, n) + "\n")
	}
	b.WriteString("To give one to a task's agent, put its path, exactly as above, in the text you send; " +
		"FrIDE copies it into that task's workspace and tells the agent where.\n\n")
	return b.String()
}
