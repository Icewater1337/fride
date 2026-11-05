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

// Two overlapping fillNewTask calls, the first one's /repos answering last:
// the picker and state must hold only the second computer's data.
func TestFillNewTaskDropsOlderCall(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
const vm = require('vm');
const assert = require('assert');
const src = require('fs').readFileSync('web/app.js', 'utf8');
const i = src.indexOf('async function fillNewTask()');
const fn = src.slice(i, src.indexOf('\n}\n', i) + 2);
const picker = { rows: [], innerHTML: '', appendChild(r) { this.rows.push(r); } };
const elem = new Proxy(function () {}, {
  get: (_, k) => k === 'classList' ? { toggle() {} } : k === 'options' ? [] : elem,
  apply: () => elem,
});
const pending = [];
const sandbox = {
  console, state: { newFillGen: 0, newNode: 'A', newProfile: '', agents: [] },
  document: { createTextNode: () => elem },
  $: s => s === '#repo-picker' ? picker : elem, el: (t, c, text) => ({ text, appendChild() {}, addEventListener() {}, classList: elem, dataset: {}, options: [], contains: () => false, setAttribute() {} }),
  nodeBase: n => '/' + n, renderModePicker() {}, updateSlugPreview() {}, renderAgentPicker() {},
  renderGitPicker() {}, suggestGit() {}, agentInfo: () => ({ installed: true }),
  api: { get(url) { return new Promise(res => pending.push({ url, res })); } },
};
vm.createContext(sandbox);
vm.runInContext(fn + '\n;globalThis.fillNewTask = fillNewTask;', sandbox);
const answer = async (url, v) => {
  for (let n = 0; !pending.some(p => p.url === url); n++) if (n > 1000) throw new Error('no request for ' + url); else await new Promise(r => setImmediate(r));
  pending.splice(pending.findIndex(p => p.url === url), 1)[0].res(v);
  await new Promise(r => setImmediate(r));
};
(async () => {
  const st = sandbox.state;
  const pa = sandbox.fillNewTask();
  await answer('/A/config', { coreRepos: ['a'], defaultProfile: 'pa', profiles: [{ name: 'pa' }] });
  st.newNode = 'B';                                  // A is now waiting on /A/repos
  const pb = sandbox.fillNewTask();
  await answer('/B/config', { coreRepos: ['b'], defaultProfile: 'pb', profiles: [{ name: 'pb' }] });
  await answer('/B/repos', [{ name: 'b', exists: true, main: '/b' }]);
  await answer('/B/agents', { agents: [{ kind: 'bagent' }] });
  await answer('/B/git/identities', { identities: ['b'], default: 'b' });
  await pb;
  await answer('/A/repos', [{ name: 'a', exists: true, main: '/a' }]);   // A answers last
  await pa;
  assert.strictEqual(picker.rows.length, 1, 'only B repositories are listed');
  assert.deepStrictEqual(st.repos.map(r => r.name), ['b']);
  assert.strictEqual(st.newConfig.defaultProfile, 'pb');
  assert.strictEqual(st.newProfile, 'pb');
  assert.deepStrictEqual(st.agents, [{ kind: 'bagent' }]);
  assert.deepStrictEqual(st.newGit.identities, ['b']);
  assert.strictEqual(pending.length, 0, 'the older call must not go on to load agents or identities');
})().catch(e => { console.error(e); process.exit(1); });
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
