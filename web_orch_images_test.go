package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Runs the real addOrchImages from web/app.js under node: ten images dropped
// at once must attach only ORCH_MAX_IMAGES and toast once (issue #244).
func TestAddOrchImagesCapCountsPendingReads(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i, j := strings.Index(s, "const ORCH_MAX_IMAGES"), strings.Index(s, "function renderOrchImages")
	if i < 0 || j < i {
		t.Fatal("addOrchImages not found in web/app.js")
	}
	script := `
const vm = require('vm');
const assert = require('assert');
const readers = [], toasts = [];
class FileReader {
  readAsDataURL(f) { this.result = 'data:' + f.name; readers.push(this); }
}
const sandbox = { FileReader, toast: m => toasts.push(m), renderOrchImages() {} };
vm.createContext(sandbox);
vm.runInContext(process.env.ORCH_JS + '\n;globalThis.add = addOrchImages; globalThis.imgs = () => orchImages;', sandbox);
const files = n => Array.from({ length: n }, (_, k) => ({ name: 'i' + k + '.png', type: 'image/png', size: 1 }));
sandbox.add(files(10));
sandbox.add(files(3));                       // a second drop while reads still run
for (const r of readers) { r.onload && r.onload(); r.onloadend && r.onloadend(); }
assert.strictEqual(sandbox.imgs().length, 8, 'at most 8 images attached');
assert.strictEqual(toasts.length, 2, 'one cap toast per drop');
`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(os.Environ(), "ORCH_JS="+s[i:j])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
