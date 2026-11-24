package main

import (
	"os/exec"
	"testing"
)

// Issue #224: an inline widget is mounted into an orchestrator message row
// before the row is in the document, when its frame has no contentWindow yet.
// Its bridge calls must still be answered, and theme updates must not throw
// on a frame without a window. Runs web/panels.js under node, DOM stubbed.
func TestInlineWidgetMountedDetachedIsAnswered(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
const vm = require('vm');
const assert = require('assert');
const src = require('fs').readFileSync('web/panels.js', 'utf8');
const mkEl = tag => ({ tag, children: [], classList: { add() {}, remove() {} }, setAttribute() {},
  appendChild(c) { this.children.push(c); c.parent = this; },
  contains(c) { for (; c; c = c.parent) if (c === this) return true; return false; } });
let onMessage;
const fetched = [];
const sandbox = {
  console, location: { origin: 'http://x' }, JSON, String, Error,
  window: { addEventListener(t, f) { if (t === 'message') onMessage = f; } },
  document: { documentElement: {} },
  getComputedStyle: () => ({ getPropertyValue: () => '' }), activeTheme: () => 'dark',
  MutationObserver: class { observe() {} }, matchMedia: () => ({ addEventListener() {} }),
  setInterval: () => 0, clearInterval() {},
  el: (tag, cls) => mkEl(tag), icon: () => mkEl('i'),
  api: { get: async () => ({ panels: [] }), post: async () => ({}) },
  fetch: async (path, init) => { fetched.push([init.method, path, init.headers['X-Fride-Panel']]); return { ok: true, text: async () => '{"n":1}' }; },
};
vm.createContext(sandbox);
vm.runInContext(src + '\n;globalThis.mountPanel = mountPanel; globalThis.unmountFrames = unmountFrames; globalThis.sendPanelTheme = sendPanelTheme; globalThis.panelsUI = panelsUI;', sandbox);
(async () => {
  const row = mkEl('div');                            // not in the document yet
  const frame = sandbox.mountPanel(row, 'inline-7', '<p>hi</p>', 'inline-frame');
  assert.strictEqual(frame.contentWindow, undefined);
  sandbox.sendPanelTheme();                           // a window-less frame must not throw
  const got = [];
  const win = { postMessage: m => got.push(m) };
  frame.contentWindow = win;                          // the row is attached: the frame gets its window
  sandbox.sendPanelTheme();
  assert.strictEqual(got.length, 1, 'theme reaches the attached frame');
  assert.strictEqual(got[0].event, 'theme');
  await onMessage({ source: win, data: { fride: 1, id: 3, op: 'api', method: 'GET', path: '/api/tasks' } });
  assert.deepStrictEqual(JSON.parse(JSON.stringify(fetched)), [['GET', '/api/tasks', 'inline-7']]);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(got[1])), { fride: 1, id: 3, ok: true, result: { n: 1 } }, 'the bridge call is answered');
  await onMessage({ source: win, data: { fride: 1, id: 4, op: 'subscribe', topic: 'tasks', sub: 's' } });
  assert.strictEqual(sandbox.panelsUI.subs.size, 1);
  await onMessage({ source: { postMessage() { throw new Error('stranger answered'); } }, data: { fride: 1, id: 5, op: 'api', path: '/api/x' } });
  sandbox.unmountFrames(row);
  assert.strictEqual(sandbox.panelsUI.frames.length, 0, 'unmount drops the frame');
  assert.strictEqual(sandbox.panelsUI.subs.size, 0, 'and its subscriptions');
})().catch(e => { console.error(e); process.exit(1); });
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
