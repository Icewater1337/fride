package main

import (
	"os"
	"os/exec"
	"testing"
)

// The web UI has no build step and no JS test runner, so this runs the real
// web/review.js under node with the DOM and app.js helpers stubbed. It skips
// where node is missing.
func TestLoadReviewGuardIsPerTask(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := os.ReadFile("web/review.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `
const vm = require('vm');
const assert = require('assert');
const src = process.env.REVIEW_JS;
const classes = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
const elem = new Proxy(function () {}, {
  get: (_, k) => k === 'classList' ? classes : k === 'checked' ? false : elem,
  apply: () => elem,
});
const state = { gen: 1 };
const pending = [];
const sandbox = {
  console, setInterval() { return 0; }, clearInterval() {}, setTimeout() { return 0; },
  document: { hidden: true, addEventListener() {} }, localStorage: { getItem() { return null; } },
  $: () => elem, el: () => elem, icon: () => elem, toast() {},
  pin: () => ({ slug: 't' + state.gen, base: '/api', gen: state.gen }),
  stale: p => p.gen !== state.gen,
  api: { get(url) { return new Promise(res => pending.push({ url, res })); } },
};
vm.createContext(sandbox);
vm.runInContext(src + '\n;globalThis.loadReview = loadReview; globalThis.rv = rv;', sandbox);
(async () => {
  const a = sandbox.pin();
  const pa = sandbox.loadReview(a, true);
  sandbox.loadReview(a, true);                       // same task: skipped
  assert.strictEqual(pending.length, 1, 'second load for the same pin must be skipped');
  state.gen = 2;                                     // switch to task B
  const pb = sandbox.loadReview(sandbox.pin(), true);
  assert.strictEqual(pending.length, 2, 'a load for another task must start');
  pending[0].res({});                                // A's late answer
  await pa;
  assert.strictEqual(sandbox.rv.data, null, "A's stale answer must stay dropped");
  sandbox.loadReview(sandbox.pin(), true);           // B still in flight
  assert.strictEqual(pending.length, 2, "A's late answer must not clear B's flag");
  pending[1].res({ repos: [] });
  await pb.catch(() => {});
  assert.strictEqual(sandbox.rv.loading, null);
  assert.deepStrictEqual(sandbox.rv.data, { repos: [] }, "B's answer is drawn");
})().catch(e => { console.error(e); process.exit(1); });
`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(os.Environ(), "REVIEW_JS="+string(src))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
