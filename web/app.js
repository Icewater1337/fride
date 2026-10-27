/* FrIDE UI. Vanilla JS on purpose: no build step, so the Go binary is the
   only thing you have to run. */

const api = {
  async get(u) { return handle(await fetch(u)); },
  async post(u, body) {
    return handle(await fetch(u, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body)
    }));
  },
  async put(u, body) {
    return handle(await fetch(u, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }));
  },
  async del(u) { return handle(await fetch(u, { method: 'DELETE' })); },
};

async function handle(res) {
  const txt = await res.text();
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch { data = { error: txt }; }
  if (!res.ok) { const e = new Error((data && data.error) || res.statusText); e.data = data; throw e; }
  return data;
}

const $ = (s) => document.querySelector(s);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

// icon draws one of the symbols defined at the top of index.html.
const SVGNS = 'http://www.w3.org/2000/svg';
function icon(name, cls) {
  const s = document.createElementNS(SVGNS, 'svg');
  s.setAttribute('class', 'i' + (cls ? ' ' + cls : ''));
  s.setAttribute('aria-hidden', 'true');
  const u = document.createElementNS(SVGNS, 'use');
  u.setAttribute('href', '#i-' + name);
  s.appendChild(u);
  return s;
}

// setLabel changes a button's words without throwing away its icon.
function setLabel(btn, text) {
  const span = btn.querySelector('span:not(.count)');
  if (span) span.textContent = text; else btn.textContent = text;
}

const state = {
  tasks: [],
  repos: [],
  agents: [],          // agent CLIs and whether this machine has them
  agentDefault: 'claude',
  newAgent: 'claude',  // the choice in the New task modal
  taskAgent: 'claude', // the open task's agent
  current: null,       // task slug
  gen: 0,              // bumped whenever the open task changes; see pin()
  terms: new Map(),    // sessionID -> {term, fit, ws, pane, tab, sess}
  closing: new Set(),  // sessions whose tab was closed and whose DELETE is in flight
  activeTerm: null,
  filePath: '',
  openFile: '',
  showHidden: false,
  poll: null,
  navigated: false,   // the user has chosen a view; the boot must not override it
  viaIde: new Set(),   // services to open in their IDE on the next Start
  config: null,        // /api/config: profiles, core repos, whether set up
  taskConfig: null,    // the same, from the computer the open task lives on
  newProfile: '',      // the choice in the New task modal
  fleet: { enabled: false, self: '', peers: [], mirrors: [], state: {} },
  currentNode: '',     // the computer the open task lives on ('' = this one)
  newNode: '',         // the computer chosen in the New task modal
  gitIds: {},          // API base -> the git identities of that computer
  gitDefaults: {},     // API base -> its git.default
  newGit: null,        // /git/identities of the computer in the New task modal
  gitEdited: false,    // the identity in the New task modal was picked by hand
  resumable: [],
  dragging: false,
  boardKey: '',
  switcherKey: '',
};

/* ------------------------------------------------------------------ the open task

   Every loader of the task view awaits the server and then draws into one
   shared set of panels. Opening task B while task A's answer is in flight used
   to let A's late answer land in B's panels — and a button drawn from it then
   acted on B with A's data. So a loader pins the task it was started for and
   draws nothing once a different task has been opened since. */

function pin() {
  return { slug: state.current, node: state.currentNode, base: tb(), gen: state.gen };
}
function stale(p) { return !p || p.gen !== state.gen; }

/* ------------------------------------------------------------------ feedback

   Errors used to be alert()s, which stop everything until dismissed and lose
   the context behind them. A toast says the same without taking the page. */

function toast(msg, kind = 'error', ms) {
  const host = $('#toasts');
  const t = el('div', 'toast ' + kind);
  t.setAttribute('role', kind === 'error' ? 'alert' : 'status');
  t.appendChild(icon(kind === 'error' ? 'alert' : kind === 'ok' ? 'check' : 'info'));
  t.appendChild(el('div', 'msg', String(msg)));
  const x = el('button', 'iconbtn xs');
  x.title = 'Dismiss';
  x.setAttribute('aria-label', 'Dismiss');
  x.appendChild(icon('x'));
  const close = () => {
    if (!t.isConnected || t.classList.contains('leaving')) return;
    t.classList.add('leaving');
    setTimeout(() => t.remove(), 200);
  };
  x.addEventListener('click', close);
  t.appendChild(x);
  host.appendChild(t);
  // An error stays long enough to be read; a long one longer.
  const life = ms !== undefined ? ms : (kind === 'error' ? 9000 + String(msg).length * 40 : 4500);
  if (life > 0) setTimeout(close, life);
  while (host.children.length > 4) host.firstChild.remove();
}

// ask is confirm() drawn as a dialog: same question, but it can say which
// button destroys something, and it does not freeze the page behind it.
function ask({ title, body, ok = 'OK', danger = false, icon: ic }) {
  return new Promise(resolve => {
    const pop = $('#askpop');
    const before = document.activeElement;
    $('#ask-title').textContent = title;
    const b = $('#ask-body');
    b.innerHTML = '';
    if (body instanceof Node) b.appendChild(body); else b.textContent = body || '';
    const ico = $('#ask-icon');
    ico.innerHTML = '';
    ico.className = 'ask-icon' + (danger ? ' danger' : '');
    ico.appendChild(icon(ic || (danger ? 'alert' : 'info')));
    const okb = $('#ask-ok'), cancel = $('#ask-cancel');
    okb.textContent = ok;
    okb.className = danger ? 'danger solid' : 'primary';
    const done = v => {
      pop.classList.add('hidden');
      okb.onclick = cancel.onclick = null;
      if (before && before.focus) before.focus();
      resolve(v);
    };
    okb.onclick = () => done(true);
    cancel.onclick = () => done(false);
    pop.classList.remove('hidden');
    // Enter must not be the way something gets deleted by accident.
    (danger ? cancel : okb).focus();
  });
}

function openModal(id) {
  const m = $('#' + id);
  m.classList.remove('hidden');
  return m;
}

// Escape closes the topmost dialog (or menu) the way its own Cancel would, so
// whatever that button cleans up is cleaned up either way.
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (openMenu) { closeMenu(); e.preventDefault(); return; }
  const open = [...document.querySelectorAll('.modal:not(.hidden)')];
  const top = open[open.length - 1];
  if (!top) return;
  e.preventDefault();
  const c = top.querySelector('[data-close]');
  if (c) c.click(); else top.classList.add('hidden');
});

// A read-only dialog also closes on a click beside it. Forms do not: a stray
// click must not throw away what was typed. Both ends of the click have to be
// on the backdrop, or selecting text and releasing outside would close it.
for (const m of document.querySelectorAll('.modal[data-light]')) {
  let down = false;
  m.addEventListener('mousedown', e => { down = e.target === m; });
  m.addEventListener('click', e => {
    if (e.target !== m || !down) return;
    const c = m.querySelector('[data-close]');
    if (c) c.click(); else m.classList.add('hidden');
  });
}

/* ------------------------------------------------------------------ menus */

let openMenu = null;
function closeMenu() {
  if (!openMenu) return;
  openMenu.remove();
  openMenu = null;
}

// showMenu drops a list of choices under anchor. Clicking the anchor again
// closes it; so does Escape, a click elsewhere or a resize.
function showMenu(anchor, { title, items }) {
  if (openMenu && openMenu._anchor === anchor) { closeMenu(); return; }
  closeMenu();
  const m = el('div', 'menu');
  m.setAttribute('role', 'menu');
  m._anchor = anchor;
  if (title) m.appendChild(el('div', 'menu-h', title));
  for (const it of items) {
    const b = el('button', 'menu-item');
    b.setAttribute('role', 'menuitem');
    b.appendChild(icon(it.icon || 'check', it.icon || it.checked ? '' : 'blank'));
    const tx = el('div', 'mtext');
    tx.appendChild(el('div', null, it.label));
    if (it.detail) tx.appendChild(el('small', null, it.detail));
    b.appendChild(tx);
    b.disabled = !!it.disabled;
    if (it.title) b.title = it.title;
    b.addEventListener('click', () => { closeMenu(); if (it.onSelect) it.onSelect(); });
    m.appendChild(b);
  }
  document.body.appendChild(m);
  const r = anchor.getBoundingClientRect();
  const left = Math.max(8, Math.min(r.left, innerWidth - m.offsetWidth - 8));
  let top = r.bottom + 4;
  if (top + m.offsetHeight > innerHeight - 8) top = Math.max(8, r.top - m.offsetHeight - 4);
  m.style.left = left + 'px';
  m.style.top = top + 'px';
  openMenu = m;
  const first = m.querySelector('.menu-item:not(:disabled)');
  if (first) first.focus();
  m.addEventListener('keydown', e => {
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    e.preventDefault();
    const all = [...m.querySelectorAll('.menu-item:not(:disabled)')];
    const i = all.indexOf(document.activeElement);
    const next = all[(i + (e.key === 'ArrowDown' ? 1 : all.length - 1)) % all.length];
    if (next) next.focus();
  });
}
document.addEventListener('mousedown', e => {
  if (!openMenu) return;
  if (openMenu.contains(e.target) || openMenu._anchor.contains(e.target)) return;
  closeMenu();
}, true);
window.addEventListener('resize', closeMenu);

/* ------------------------------------------------------------------ fleet */

// A task lives on one computer; its API is reached through this computer's
// server, which forwards to the owner. '' or this computer's own name means here.
function isLocal(node) { return !node || node === state.fleet.self; }
function nodeBase(node) { return isLocal(node) ? '/api' : '/api/node/' + encodeURIComponent(node); }
function tb() { return nodeBase(state.currentNode); }

// The open task's computer decides its profiles, IDEs and ledger, not this one.
function taskCfg() {
  return (isLocal(state.currentNode) ? state.config : state.taskConfig) || {};
}

async function loadFleet() {
  try { state.fleet = Object.assign({ peers: [], mirrors: [], state: {} }, await api.get('/api/fleet')); }
  catch { state.fleet = { enabled: false, self: '', peers: [], mirrors: [], state: {} }; }
  $('#nav-computers').classList.toggle('hidden', !state.fleet.enabled);
  const bar = $('#fleetbar');
  const st = state.fleet.state || {};
  if (state.fleet.enabled && st.phase === 'needs-login') {
    bar.className = 'envbar bad';
    bar.innerHTML = '';
    bar.appendChild(el('b', null, 'Sign this computer in to your tailnet to link it with your other computers: '));
    if (st.authUrl) {
      const a = el('a', null, st.authUrl);
      a.href = st.authUrl; a.target = '_blank';
      bar.appendChild(a);
    }
  } else {
    bar.className = 'envbar hidden';
  }
  return state.fleet;
}
function onlinePeers() { return (state.fleet.peers || []).filter(p => p.online); }

/* ------------------------------------------------------------------ views */

function showView(name) {
  state.navigated = true;
  closeMenu();
  for (const v of document.querySelectorAll('.view')) v.classList.remove('active');
  $('#' + name).classList.add('active');
  // A task is opened from the board, so the board stays lit while one is open.
  $('#nav-board').classList.toggle('active', name === 'board' || name === 'task');
  $('#nav-orch').classList.toggle('active', name === 'orch');
  $('#nav-inbox').classList.toggle('active', name === 'inbox');
  $('#nav-reviews').classList.toggle('active', name === 'reviews');
  $('#nav-instances').classList.toggle('active', name === 'instances');
  $('#nav-settings').classList.toggle('active', name === 'settings');
  $('#nav-computers').classList.toggle('active', name === 'computers');
}

/* ------------------------------------------------------------------ agents */

// The New task modal, the terminal tab button and the permission badge all
// describe the same agent, so the descriptions are fetched once and cached.
// Re-fetched whenever the modal opens: a CLI installed since the server started
// must become selectable without restarting it.
async function loadAgents(base = '/api') {
  const d = await api.get(base + '/agents');
  state.agents = d.agents || [];
  state.agentDefault = d.default || 'claude';
  return d;
}

function agentInfo(kind) {
  return state.agents.find(a => a.kind === kind) || {
    kind: kind || 'claude', label: kind || 'claude', installed: true,
    safeLabel: 'default', dangerLabel: 'skip-permissions', dangerFlag: '',
    resumable: (kind || 'claude') === 'claude',
  };
}

/* ------------------------------------------------------------------ board */

function renderSwitcher() {
  const host = $('#switcher');
  // Done tasks are finished work — keeping them here would defeat the purpose
  const live = state.tasks.filter(t => t.status !== 'done' && !t.hidden);
  const key = JSON.stringify([state.current, state.currentNode, live.map(t =>
    [t.slug, t.node, t.name, t.status, !!t.attention, !!(t.stack && t.stack.up)])]);
  if (key === state.switcherKey) return;
  state.switcherKey = key;
  host.innerHTML = '';
  if (!live.length) { host.appendChild(el('span', 'empty', 'no open tasks')); fadeSwitcher(); return; }
  for (const t of live) {
    const here = t.slug === state.current && (isLocal(t.node) ? isLocal(state.currentNode) : t.node === state.currentNode);
    const chip = el('button', 'chip' + (here ? ' active' : '') +
                             (t.attention ? ' attn' : ''));
    if (t.attention) chip.appendChild(el('span', 'dot attn'));
    else if (t.stack && t.stack.up) chip.appendChild(el('span', 'dot stack'));
    chip.appendChild(el('span', null, t.name));
    chip.title = t.attention ? `${t.agent || 'the agent'} is waiting for input`
               : (t.stack && t.stack.up ? 'stack running' : t.status.replace('_', ' '));
    chip.addEventListener('click', () => openTask(t.slug, t.node));
    host.appendChild(chip);
  }
  fadeSwitcher();
}

// The switcher fades out at its right edge only when there is more to scroll
// to; on a short list the fade would just eat the last chip's end.
function fadeSwitcher() {
  const host = $('#switcher');
  host.classList.toggle('overflowing', host.scrollWidth > host.clientWidth + 1);
}
window.addEventListener('resize', fadeSwitcher);

async function refreshTasks() {
  await loadFleet();
  state.tasks = await api.get('/api/fleet/tasks');
  renderSwitcher();
  return state.tasks;
}

// renderColumns is the single place that decides what belongs on the board.
// The background refresh calls it too: when this was duplicated there, the copy
// did not know about hidden tasks and put them straight back three seconds
// after they were hidden.
//
// It redraws only when something changed, and never mid-drag: rebuilding the
// cards every few seconds dropped the card being dragged out from under the
// pointer and flickered every hover.
function renderColumns(tasks, force) {
  const key = JSON.stringify([state.showHidden, state.fleet.enabled, state.fleet.self, tasks]);
  if (!force && (state.dragging || key === state.boardKey)) return;
  state.boardKey = key;
  const hidden = tasks.filter(t => t.hidden);
  $('#hidden-count').textContent = hidden.length ? `${hidden.length} hidden` : 'nothing hidden';
  const tog = $('#toggle-hidden');
  setLabel(tog, state.showHidden ? 'Back to the board' : 'Show hidden');
  tog.querySelector('use').setAttribute('href', state.showHidden ? '#i-arrow-left' : '#i-eye');
  tog.disabled = !hidden.length && !state.showHidden;
  $('#hidden-banner').classList.toggle('hidden', !state.showHidden);
  const visible = tasks.filter(t => !t.hidden);
  for (const s of ['open', 'in_progress', 'done']) {
    const col = $('#col-' + s);
    col.innerHTML = '';
    // Reviewing hidden tasks shows ONLY those, so unhiding is a short list to
    // work through rather than hunting them among the visible ones.
    const items = tasks.filter(t => t.status === s && (state.showHidden ? t.hidden : !t.hidden));
    $('#count-' + s).textContent = items.length;
    for (const t of items) col.appendChild(card(t));
    if (!items.length) col.appendChild(emptyColumn(s, visible.length));
  }
}

function emptyColumn(status, visibleCount) {
  const box = el('div', 'cards-empty');
  if (state.showHidden) { box.textContent = 'No hidden tasks here'; return box; }
  if (status === 'open' && !visibleCount) {
    box.appendChild(el('div', null, 'No tasks yet.'));
    const b = el('button', 'ghost sm');
    b.style.marginTop = '10px';
    b.appendChild(icon('plus'));
    b.appendChild(el('span', null, 'Create a task'));
    b.addEventListener('click', () => $('#btn-new').click());
    box.appendChild(b);
    return box;
  }
  box.textContent = { open: 'Nothing open', in_progress: 'Nothing in progress', done: 'Nothing done yet' }[status];
  return box;
}

async function loadBoard() {
  state.tasks = await api.get('/api/fleet/tasks');
  renderSwitcher();
  renderColumns(state.tasks, true);
  // Cached server-side, so this is a local round-trip unless the probe is stale.
  loadEnv(false);
}

function card(t) {
  const c = el('div', 'card' + (t.hidden ? ' hidden-task' : ''));
  c.draggable = true;
  c.tabIndex = 0;
  c.setAttribute('role', 'button');
  c.setAttribute('aria-label', 'Open ' + t.name);
  c.dataset.slug = t.slug;

  const top = el('div', 'card-top');
  top.appendChild(el('h3', null, t.name));
  const acts = el('div', 'card-actions');
  const hb = el('button', 'iconbtn xs');
  hb.appendChild(icon(t.hidden ? 'eye' : 'eye-off'));
  hb.setAttribute('aria-label', t.hidden ? 'Unhide' : 'Hide');
  hb.title = t.hidden
    ? 'Put this task back on the board'
    : 'Hide: take this task off the board. Nothing is deleted: worktrees, branches and any running stack stay.';
  hb.addEventListener('click', async ev => {
    ev.stopPropagation();
    hb.disabled = true;
    try { await api.put(`${nodeBase(t.node)}/tasks/${t.slug}/hidden`, { hidden: !t.hidden }); }
    catch (e) { toast(e.message); }
    await loadBoard();
  });
  acts.appendChild(hb);
  const db = el('button', 'iconbtn xs danger');
  db.appendChild(icon('trash'));
  db.setAttribute('aria-label', 'Delete');
  db.title = 'Delete this task AND its workspace folder, including every worktree in it';
  db.addEventListener('click', ev => { ev.stopPropagation(); deleteTask(t, () => loadBoard()); });
  acts.appendChild(db);
  top.appendChild(acts);
  c.appendChild(top);

  const meta = el('div', 'meta');
  if (state.fleet.enabled) {
    const n = el('span', 'pill node' + (isLocal(t.node) ? ' here' : ''));
    n.appendChild(icon('monitor'));
    n.appendChild(document.createTextNode(isLocal(t.node) ? 'this computer' : t.node));
    meta.appendChild(n);
  }
  for (const r of (t.repos || [])) meta.appendChild(el('span', 'pill', r.name));
  c.appendChild(meta);

  if (t.stack && t.stack.up) {
    const foot = el('div', 'card-foot');
    const run = el(t.stack.entryUrl && isLocal(t.node) ? 'a' : 'span', 'run');
    run.appendChild(el('span', 'dot stack'));
    run.appendChild(el('span', null, t.stack.entryUrl
      ? t.stack.entryUrl.replace(/^https?:\/\//, '')
      : `running ${t.stack.running}/${t.stack.total}`));
    if (run.tagName === 'A') {
      run.href = t.stack.entryUrl; run.target = '_blank'; run.title = 'Open the app';
      run.addEventListener('click', ev => ev.stopPropagation());
    }
    foot.appendChild(run);
    c.appendChild(foot);
  }
  if (t.attention) {
    const row = el('div', 'attnrow');
    row.appendChild(el('span', 'dot attn'));
    row.appendChild(el('span', null, `${t.agent || 'the agent'} is waiting for input`));
    c.appendChild(row);
  }
  for (const [repo, st] of Object.entries(t.setup || {})) {
    if (st.startsWith('running')) c.appendChild(el('div', 'instrow', `${repo}: setup ${st}…`));
    else if (st.startsWith('failed')) c.appendChild(el('div', 'instrow err', `${repo}: setup ${st}`));
  }

  c.addEventListener('click', () => openTask(t.slug, t.node));
  c.addEventListener('keydown', e => {
    if ((e.key === 'Enter' || e.key === ' ') && e.target === c) { e.preventDefault(); openTask(t.slug, t.node); }
  });
  c.addEventListener('dragstart', e => {
    state.dragging = true;
    c.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', JSON.stringify({ slug: t.slug, node: t.node || '', status: t.status }));
  });
  c.addEventListener('dragend', () => {
    state.dragging = false;
    c.classList.remove('dragging');
    for (const col of document.querySelectorAll('.column.dragover')) col.classList.remove('dragover');
  });
  return c;
}

for (const col of document.querySelectorAll('.column')) {
  col.addEventListener('dragover', e => { e.preventDefault(); col.classList.add('dragover'); });
  // dragleave also fires when the pointer moves onto a card inside the column;
  // only leaving the column itself clears the highlight.
  col.addEventListener('dragleave', e => {
    if (!col.contains(e.relatedTarget)) col.classList.remove('dragover');
  });
  col.addEventListener('drop', async e => {
    e.preventDefault();
    col.classList.remove('dragover');
    state.dragging = false;
    let ref = {};
    try { ref = JSON.parse(e.dataTransfer.getData('text/plain')); } catch { return; }
    if (!ref.slug || ref.status === col.dataset.status) return;
    await setStatus(ref.slug, col.dataset.status, ref.node);
    await loadBoard();
  });
}

async function setStatus(slug, status, node) {
  try { await api.put(`${nodeBase(node)}/tasks/${slug}/status`, { status }); }
  catch (err) { toast(err.message); }
}

/* ------------------------------------------------------------------ task detail */

function findTask(slug, node) {
  return state.tasks.find(x => x.slug === slug && (isLocal(x.node) ? isLocal(node) : x.node === node));
}

// clearTaskView blanks what belongs to the previous task, so its stack, files
// and notes are not on screen under the new task's name while it loads.
function clearTaskView(slug, node) {
  const t = findTask(slug, node);
  $('#task-title').textContent = t ? t.name : slug;
  for (const id of ['#stack-entry', '#stack-db', '#stack-warnings', '#files-list', '#files-path',
                    '#repo-list', '#jr-list', '#idea-list', '#mem-head', '#ide-choices']) {
    $(id).innerHTML = '';
  }
  $('#stack-table').querySelector('tbody').innerHTML = '';
  $('#git-body').textContent = '—';
  $('#usage-body').textContent = '—';
  $('#stack-log').classList.add('hidden');
  $('#install-state').className = 'hidden';
  $('#file-view').classList.add('hidden');
  $('#term-host').classList.remove('hidden');
  $('#term-empty').classList.add('hidden');
  $('#resume-list').classList.add('hidden');
  state.resumable = [];
  if (typeof reviewReset === 'function') reviewReset();
}

async function openTask(slug, node) {
  state.gen++;
  state.current = slug;
  state.currentNode = isLocal(node) ? '' : node;
  state.filePath = '';
  state.openFile = '';
  state.viaIde.clear();
  stopOrch(); // reachable from the orchestrator view via the task switcher
  stopInbox(); // and from the inbox, through a thread's task
  stopPolling();
  disposeTerms();
  clearTaskView(slug, state.currentNode);
  showView('task');
  renderSwitcher();
  const p = pin();
  state.taskConfig = isLocal(p.node) ? null
    : await api.get(p.base + '/config').catch(() => null);
  if (stale(p)) return;
  await loadGitIds(p.base);
  if (stale(p)) return;
  try {
    if (!(await refreshTask(p))) return;
  } catch (e) {
    if (stale(p)) return;
    // Deleted elsewhere, or its computer went away: say so on the board rather
    // than leaving a half-drawn task behind.
    toast(`Could not open ${slug}: ${e.message}`);
    await goBoard();
    return;
  }
  // each of these is independent — one failing must not abandon the others
  loadFiles('', p).catch(e => console.error('files:', e));
  loadMem(p).catch(e => console.error('mem:', e));
  loadUsage(p).catch(e => console.error('usage:', e));
  loadJournal(p).catch(e => console.error('journal:', e));
  loadIdea(p).catch(e => console.error('idea:', e));
  loadReview(p).catch(e => console.error('review:', e));
  try { await syncTerminals(p); } catch (e) { console.error('terminals:', e); }
  if (stale(p)) return;
  startPolling();
}

async function refreshTask(p = pin()) {
  const t = await api.get(`${p.base}/tasks/${p.slug}`);
  if (stale(p)) return null;
  $('#task-title').textContent = t.name;
  $('#task-title').title = t.name;
  for (const b of document.querySelectorAll('[data-set-status]')) {
    b.classList.toggle('on', b.dataset.setStatus === t.status);
  }
  if (!state.agents.length) { try { await loadAgents(); } catch {} }
  state.taskAgent = t.agent || 'claude';
  const ag = agentInfo(state.taskAgent);
  const newagent = $('#btn-newagent');
  setLabel(newagent, state.taskAgent);
  newagent.title = `New ${ag.label} session in the workspace`;
  setLabel($('#empty-agent'), `Start ${ag.label}`);
  // The profile is a property of the task, but it only takes effect at the
  // next stack up: the running stack was exec'd with the old commands, which
  // cannot be changed underneath it. The badge says so rather than implying it
  // switched.
  const modeBadge = $('#task-mode-badge');
  const cfg = taskCfg();
  const profiles = cfg.profiles || [];
  const curProfile = (t.stack && t.stack.profile) || t.profile || cfg.defaultProfile || '';
  setLabel(modeBadge, curProfile || '?');
  modeBadge.classList.toggle('hidden', profiles.length === 0);
  modeBadge.onclick = () => {
    showMenu(modeBadge, {
      title: profiles.length < 2 ? 'The only profile in config.json' : 'Boot this task with',
      items: profiles.map(pr => ({
        label: pr.name + (pr.name === cfg.defaultProfile ? ' (default)' : ''),
        detail: pr.description || (pr.services || []).join(', '),
        checked: pr.name === curProfile,
        onSelect: async () => {
          if (pr.name === curProfile || stale(p)) return;
          try {
            await api.put(`${p.base}/tasks/${p.slug}/profile`, { profile: pr.name });
          } catch (e) { toast(e.message); return; }
          await refreshTask(p);
          if (t.stack && t.stack.up) {
            toast('Switched to ' + pr.name + '. The running stack keeps the profile it started with — ' +
                  'stop and start it to pick this up.', 'info', 9000);
          }
        },
      })),
    });
  };

  renderGitBadge(t, p);
  renderNodeBadge(t);

  const perm = $('#task-perm-badge');
  setLabel(perm, t.dangerous ? ag.dangerLabel : ag.safeLabel);
  perm.querySelector('use').setAttribute('href', t.dangerous ? '#i-shield-alert' : '#i-shield');
  perm.classList.remove('hidden');
  perm.classList.toggle('danger', !!t.dangerous);
  perm.title = 'Permission mode for NEW agent sessions — click to switch ' +
               '(a running one keeps the mode it started with)';
  perm.onclick = async () => {
    const next = !t.dangerous;
    if (next && !await ask({
      title: `Skip permission checks for ${ag.label}?`,
      body: `Future ${ag.label} sessions in this task start with ${ag.dangerFlag}. ` +
            'The agent will run anything in this workspace without asking.',
      ok: 'Skip permissions', danger: true, icon: 'shield-alert',
    })) return;
    if (stale(p)) return;
    try { await api.put(`${p.base}/tasks/${p.slug}/permissions`, { dangerous: next }); }
    catch (e) { toast(e.message); return; }
    await refreshTask(p);
  };
  renderStack(t.stack);
  renderIdeChoices(t);
  const inst = $('#install-state');
  const setups = Object.entries(t.setup || {}).filter(([, st]) => st !== 'done');
  if (setups.length) {
    inst.className = setups.some(([, st]) => st.startsWith('failed')) ? 'instrow err' : 'instrow';
    inst.textContent = setups.map(([repo, st]) => `${repo}: setup ${st}`).join(' · ') +
      ' — services start automatically when it finishes';
  } else {
    inst.className = 'hidden';
    inst.textContent = '';
  }
  // Git status is the slowest call here, so the rest of the view does not
  // wait for it; the repos panel fills in when it answers.
  renderRepos(t, null);
  loadGit(p).then(stats => { if (!stale(p)) renderRepos(t, stats); })
    .catch(e => console.error('git:', e));
  return t;
}

// svcState names the dot beside a service: up, on its way, broken, not part
// of this task, or simply not running.
function svcState(sv) {
  if (sv.running) return 'up';
  if (sv.mode === 'absent' || sv.mode === 'off') return 'off';
  const n = sv.note || '';
  if (/^(starting|setup running)/.test(n)) return 'busy';
  if (/^(exited|not started|setup failed)/.test(n)) return 'fail';
  return '';
}

function renderStack(s) {
  const body = $('#stack-table').querySelector('tbody');
  body.innerHTML = '';
  const entry = $('#stack-entry');
  entry.innerHTML = '';
  if (s && s.error) {
    entry.appendChild(el('div', 'warn', s.error));
  } else if (!s || !s.k) {
    entry.appendChild(el('span', 'hint', 'Not running — press Start.'));
  } else if (s.entryUrl && !isLocal(state.currentNode)) {
    // The stack runs on another computer. Its URLs name that computer's
    // localhost; "Open here" serves the same ports on this one's.
    const node = state.currentNode, slug = state.current;
    if (mirroredHere(node, slug)) {
      entry.appendChild(entryLink(s.entryUrl, 'served from ' + node));
      const stop = el('button', 'mini', 'stop opening here');
      stop.addEventListener('click', async () => {
        try { await api.del(`/api/fleet/mirror?node=${encodeURIComponent(node)}&task=${encodeURIComponent(slug)}`); }
        catch (e) { toast(e.message); }
        await loadFleet();
        renderStack(s);
      });
      entry.appendChild(stop);
    } else {
      const open = el('button', 'primary mini', 'Open here');
      open.title = `Serve this stack's ports on this computer's localhost, so ${s.entryUrl} works here as it does on ${node}`;
      open.addEventListener('click', async () => {
        open.disabled = true;
        try {
          const r = await api.post('/api/fleet/mirror', { node, task: slug });
          if (r.problems && r.problems.length) toast('Some ports could not be opened here:\n' + r.problems.join('\n'));
          await loadFleet();
          renderStack(s);
          if (r.entryUrl) window.open(r.entryUrl, '_blank');
        } catch (e) { toast(e.message); open.disabled = false; }
      });
      entry.appendChild(open);
      entry.appendChild(el('span', 'hint', `running on ${node}`));
    }
  } else if (s.entryUrl) {
    entry.appendChild(entryLink(s.entryUrl, 'open the app'));
  } else if (s.up) {
    entry.appendChild(el('span', 'hint', 'Starting — the app link appears once its entry service listens.'));
  }
  for (const sv of (s && s.services) || []) {
    const tr = el('tr');
    const nm = el('td', 'svc');
    const line = el('div', 'svcname');
    const st = svcState(sv);
    const dot = el('span', 'svcdot ' + st);
    dot.title = { up: 'running', busy: 'starting', fail: 'not running', off: 'not part of this task' }[st] || 'not running';
    line.appendChild(dot);
    line.appendChild(el('span', null, sv.name));
    line.appendChild(el('small', 'svcmode ' + sv.mode, sv.mode));
    nm.appendChild(line);
    if (sv.note) nm.appendChild(el('div', 'svcnote', sv.note));
    tr.appendChild(nm);
    tr.appendChild(el('td', 'port', sv.port ? ':' + sv.port : ''));
    const stc = el('td', 'state');
    if (sv.log) {
      const lb = el('button', 'mini', 'log');
      lb.title = 'Show the end of this service’s log';
      lb.addEventListener('click', () => showStackLog(sv.name));
      stc.appendChild(lb);
    }
    tr.appendChild(stc);
    body.appendChild(tr);
  }
  const db = $('#stack-db');
  db.innerHTML = '';
  for (const sv of (s && s.services) || []) {
    for (const [k, v] of Object.entries(sv.facts || {})) {
      db.appendChild(el('div', null, `${sv.name} ${k}: ${v}`));
    }
  }
  const warns = $('#stack-warnings');
  warns.innerHTML = '';
  for (const w of (s && s.warnings) || []) warns.appendChild(el('div', 'warn', w));
}

function entryLink(url, what) {
  const a = el('a', 'entrylink');
  a.href = url; a.target = '_blank';
  a.title = what;
  a.appendChild(el('span', 'dot stack'));
  a.appendChild(el('span', null, url.replace(/^https?:\/\//, '')));
  a.appendChild(icon('external'));
  return a;
}

function renderNodeBadge(t) {
  const badge = $('#task-node-badge');
  const mv = $('#btn-move');
  const on = state.fleet.enabled;
  badge.classList.toggle('hidden', !on);
  badge.innerHTML = '';
  badge.appendChild(icon('monitor'));
  badge.appendChild(el('span', null, isLocal(state.currentNode) ? (state.fleet.self || 'this computer') : state.currentNode));
  badge.classList.toggle('remote', !isLocal(state.currentNode));
  mv.classList.toggle('hidden', !on || moveTargets().length === 0);
}

// Where the open task can go: every online computer but the one it is on.
function moveTargets() {
  const out = onlinePeers().map(p => p.name).filter(n => n !== state.currentNode);
  if (!isLocal(state.currentNode)) out.unshift(state.fleet.self);
  return out;
}

$('#btn-move').addEventListener('click', async () => {
  await loadFleet();
  const host = $('#move-targets');
  host.innerHTML = '';
  const targets = moveTargets();
  targets.forEach((name, i) => {
    const row = el('label', 'agent-pick');
    const rb = el('input');
    rb.type = 'radio';
    rb.name = 'moveto';
    rb.value = name;
    rb.checked = i === 0;
    row.appendChild(rb);
    row.appendChild(el('div', null, name === state.fleet.self ? name + ' (this computer)' : name));
    host.appendChild(row);
  });
  const t = findTask(state.current, state.currentNode);
  $('#move-stack').checked = !!(t && t.stack && t.stack.up);
  $('#move-agent').checked = true;
  $('#move-status').textContent = '';
  $('#move-go').disabled = targets.length === 0;
  openModal('movepop');
});
$('#move-cancel').addEventListener('click', () => $('#movepop').classList.add('hidden'));
$('#move-go').addEventListener('click', async () => {
  const to = (document.querySelector('input[name=moveto]:checked') || {}).value;
  if (!to) return;
  const b = $('#move-go');
  const p = pin();
  b.disabled = true;
  $('#move-status').textContent = `Stopping the agent and the stack, then sending the task to ${to}…`;
  try {
    const r = await api.post(`${p.base}/tasks/${p.slug}/move`, {
      to, startAgent: $('#move-agent').checked, startStack: $('#move-stack').checked,
    });
    $('#movepop').classList.add('hidden');
    await loadBoard();
    await openTask(p.slug, to === state.fleet.self ? '' : to);
    toast(`Moved to ${to}.`, 'ok');
    if (r.notes && r.notes.length) console.log('moved:', r.notes.join('\n'));
  } catch (e) {
    $('#move-status').textContent = e.message;
  }
  b.disabled = false;
});

/* ------------------------------------------------------------------ review

   Review starts a reviewer for the task's merge requests, in a tab of its own,
   with the agent, model and effort picked here. The choices are remembered
   per browser, the model and effort per agent. */

const review = { agents: [], kind: '', models: new Map() };
function reviewPrefs() {
  try { return JSON.parse(localStorage.getItem('fride.review')) || {}; } catch { return {}; }
}
function saveReviewPrefs(pr) {
  try { localStorage.setItem('fride.review', JSON.stringify(pr)); } catch {}
}

$('#reviewpop').addEventListener('toggle', async e => {
  if (e.newState !== 'open') return;
  const pop = e.currentTarget, r = $('#btn-review').getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(r.right - pop.offsetWidth, innerWidth - pop.offsetWidth - 8)) + 'px';
  pop.style.top = (r.bottom + 4) + 'px';
  // The task's own computer runs the reviewer, so its CLIs are the ones offered.
  try { review.agents = (await api.get(tb() + '/agents')).agents || []; }
  catch (err) { toast(err.message); return; }
  const pr = reviewPrefs();
  const usable = k => review.agents.some(a => a.kind === k && a.installed);
  review.kind = [pr.last, state.taskAgent].find(usable) || (review.agents.find(a => a.installed) || {}).kind || '';
  const host = $('#review-agents');
  host.innerHTML = '';
  for (const a of review.agents) {
    const row = el('label', 'agent-pick' + (a.installed ? '' : ' missing'));
    const rb = el('input');
    rb.type = 'radio';
    rb.name = 'review-agent';
    rb.disabled = !a.installed;
    rb.checked = a.kind === review.kind;
    rb.addEventListener('change', () => { review.kind = a.kind; renderReviewChoices(); });
    row.appendChild(rb);
    const nm = el('div');
    nm.appendChild(document.createTextNode(a.label));
    if (!a.installed) nm.appendChild(el('small', null, a.detail || 'not installed'));
    row.appendChild(nm);
    host.appendChild(row);
  }
  renderReviewChoices();
});

function renderReviewChoices() {
  const a = review.agents.find(x => x.kind === review.kind) || {};
  const mine = (reviewPrefs().byAgent || {})[review.kind] || {};
  $('#review-model').value = mine.model || '';
  const sel = $('#review-effort');
  sel.innerHTML = '';
  for (const v of ['', ...(a.efforts || [])]) {
    const o = el('option', null, v || 'the CLI\'s default');
    o.value = v;
    sel.appendChild(o);
  }
  sel.value = (a.efforts || []).includes(mine.effort) ? mine.effort : '';
  sel.disabled = !(a.efforts || []).length;
  $('#review-go').disabled = !a.installed;
  fillReviewModels(review.kind);
}

// Suggestions only; any name the CLI accepts can be typed. Asking can take
// seconds (one CLI asks its server), so the answer is kept per computer.
async function fillReviewModels(kind) {
  const list = $('#review-models');
  list.innerHTML = '';
  const key = tb() + '|' + kind;
  if (!review.models.has(key)) {
    review.models.set(key, api.get(`${tb()}/agents/models?kind=${encodeURIComponent(kind)}`)
      .then(d => d.models || []).catch(() => []));
  }
  const models = await review.models.get(key);
  if (review.kind !== kind) return;
  for (const m of models) { const o = el('option'); o.value = m; list.appendChild(o); }
}

$('#review-go').addEventListener('click', async e => {
  const b = e.currentTarget, p = pin();
  const body = { agent: review.kind, model: $('#review-model').value.trim(), effort: $('#review-effort').value };
  b.disabled = true;
  try {
    const s = await api.post(`${p.base}/tasks/${p.slug}/review`, body);
    const pr = reviewPrefs();
    pr.last = body.agent;
    pr.byAgent = Object.assign(pr.byAgent || {}, { [body.agent]: { model: body.model, effort: body.effort } });
    saveReviewPrefs(pr);
    $('#reviewpop').hidePopover();
    if (!stale(p)) { attachTerm(s, p); selectTerm(s.id); }
  } catch (err) { toast(err.message); }
  b.disabled = false;
});

function mirroredHere(node, task) {
  return (state.fleet.mirrors || []).some(m => m.node === node && m.task === task);
}

async function showStackLog(name) {
  const p = pin();
  const box = $('#stack-log');
  $('#stack-log-name').textContent = name + ' — last 200 lines';
  box.classList.remove('hidden');
  const pre = $('#stack-log-body');
  try {
    const r = await api.get(`${p.base}/tasks/${p.slug}/stack/log?service=${encodeURIComponent(name)}&lines=200`);
    if (stale(p)) return;
    pre.textContent = r.log || '(empty)';
    pre.scrollTop = pre.scrollHeight;
  } catch (e) { if (!stale(p)) pre.textContent = e.message; }
}
$('#stack-log-close').addEventListener('click', () => $('#stack-log').classList.add('hidden'));

function renderRepos(t, stats) {
  const host = $('#repo-list');
  host.innerHTML = '';
  const byRepo = {};
  for (const s of stats || []) byRepo[s.repo] = s;
  for (const r of t.repos || []) {
    const st = byRepo[r.name];
    const row = el('div', 'repo-row');
    row.appendChild(el('span', null, r.name));
    const right = el('span', 'meta2');
    const branch = (t.branches && t.branches[r.name]) || r.branch;
    const br = el('span', 'br');
    br.appendChild(icon('branch'));
    br.appendChild(document.createTextNode(branch));
    br.title = branch;
    right.appendChild(br);
    if (st && (st.added || st.deleted)) {
      const ch = el('span', 'churn');
      ch.appendChild(el('span', 'add', '+' + st.added));
      ch.appendChild(document.createTextNode(' '));
      ch.appendChild(el('span', 'del', '−' + st.deleted));
      ch.title = 'lines changed since ' + (st.baseRef || 'the base') +
                 ' (committed and uncommitted)';
      right.appendChild(ch);
    }
    row.appendChild(right);
    host.appendChild(row);
  }
}

// --- Uncommitted card -------------------------------------------------------
function statusClass(code) {
  if (code.includes('?')) return 'new';
  if (code.includes('D')) return 'del';
  if (code.includes('A')) return 'new';
  return 'mod';
}

async function loadGit(p = pin()) {
  const host = $('#git-body');
  let stats;
  try { stats = await api.get(`${p.base}/tasks/${p.slug}/git`); }
  catch (e) { if (!stale(p)) host.textContent = e.message; return null; }
  if (stale(p)) return stats;
  host.innerHTML = '';
  let any = false;
  for (const st of stats || []) {
    if (!st.changed || !st.changed.length) continue;
    any = true;
    const box = el('div', 'grepo');
    box.appendChild(el('div', 'gname', `${st.repo} · ${st.changed.length} file${st.changed.length === 1 ? '' : 's'}`));
    for (const f of st.changed.slice(0, 12)) {
      const row = el('div', 'gfile');
      row.appendChild(el('span', 'st ' + statusClass(f.status), f.status.trim() || '?'));
      const pth = el('span', 'pth', f.path);
      pth.title = f.path;
      row.appendChild(pth);
      box.appendChild(row);
    }
    if (st.changed.length > 12) {
      box.appendChild(el('div', 'clean', `…and ${st.changed.length - 12} more`));
    }
    host.appendChild(box);
  }
  if (!any) {
    const c = el('div', 'clean');
    c.appendChild(icon('check'));
    c.appendChild(el('span', null, 'All repos clean'));
    host.appendChild(c);
  }
  return stats;
}

$('#git-refresh').addEventListener('click', async ev => {
  ev.stopPropagation();
  const p = pin();
  const stats = await loadGit(p);
  try {
    const t = await api.get(`${p.base}/tasks/${p.slug}`);
    if (!stale(p)) renderRepos(t, stats);
  } catch (e) { toast(e.message); }
});

const fmt = n => (n || 0).toLocaleString();

async function loadUsage(p = pin()) {
  const host = $('#usage-body');
  host.textContent = 'loading…';
  let u;
  try { u = await api.get(`${p.base}/tasks/${p.slug}/usage`); }
  catch (e) { if (!stale(p)) host.textContent = e.message; return; }
  if (stale(p)) return;
  renderResumable(p).catch(e => console.error('resumable:', e));
  if (!u || !u.messages) {
    // Token counts are read out of Claude Code's own transcripts, so for a task
    // running another CLI the panel is empty by construction, not by accident.
    host.textContent = state.taskAgent === 'claude'
      ? 'No Claude activity recorded for this workspace yet.'
      : `Token counts come from Claude Code transcripts — this task runs ${agentInfo(state.taskAgent).label}.`;
    return;
  }
  const rows = [
    ['input', u.inputTokens], ['output', u.outputTokens],
    ['cache read', u.cacheReadTokens], ['cache write', u.cacheCreationTokens],
  ];
  const tbl = el('table');
  for (const [k, v] of rows) {
    const tr = el('tr');
    tr.appendChild(el('td', 'k', k));
    tr.appendChild(el('td', 'n', fmt(v)));
    tbl.appendChild(tr);
  }
  const tot = el('tr', 'tot');
  tot.appendChild(el('td', 'k', 'total'));
  tot.appendChild(el('td', 'n', fmt(u.total)));
  tbl.appendChild(tot);
  host.innerHTML = '';
  host.appendChild(tbl);
  const sub = el('div', 'sub',
    `${u.messages} messages · ${u.sessions} session${u.sessions === 1 ? '' : 's'}` +
    (u.models && u.models.length ? ' · ' + u.models.join(', ') : ''));
  host.appendChild(sub);
  if (u.lastActivity) {
    host.appendChild(el('div', 'sub', 'last activity ' + new Date(u.lastActivity).toLocaleString()));
  }
}

$('#usage-refresh').addEventListener('click', ev => { ev.stopPropagation(); loadUsage(); });

// The task poll also keeps the tabs honest: which agent is waiting for you,
// which is working, and sessions opened from somewhere else (the orchestrator,
// another browser) — before, those only appeared after reopening the task.
function startPolling() {
  stopPolling();
  const p = pin();
  state.poll = setInterval(async () => {
    if (stale(p) || !$('#task').classList.contains('active')) return;
    try {
      const s = await api.get(`${p.base}/tasks/${p.slug}/stack`);
      if (stale(p)) return;
      renderStack(s);
      // While a setup is in flight the task view carries the phase, so keep
      // the detail panel honest without a full re-render.
      const inst = $('#install-state');
      if (inst && !inst.classList.contains('hidden')) await refreshTask(p);
      await syncTerminals(p);
    } catch { /* task may have been deleted */ }
  }, 4000);
}
function stopPolling() { if (state.poll) { clearInterval(state.poll); state.poll = null; } }

// The switcher must stay live on every view — the whole point is to notice a
// task that needs you while you are looking at a different one.
setInterval(async () => {
  if (document.body.classList.contains('unconfigured')) return;
  try {
    const tasks = await refreshTasks();
    if ($('#board').classList.contains('active')) renderColumns(tasks);
  } catch { /* server restarting */ }
}, 3000);

// A prerequisite can break mid-session (docker stopped, a token expired), so
// this does not wait for the board to be reloaded. The server caches the probe.
setInterval(() => {
  if ($('#board').classList.contains('active')) loadEnv(false);
}, 120000);

setInterval(() => {
  if ($('#task').classList.contains('active')) loadMem();
}, 20000);

// markTab draws a tab's state: a dot when its agent waits for you (unless you
// are looking at it), a pulse while it works, a strike when it is gone.
function markTab(entry) {
  const tab = entry.tab;
  if (!tab) return;
  const has = tab.querySelector('.tdot');
  const want = entry.sess.kind === 'agent' && entry.attention && state.activeTerm !== entry.sess.id;
  if (want && !has) tab.insertBefore(el('span', 'tdot'), tab.querySelector('.x'));
  if (!want && has) has.remove();
  tab.classList.toggle('busy', entry.sess.kind === 'agent' && !!entry.busy && !entry.dead);
  tab.classList.toggle('dead', !!entry.dead);
  if (want) tab.title = 'The agent is waiting for your input';
  else tab.title = entry.dead ? 'Disconnected — the session has ended' : 'Double-click to rename';
}

/* ------------------------------------------------------------------ terminals */

function disposeTerms() {
  for (const [, t] of state.terms) {
    try { t.ws.close(); } catch {}
    try { t.term.dispose(); } catch {}
    t.pane.remove();
    if (t.tab) t.tab.remove();
  }
  state.terms.clear();
  state.closing.clear();
  state.activeTerm = null;
  for (const tab of document.querySelectorAll('#term-tabs .tab')) tab.remove();
}

function updateTermEmpty() {
  const fileOpen = !$('#file-view').classList.contains('hidden') || !$('#review-view').classList.contains('hidden');
  $('#term-empty').classList.toggle('hidden', state.terms.size > 0 || fileOpen);
}

async function syncTerminals(p = pin()) {
  const sessions = (await api.get(`${p.base}/tasks/${p.slug}/terminals`)) || [];
  if (stale(p)) return;
  for (const s of sessions) {
    if (state.closing.has(s.id)) continue;
    const e = state.terms.get(s.id);
    if (e) {
      e.attention = s.attention;
      e.busy = s.busy;
      e.sess = Object.assign(e.sess, s);
      markTab(e);
    } else {
      attachTerm(s, p);
    }
  }
  if (!state.activeTerm && sessions.length && $('#file-view').classList.contains('hidden') &&
      $('#review-view').classList.contains('hidden')) {
    selectTerm(sessions[0].id);
  }
  updateTermEmpty();
}

function attachTerm(sess, p = pin()) {
  // The poll and a click can both learn of the same new session.
  if (state.terms.has(sess.id)) return state.terms.get(sess.id);
  const pane = el('div', 'term-pane');
  $('#term-host').appendChild(pane);

  const term = new Terminal({
    fontFamily: 'ui-monospace, "SF Mono", SFMono-Regular, "JetBrains Mono", Menlo, Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.15,
    cursorBlink: true,
    scrollback: 5000,
    theme: termTheme(),
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(pane);

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const nodeQ = isLocal(p.node) ? '' : `&node=${encodeURIComponent(p.node)}`;
  const ws = new WebSocket(`${proto}://${location.host}/ws/term?id=${encodeURIComponent(sess.id)}${nodeQ}`);
  ws.binaryType = 'arraybuffer';

  const entry = { term, fit, ws, pane, sess, attention: !!sess.attention, busy: !!sess.busy, dead: false, size: '' };

  ws.onopen = () => { doFit(); };
  ws.onmessage = ev => {
    const data = typeof ev.data === 'string' ? ev.data : new Uint8Array(ev.data);
    term.write(data);
  };
  ws.onclose = () => {
    if (state.terms.get(sess.id) !== entry) return; // closed by us
    term.write('\r\n\x1b[90m[disconnected]\x1b[0m\r\n');
    entry.dead = true;
    markTab(entry);
  };

  term.onData(d => { if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'data', data: d })); });

  // Only a real change of size is sent: the observer below fires on every
  // layout pass, and each resize makes the program on the other end redraw.
  function doFit() {
    if (!pane.classList.contains('active')) return;
    try { fit.fit(); } catch { return; }
    const size = term.rows + 'x' + term.cols;
    if (ws.readyState === 1 && size !== entry.size) {
      entry.size = size;
      ws.send(JSON.stringify({ type: 'resize', rows: term.rows, cols: term.cols }));
    }
  }
  entry.doFit = doFit;
  state.terms.set(sess.id, entry);

  // tab
  const tab = el('div', 'tab');
  tab.setAttribute('role', 'tab');
  tab.tabIndex = 0;
  tab.dataset.id = sess.id;
  entry.tab = tab;
  // the server titles a permission-skipping agent "<agent> !"
  if (sess.title && sess.title.endsWith('!')) tab.classList.add('danger-tab');
  tab.appendChild(icon(sess.kind === 'agent' ? 'sparkle' : 'terminal', 'ticon'));
  tab.appendChild(el('span', 'tlabel', sess.title || sess.kind));
  const x = el('span', 'x');
  x.setAttribute('role', 'button');
  x.setAttribute('aria-label', 'Close this session');
  x.title = 'Close this session';
  x.appendChild(icon('x'));
  x.addEventListener('click', async ev => {
    ev.stopPropagation();
    if (sess.kind === 'agent' && !entry.dead && !await ask({
      title: 'Close this agent session?',
      body: `“${sess.title || sess.kind}” stops. The conversation is kept and can be resumed later.`,
      ok: 'Close session', danger: true,
    })) return;
    if (stale(p)) return;
    // Taken off the page first, so a failing request cannot leave a tab that
    // will not go away; the poll stays off it until the server agrees.
    state.closing.add(sess.id);
    try { ws.close(); } catch {}
    try { term.dispose(); } catch {}
    pane.remove();
    tab.remove();
    state.terms.delete(sess.id);
    if (state.activeTerm === sess.id) {
      state.activeTerm = null;
      const first = state.terms.keys().next();
      if (!first.done) selectTerm(first.value);
    }
    updateTermEmpty();
    try { await api.del(`${p.base}/tasks/${p.slug}/terminals/${sess.id}`); }
    catch (e) { toast('Closing the session: ' + e.message); }
    state.closing.delete(sess.id);
    renderResumable(p).catch(() => {});
  });
  tab.appendChild(x);
  tab.addEventListener('click', () => selectTerm(sess.id));
  tab.addEventListener('keydown', ev => {
    if (ev.target === tab && (ev.key === 'Enter' || ev.key === ' ')) { ev.preventDefault(); selectTerm(sess.id); }
    if (ev.target === tab && ev.key === 'F2') { ev.preventDefault(); startRename(tab, sess, p); }
  });
  tab.addEventListener('dblclick', ev => {
    ev.preventDefault();
    ev.stopPropagation();
    startRename(tab, sess, p);
  });
  $('#term-tabs').appendChild(tab);
  markTab(entry);
  updateTermEmpty();
  return entry;
}

// startRename swaps the tab label for an input. Enter or blur commits, Escape
// cancels; the label is restored either way so a failed request cannot leave the
// tab stuck in edit mode.
function startRename(tab, sess, p = pin()) {
  // By class, not position: the attention dot and the icon sit in the tab too.
  const label = tab.querySelector('.tlabel');
  if (!label || tab.querySelector('input.rename')) return;
  const current = label.textContent;
  const input = el('input', 'rename');
  input.value = current;
  tab.replaceChild(input, label);
  input.focus();
  input.select();

  let done = false;
  const finish = async (commit) => {
    if (done) return;
    done = true;
    const next = input.value.trim();
    const span = el('span', 'tlabel', commit && next ? next : current);
    tab.replaceChild(span, input);
    if (!commit || !next || next === current) return;
    try {
      await api.put(`${p.base}/tasks/${p.slug}/terminals/${sess.id}`, { title: next });
      sess.title = next;
    } catch (e) {
      span.textContent = current;   // server rejected it — do not lie in the UI
      toast('Renaming the tab: ' + e.message);
    }
  };
  input.addEventListener('keydown', ev => {
    ev.stopPropagation();
    if (ev.key === 'Enter') finish(true);
    if (ev.key === 'Escape') finish(false);
  });
  input.addEventListener('blur', () => finish(true));
  input.addEventListener('click', ev => ev.stopPropagation());
}

function selectTerm(id) {
  state.activeTerm = id;
  $('#file-view').classList.add('hidden');
  hideReview();
  $('#term-host').classList.remove('hidden');
  for (const [sid, e] of state.terms) {
    const on = sid === id;
    e.pane.classList.toggle('active', on);
    if (e.tab) {
      e.tab.classList.toggle('active', on);
      e.tab.setAttribute('aria-selected', on ? 'true' : 'false');
    }
    if (on) { e.doFit(); e.term.focus(); e.attention = false; }
    markTab(e);
  }
  updateTermEmpty();
}

// The terminal fills whatever room the layout leaves it, which changes with
// more than the window: a panel collapsing, the file viewer closing, the
// sidebar's scrollbar appearing.
{
  let queued = false;
  new ResizeObserver(() => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      const e = state.terms.get(state.activeTerm);
      if (e) e.doFit();
    });
  }).observe($('#term-host'));
}

async function newAgent(btn) {
  const p = pin();
  btn.disabled = true;
  try {
    const s = await api.post(`${p.base}/tasks/${p.slug}/terminals`, { kind: 'agent' });
    if (!stale(p)) { attachTerm(s, p); selectTerm(s.id); }
  } catch (e) { toast(e.message); }
  btn.disabled = false;
}

async function newShell(btn) {
  const p = pin();
  btn.disabled = true;
  try {
    const s = await api.post(`${p.base}/tasks/${p.slug}/terminals`, { kind: 'shell', dir: state.filePath });
    if (!stale(p)) { attachTerm(s, p); selectTerm(s.id); }
  } catch (e) { toast(e.message); }
  btn.disabled = false;
}

$('#btn-newagent').addEventListener('click', e => newAgent(e.currentTarget));
$('#empty-agent').addEventListener('click', e => newAgent(e.currentTarget));
$('#btn-newterm').addEventListener('click', e => newShell(e.currentTarget));
$('#empty-shell').addEventListener('click', e => newShell(e.currentTarget));

// Sessions die with the server, but the conversation does not: Claude Code keeps
// a transcript per working directory, so --continue picks the agent back up.
// The Resume menu lists every recorded conversation that is not already live
// in a tab. Choosing one resumes that specific transcript (claude --resume
// <id>), and it leaves the list because the server then reports it as claimed.
//
// Claude Code only: the list is built from ~/.claude/projects, so a task running
// another agent has no Resume button at all.
//
// The task is pinned for the whole call. Every task draws into the same
// button, and this runs un-awaited off loadUsage(), so switching tasks while
// the fetch is in flight used to let the OLD task's conversations land in the
// NEW task's list — and choosing one then asked the new task to resume a
// conversation belonging to the old one, which Claude Code answers with
// "No conversation found with session ID" and an immediately dead tab.
async function renderResumable(p = pin()) {
  const btn = $('#resume-list');
  if (!agentInfo(state.taskAgent).resumable) { btn.classList.add('hidden'); state.resumable = []; return; }
  let list = [];
  try { list = (await api.get(`${p.base}/tasks/${p.slug}/resumable`)) || []; } catch { return; }
  if (stale(p)) return; // switched away mid-flight: not ours to draw
  state.resumable = list;
  state.resumableFor = p;
  const free = list.filter(c => !c.inUse).length;
  btn.classList.toggle('hidden', !list.length);
  const count = btn.querySelector('.count');
  count.textContent = free;
  count.classList.toggle('hidden', !free);
}

$('#resume-list').addEventListener('click', () => {
  const btn = $('#resume-list');
  const p = state.resumableFor;
  if (stale(p)) return;
  showMenu(btn, {
    title: 'Resume a conversation',
    items: state.resumable.map(c => {
      const when = c.lastActivity ? new Date(c.lastActivity) : null;
      const stamp = when ? when.toLocaleString([], { month: 'short', day: 'numeric',
                                                    hour: '2-digit', minute: '2-digit' }) : '';
      // The title first, because it is the only part that says which
      // conversation this is. One tab routinely records several without you
      // opening anything — /clear starts a new one, and so does changing the
      // model or updating the CLI — and as a list of bare timestamps they read
      // as duplicates of the session you are already in.
      const meta = `${stamp}${c.messages ? ' · ' + c.messages + ' msgs' : ''}`;
      return {
        icon: 'history',
        label: c.title || meta || c.id,
        detail: (c.title ? meta : '') + (c.inUse ? ' · in use' : ''),
        // Still being written, by a tab that changed session id under us or
        // by a terminal outside fride. Shown rather than hidden — it is the
        // conversation you are probably looking for — but not startable: a
        // second --resume on it would give one transcript two writers.
        disabled: c.inUse,
        title: c.inUse ? `Conversation ${c.id} is open in another session` : `Resume conversation ${c.id}`,
        onSelect: async () => {
          // p, not the open task: an item belongs to the task it was drawn for.
          if (stale(p)) return;
          try {
            const sess = await api.post(`${p.base}/tasks/${p.slug}/terminals`,
                                        { kind: 'agent', resumeId: c.id });
            if (!stale(p)) { attachTerm(sess, p); selectTerm(sess.id); }
          } catch (e) { toast(e.message); }
          await renderResumable(p);
        },
      };
    }),
  });
});

/* ------------------------------------------------------------------ files */

function renderCrumbs(path) {
  const host = $('#files-path');
  host.innerHTML = '';
  const parts = path ? path.split('/') : [];
  const root = el('button', null, 'workspace');
  root.addEventListener('click', () => loadFiles(''));
  host.appendChild(root);
  parts.forEach((seg, i) => {
    host.appendChild(el('span', 'sep', '/'));
    const b = el('button', null, seg);
    const to = parts.slice(0, i + 1).join('/');
    b.addEventListener('click', () => loadFiles(to));
    host.appendChild(b);
  });
}

async function loadFiles(path, p = pin()) {
  state.filePath = path;
  renderCrumbs(path);
  $('#files-up').disabled = !path;
  const list = $('#files-list');
  let entries;
  try { entries = await api.get(`${p.base}/tasks/${p.slug}/files?path=${encodeURIComponent(path)}`); }
  catch (e) {
    if (stale(p) || state.filePath !== path) return;
    list.innerHTML = '';
    list.appendChild(el('div', 'warn', e.message));
    return;
  }
  // A quicker click on another folder owns the list by now.
  if (stale(p) || state.filePath !== path) return;
  list.innerHTML = '';
  if (!(entries || []).length) list.appendChild(el('div', 'empty', 'Empty folder'));
  for (const en of entries || []) {
    const row = el('button', 'file-row' + (en.isDir ? ' dir' : '') + (en.path === state.openFile ? ' open' : ''));
    row.appendChild(icon(en.isDir ? 'folder' : 'file', 'ic'));
    row.appendChild(el('span', null, en.name));
    row.title = en.path;
    row.addEventListener('click', () => en.isDir ? loadFiles(en.path) : openFile(en.path));
    list.appendChild(row);
  }
}

$('#files-reveal').addEventListener('click', async ev => {
  ev.stopPropagation();
  const b = $('#files-reveal');
  const p = pin();
  b.disabled = true;
  try { await api.post(`${p.base}/tasks/${p.slug}/reveal?path=${encodeURIComponent(state.filePath || '')}`); }
  catch (e) { toast(e.message); }
  b.disabled = false;
});

$('#files-up').addEventListener('click', ev => {
  ev.stopPropagation();
  const p = state.filePath;
  if (!p) return;
  const parts = p.split('/');
  parts.pop();
  loadFiles(parts.join('/'));
});

async function openFile(path) {
  const p = pin();
  let fc;
  try { fc = await api.get(`${p.base}/tasks/${p.slug}/file?path=${encodeURIComponent(path)}`); }
  catch (e) { if (!stale(p)) toast(e.message); return; }
  if (stale(p)) return;
  state.openFile = path;
  for (const r of document.querySelectorAll('#files-list .file-row')) r.classList.toggle('open', r.title === path);
  const name = $('#file-view-name');
  name.textContent = path;
  if (fc.truncated) name.appendChild(el('span', 'tag', 'first 1 MiB'));
  const body = $('#file-view-body'), gutter = $('#file-view-gutter');
  if (fc.withheld || fc.binary) {
    body.className = 'note';
    gutter.textContent = '';
    body.textContent = fc.withheld
      ? 'Withheld: this file holds credentials.\n\n' +
        'The API is unauthenticated on loopback, so secret-bearing config is not served.\n' +
        'Open a shell tab and read it there if you need it.'
      : 'Binary file — not shown.';
  } else {
    body.className = '';
    const text = fc.content || '';
    const n = text.split('\n').length - (text.endsWith('\n') ? 1 : 0);
    gutter.textContent = Array.from({ length: Math.max(n, 1) }, (_, i) => i + 1).join('\n');
    body.textContent = text;
  }
  $('#file-view').querySelector('.code').scrollTop = 0;
  hideReview();
  $('#file-view').classList.remove('hidden');
  $('#term-host').classList.add('hidden');
  for (const [, e] of state.terms) if (e.tab) e.tab.classList.remove('active');
}

$('#file-close').addEventListener('click', () => {
  state.openFile = '';
  for (const r of document.querySelectorAll('#files-list .file-row.open')) r.classList.remove('open');
  $('#file-view').classList.add('hidden');
  // A file opened from the review goes back to the review, not to a terminal.
  if (state.fileFromReview) { state.fileFromReview = false; showReview(); return; }
  $('#term-host').classList.remove('hidden');
  if (state.activeTerm) selectTerm(state.activeTerm);
  else updateTermEmpty();
});

/* ------------------------------------------------------------------ side panels

   Each panel folds away on a click on its header, and stays folded across
   tasks and reloads: the sidebar is long, and which parts matter is a habit,
   not a per-task choice. */

const FOLD_KEY = 'fride-folded';
function foldedPanels() {
  try { return new Set(JSON.parse(localStorage.getItem(FOLD_KEY) || '[]')); } catch { return new Set(); }
}
for (const panel of document.querySelectorAll('.panel[data-panel]')) {
  const name = panel.dataset.panel;
  if (foldedPanels().has(name)) panel.classList.add('collapsed');
  const head = panel.querySelector('.panel-h');
  head.addEventListener('click', e => {
    if (e.target.closest('button')) return;
    panel.classList.toggle('collapsed');
    const set = foldedPanels();
    if (panel.classList.contains('collapsed')) set.add(name); else set.delete(name);
    try { localStorage.setItem(FOLD_KEY, JSON.stringify([...set])); } catch { /* private mode */ }
  });
}

/* ------------------------------------------------------------------ task actions */

// Which projects to hand to their IDE on the next Start. Reset per task.
function ideServices(t) {
  const name = (t.stack && t.stack.profile) || '';
  const p = (taskCfg().profiles || []).find(x => x.name === name);
  return (p && p.ide) || [];
}

function renderIdeChoices(t) {
  const host = $('#ide-choices');
  if (!host) return;
  host.innerHTML = '';
  // Services this task runs from source and that have an IDE run
  // configuration in config.json.
  const startable = ((t.stack && t.stack.services) || [])
    .filter(sv => sv.mode === 'source' && ideServices(t).includes(sv.name))
    .map(sv => ({ name: sv.name }));
  if (!startable.length) return;
  host.appendChild(el('div', 'idehdr', 'Open in the IDE instead of starting here:'));
  for (const r of startable) {
    const lab = el('label', 'idelab');
    const cb = el('input');
    cb.type = 'checkbox';
    cb.checked = state.viaIde.has(r.name);
    cb.addEventListener('change', () => {
      if (cb.checked) state.viaIde.add(r.name); else state.viaIde.delete(r.name);
    });
    lab.appendChild(cb);
    lab.appendChild(el('span', null, r.name));
    host.appendChild(lab);
  }
}

$('#btn-up').addEventListener('click', async () => {
  const b = $('#btn-up');
  const p = pin();
  b.disabled = true; setLabel(b, 'Starting…');
  try {
    await startStack(false, p);
  } catch (e) {
    // The memory check is the one refusal worth arguing with: it is an
    // estimate about the near future, and the user can see things it cannot
    // (a build that just finished, an IDE they are about to close).
    if (e.data && e.data.memoryBlock) {
      if (await ask({ title: 'Not enough memory for another stack', body: e.message,
                      ok: 'Start it anyway', danger: true }) && !stale(p)) {
        try { await startStack(true, p); } catch (e2) { toast(e2.message); }
      }
    } else {
      toast(e.message);
    }
  }
  b.disabled = false; setLabel(b, 'Start');
  loadMem();
});

async function startStack(force, p = pin()) {
  const s = await api.post(`${p.base}/tasks/${p.slug}/stack/up`, { viaIde: [...state.viaIde], force });
  if (stale(p)) return;
  renderStack(s);
  await refreshTask(p);
}

/* ------------------------------------------------------------------ memory

   Two ceilings share this line: the computer's RAM and Docker's own
   allotment, which a stack's containers can fill long before the computer
   notices. Shown before Start rather than only explained after a refusal.
   It is the memory of the computer the task runs on, hence the task's base. */

async function loadMem(p = pin()) {
  const host = $('#mem-head');
  if (!host) return;
  let m;
  try { m = await api.get(`${p.base}/mem`); } catch { if (!stale(p)) host.textContent = ''; return; }
  if (stale(p)) return;
  host.innerHTML = '';
  if (!m.budget.host && !m.budget.docker) { host.className = 'memhead'; return; }
  const gb = (b) => (b / (1 << 30)).toFixed(1) + ' GB';
  host.className = 'memhead ' + (m.roomForAnother ? (m.warn ? 'tight' : 'ok') : 'bad');
  const parts = [];
  if (m.budget.host && m.mem.hostTotal) parts.push(`RAM ${gb(m.mem.hostFree)} free of ${gb(m.mem.hostTotal)}`);
  if (m.budget.docker && m.mem.dockerKnown) {
    parts.push(`Docker ${gb(m.mem.dockerTotal - m.mem.dockerUsed)} of ${gb(m.mem.dockerTotal)}`);
  }
  host.appendChild(el('div', 'memline', parts.join(' · ')));
  if (!m.roomForAnother) host.appendChild(el('div', 'memwhy', 'No room for another stack: ' + m.reason));
  else if (m.warn) host.appendChild(el('div', 'memwhy', m.warn));
}

$('#btn-down').addEventListener('click', async () => {
  const b = $('#btn-down');
  const p = pin();
  b.disabled = true; setLabel(b, 'Stopping…');
  try {
    const s = await api.post(`${p.base}/tasks/${p.slug}/stack/down`);
    if (!stale(p)) renderStack(s);
  } catch (e) { toast(e.message); }
  b.disabled = false; setLabel(b, 'Stop');
});

for (const b of document.querySelectorAll('[data-set-status]')) {
  b.addEventListener('click', async () => {
    if (b.classList.contains('on')) return;
    const p = pin();
    await setStatus(p.slug, b.dataset.setStatus, p.node);
    try { await refreshTask(p); } catch (e) { toast(e.message); }
  });
}

// deleteTask removes the task and its folder. It asks git first so that
// uncommitted work is named before anything is destroyed, rather than being
// discovered afterwards.
async function deleteTask(t, done) {
  // The board list omits per-repo git status, which costs a status call per
  // repo and would run on every refresh. Fetch it for this one task instead, so
  // the confirmation can name what is about to be lost.
  let detail = t;
  try { detail = await api.get(nodeBase(t.node) + '/tasks/' + t.slug); } catch { /* fall back to what we have */ }
  const dirty = Object.entries(detail.dirty || {}).filter(([, n]) => n > 0);
  const body = el('div');
  body.appendChild(el('div', null, 'This removes the whole workspace folder, including every worktree in it:'));
  const path = el('code', null, detail.workspace || t.slug);
  path.style.display = 'block';
  path.style.margin = '8px 0';
  body.appendChild(path);
  body.appendChild(el('div', null, 'Commits already pushed are safe; anything else is not.'));
  if (dirty.length) {
    const w = el('div', 'warn', 'Uncommitted changes: ' + dirty.map(([r, n]) => `${r} (${n})`).join(', '));
    w.style.marginTop = '10px';
    body.appendChild(w);
  }
  if (!await ask({ title: `Delete “${detail.name || t.name}”?`, body, ok: 'Delete task', danger: true, icon: 'trash' })) return;
  try {
    await api.del(`${nodeBase(t.node)}/tasks/${t.slug}?purge=1`);
  } catch (e) { toast(e.message); return; }
  toast(`Deleted ${detail.name || t.name}.`, 'ok');
  if (done) await done();
}

$('#btn-delete').addEventListener('click', async () => {
  const t = findTask(state.current, state.currentNode) || { slug: state.current, name: state.current, node: state.currentNode };
  await deleteTask(t, async () => {
    state.current = null;
    state.gen++;
    await goBoard();
  });
});

async function goBoard() {
  stopPolling();
  stopOrch();
  stopInbox();
  disposeTerms();
  try { await loadBoard(); } catch (e) { toast(e.message); }
  showView('board');
}

$('#back').addEventListener('click', goBoard);

/* ------------------------------------------------------------------ instances */

async function loadInstances() {
  const rows = await api.get('/api/instances');
  const body = $('#instances-table').querySelector('tbody');
  body.innerHTML = '';
  for (const r of rows || []) {
    const tr = el('tr');
    tr.appendChild(el('td', 'mono', String(r.k)));
    const name = el('td');
    if (r.exists) {
      const b = el('button', 'link', r.name);
      b.title = 'Open this task';
      b.addEventListener('click', () => openTask(r.name, ''));
      name.appendChild(b);
    } else {
      name.appendChild(document.createTextNode(r.name + ' '));
      name.appendChild(el('span', 'tag muted', 'deleted'));
    }
    tr.appendChild(name);
    const ports = el('td');
    const pl = el('div', 'portlist');
    for (const [n, p] of Object.entries(r.ports || {})) pl.appendChild(el('span', 'pill', `${n} :${p}`));
    ports.appendChild(pl);
    tr.appendChild(ports);
    const lis = el('td');
    const ll = el('div', 'portlist');
    for (const p of r.listening || []) ll.appendChild(el('span', 'pill up', ':' + p));
    if (!(r.listening || []).length) ll.appendChild(el('span', 'hint', 'nothing listening'));
    lis.appendChild(ll);
    tr.appendChild(lis);
    body.appendChild(tr);
  }
  if (!(rows || []).length) {
    const tr = el('tr');
    const td = el('td', 'empty', 'No task holds a slot. A slot is taken when a stack starts and freed when it stops.');
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
  }
}

$('#toggle-hidden').addEventListener('click', async () => {
  state.showHidden = !state.showHidden;
  await loadBoard();
});

function leaveTask() { stopPolling(); stopOrch(); stopInbox(); }

$('#nav-board').addEventListener('click', goBoard);
$('#nav-instances').addEventListener('click', async () => {
  leaveTask();
  try { await loadInstances(); } catch (e) { toast(e.message); }
  showView('instances');
});
$('#nav-computers').addEventListener('click', async () => {
  leaveTask();
  try { await loadComputers(); } catch (e) { toast(e.message); }
  showView('computers');
});

async function loadComputers() {
  await loadFleet();
  const f = state.fleet;
  const me = $('#computer-self');
  me.innerHTML = '';
  const st = f.state || {};
  const head = el('div', 'comp-head');
  head.appendChild(el('span', 'dot ' + (st.phase === 'running' ? 'good' : 'fail')));
  head.appendChild(el('b', null, f.self || 'this computer'));
  head.appendChild(el('span', 'hint', ' — this computer · ' + (st.phase || '?') + (st.kind ? ' (' + st.kind + ')' : '')));
  me.appendChild(head);
  if (st.self && st.self.loginName) me.appendChild(el('div', 'hint', 'signed in as ' + st.self.loginName));
  if (st.phase === 'needs-login') {
    const p = el('div', 'signin');
    p.appendChild(document.createTextNode('Sign this computer in to your tailnet: '));
    if (st.authUrl) {
      const a = el('a', null, st.authUrl);
      a.href = st.authUrl; a.target = '_blank';
      p.appendChild(a);
    } else p.appendChild(el('span', 'hint', 'waiting for the sign-in link…'));
    me.appendChild(p);
  }
  if (st.error) me.appendChild(el('div', 'warn', st.error));

  const rows = $('#computers-table').querySelector('tbody');
  rows.innerHTML = '';
  const tasks = state.tasks || [];
  for (const p of f.peers || []) {
    const tr = el('tr');
    tr.appendChild(el('td', null, p.name));
    const s = el('td');
    s.appendChild(el('span', 'dot ' + (p.online ? 'good' : 'fail')));
    s.appendChild(document.createTextNode(p.online ? ' online' : ' offline' + (p.lastSeen && !p.lastSeen.startsWith('0001') ? ' since ' + new Date(p.lastSeen).toLocaleString() : '')));
    tr.appendChild(s);
    tr.appendChild(el('td', null, p.os || ''));
    tr.appendChild(el('td', null, String(tasks.filter(t => t.node === p.name).length)));
    rows.appendChild(tr);
  }
  if (!(f.peers || []).length) {
    const tr = el('tr');
    const td = el('td', 'empty', 'No other computers yet. Run `fride serve --fleet` on another one, signed in to the same account.');
    td.colSpan = 4;
    tr.appendChild(td);
    rows.appendChild(tr);
  }
  const mh = $('#mirror-list');
  mh.innerHTML = '';
  for (const m of f.mirrors || []) {
    const row = el('div', 'baserow');
    row.appendChild(el('span', null, `localhost:${m.port}`));
    row.appendChild(el('span', 'n', `${m.task} on ${m.node}` + (m.connections ? ` · ${m.connections} open` : '')));
    row.appendChild(el('span', 'sp'));
    const x = el('button', 'mini', 'stop');
    x.addEventListener('click', async () => {
      try { await api.del(`/api/fleet/mirror?node=${encodeURIComponent(m.node)}&task=${encodeURIComponent(m.task)}`); }
      catch (e) { toast(e.message); }
      await loadComputers();
    });
    row.appendChild(x);
    mh.appendChild(row);
  }
  if (!(f.mirrors || []).length) mh.appendChild(el('div', 'hint', 'Nothing from another computer is open here.'));
}

$('#nav-orch').addEventListener('click', () => { stopPolling(); stopInbox(); showView('orch'); startOrch(); });
$('#nav-inbox').addEventListener('click', () => { leaveTask(); showView('inbox'); startInbox(); });

/* ------------------------------------------------ environment prerequisites */

// The failure these prevent is a late one: the task is created, the worktrees
// are built, and only the eventual install or boot says what was wrong. So
// a broken prerequisite is stated in full on the board, with the fix; a working
// one stays a single quiet line.
async function loadEnv(force) {
  const bar = $('#envbar');
  let r;
  try { r = force ? await api.post('/api/env', {}) : await api.get('/api/env'); }
  catch { bar.classList.add('hidden'); return; }
  bar.className = 'envbar ' + (r.ok ? 'ok' : 'bad');
  bar.innerHTML = '';

  const head = el('div', 'envhead');
  head.appendChild(el('span', 'dot ' + (r.ok ? 'good' : 'fail')));
  head.appendChild(document.createTextNode(
    r.ok ? 'Environment ready' : 'Set this up before creating a task'));
  if (r.ok) {
    const checks = el('span', 'checks');
    for (const c of r.checks) {
      const s = el('span');
      s.appendChild(icon('check'));
      s.appendChild(document.createTextNode(c.label));
      if (c.detail) s.title = c.detail;
      checks.appendChild(s);
    }
    head.appendChild(checks);
  }
  head.appendChild(el('span', 'spacer'));
  const btn = el('button', 'mini');
  btn.appendChild(icon('refresh'));
  btn.appendChild(el('span', null, 'Re-check'));
  btn.title = 'Run the checks again (and re-read config.json)';
  btn.addEventListener('click', async () => {
    btn.disabled = true; setLabel(btn, 'Checking…');
    await loadEnv(true);
  });
  head.appendChild(btn);
  bar.appendChild(head);
  if (r.ok) return;

  for (const c of r.checks) {
    if (c.ok) continue;
    const item = el('div', 'envitem');
    const t = el('div', '');
    t.appendChild(el('b', '', c.label + ': '));
    t.appendChild(document.createTextNode(c.detail));
    item.appendChild(t);
    if (c.needed) item.appendChild(el('div', 'envwhy', 'Needed for ' + c.needed + '.'));
    if (c.fix) item.appendChild(el('pre', 'envfix', c.fix));
    bar.appendChild(item);
  }
}

/* --------------------------------------------------------------------- theme */

// Three states, deliberately: no stored choice means follow the system, and
// that has to keep following it when the system flips mid-session. An explicit
// choice stamps data-theme, which both CSS blocks key off.
const THEME_KEY = 'fride-theme';

function storedTheme() {
  try { return localStorage.getItem(THEME_KEY); } catch { return null; }
}

function systemTheme() {
  return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function activeTheme() {
  return storedTheme() || systemTheme();
}

// termTheme reads the palette out of the stylesheet, so the terminal cannot
// drift from the rest of the UI when a colour is changed in one place.
function termTheme() {
  const css = getComputedStyle(document.documentElement);
  const v = n => css.getPropertyValue(n).trim();
  const t = {
    background: v('--term-bg') || '#000000',
    foreground: v('--term-fg') || '#e4e6eb',
    cursor: v('--term-cursor') || '#7aa2f7',
    cursorAccent: v('--term-bg') || '#000000',
    selectionBackground: v('--term-sel') || 'rgba(122,162,247,.3)',
  };
  for (const c of ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white']) {
    const base = v('--term-' + c), bright = v('--term-bright-' + c);
    if (base) t[c] = base;
    if (bright) t['bright' + c[0].toUpperCase() + c.slice(1)] = bright;
  }
  return t;
}

function applyTheme(mode) {
  const root = document.documentElement;
  if (mode) root.setAttribute('data-theme', mode);
  else root.removeAttribute('data-theme');

  const dark = activeTheme() === 'dark';
  const btn = $('#theme-toggle');
  if (btn) {
    // The icon shows what you get by pressing it, not what you are in.
    btn.innerHTML = '';
    btn.appendChild(icon(dark ? 'sun' : 'moon'));
    btn.title = dark ? 'Switch to light' : 'Switch to dark';
  }
  // xterm paints its own canvas, so a live terminal keeps the old colours
  // until it is told. Repainting is why the toggle works mid-session.
  const t = termTheme();
  for (const [, e] of state.terms) {
    if (e.term) e.term.options.theme = t;
  }
}

$('#theme-toggle').addEventListener('click', () => {
  const next = activeTheme() === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem(THEME_KEY, next); } catch { /* private mode */ }
  applyTheme(next);
});

// Only relevant while no explicit choice is stored.
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if (!storedTheme()) applyTheme(null);
});

applyTheme(storedTheme());

/* ---------------------------------------------------------------- profiles */

// A profile is one way to boot the stack, from config.json — "local" (all from
// source) and "docker" (dependencies as containers) are typical. The default
// is preselected; the choice only matters at the task's next stack up.
async function loadConfig() {
  try { state.config = await api.get('/api/config'); } catch { state.config = null; }
  const c = state.config;
  $('#stack-name').textContent = c && c.name ? c.name : '';
  return c;
}

/* ---------------------------------------------------------------- git identities */

// A computer's git identities, kept per API base: the task view draws its
// badge on every refresh, and the list only changes when config.json or the
// companion does. A computer whose FrIDE predates identities has none.
async function loadGitIds(base) {
  let d = null;
  try { d = await api.get(base + '/git/identities'); } catch { /* none */ }
  state.gitIds[base] = (d && d.identities) || [];
  state.gitDefaults[base] = (d && d.default) || '';
  return state.gitIds[base];
}

// The badge names who the task commits and pushes as. A switch applies to
// sessions started after it: a running one keeps the environment it got.
function renderGitBadge(t, p) {
  const badge = $('#task-git-badge');
  const ids = state.gitIds[p.base] || [];
  const cur = t.gitIdentity || '';
  const g = ids.find(x => x.id === cur);
  badge.classList.toggle('hidden', !cur && !ids.length);
  badge.classList.toggle('missing', !!cur && !g);
  setLabel(badge, cur ? (g ? g.label : cur + ' (gone)') : 'no git identity');
  badge.onclick = async () => {
    const fresh = await loadGitIds(p.base);
    if (stale(p)) return;
    const pick = async (id) => {
      if (id === cur || stale(p)) return;
      try { await api.put(`${p.base}/tasks/${p.slug}/git-identity`, { gitIdentity: id || 'none' }); }
      catch (e) { toast(e.message); return; }
      await refreshTask(p);
      if ((t.terminals || []).length) {
        toast('Sessions started from now on use it. Open ones keep the identity they started with.', 'info', 8000);
      }
    };
    showMenu(badge, {
      title: 'Commit and push as',
      items: fresh.map(x => ({
        label: x.label, checked: x.id === cur, onSelect: () => pick(x.id),
        detail: x.host + (x.name || x.email ? ' · ' + [x.name, x.email].filter(Boolean).join(' ') : '')
          + (x.has_token ? '' : ' · no token'),
      })).concat([{ label: 'None', detail: "git's own config", checked: !cur, onSelect: () => pick('') }]),
    });
  };
}

// The identity a new task gets unless another is picked: the server names,
// per repository, the one its origin's host picks (else the default), and
// the first selected repository decides, as it does on the server. Picking
// one by hand stops the suggestion from following the repositories.
function renderGitPicker() {
  const ids = (state.newGit && state.newGit.identities) || [];
  $('#git-line').classList.toggle('hidden', ids.length === 0);
  const sel = $('#new-git');
  sel.innerHTML = '';
  for (const g of ids) {
    const o = el('option', null, `${g.label} — ${g.host}${g.has_token ? '' : ' (no token yet)'}`);
    o.value = g.id;
    sel.appendChild(o);
  }
  const none = el('option', null, "None — git's own config");
  none.value = 'none';
  sel.appendChild(none);
  suggestGit();
}

function suggestGit() {
  if (state.gitEdited || !state.newGit) return;
  const first = [...$('#repo-picker').querySelectorAll('input[type=checkbox]')].find(cb => cb.checked);
  const repo = first && state.repos.find(r => r.name === first.dataset.repo);
  const want = repo ? repo.gitIdentity : state.newGit.default;
  $('#new-git').value = want || 'none';
}

$('#new-git').addEventListener('change', () => { state.gitEdited = true; });

function renderModePicker() {
  const host = $('#mode-picker');
  if (!host) return;
  host.innerHTML = '';
  const cfg = state.newConfig || state.config || {};
  const profiles = cfg.profiles || [];
  for (const p of profiles) {
    const row = el('label', 'agent-pick');
    const rb = el('input');
    rb.type = 'radio';
    rb.name = 'profile';
    rb.value = p.name;
    rb.checked = p.name === state.newProfile;
    rb.addEventListener('change', () => { state.newProfile = p.name; renderModeHints(); });
    row.appendChild(rb);
    const nm = el('div');
    nm.appendChild(document.createTextNode(p.name));
    if (p.name === cfg.defaultProfile) {
      const tag = el('span', 'tag muted', 'default');
      tag.style.marginLeft = '6px';
      nm.appendChild(tag);
    }
    nm.appendChild(el('small', null, p.description || p.services.join(', ')));
    row.appendChild(nm);
    host.appendChild(row);
  }
  if (!profiles.length) host.appendChild(el('div', 'hint', 'config.json has no profiles yet.'));
  renderModeHints();
}

function renderModeHints() {
  $('#mode-hint').textContent = 'A repository you check out here runs from source; a service whose ' +
    'repository is not here comes from a container, or is left out, as the profile says.';
}

/* ------------------------------------------------------------------ new task */

$('#btn-new').addEventListener('click', async () => {
  $('#modal-error').textContent = '';
  $('#new-name').value = '';
  $('#new-dangerous').checked = false;
  $('#slug-preview').textContent = '—';
  $('#new-branch').value = '';
  state.branchEdited = false;
  setBranchHint('');
  await loadFleet();
  if (!isLocal(state.newNode) && !onlinePeers().some(p => p.name === state.newNode)) state.newNode = '';
  renderNodePicker();
  openModal('modal');
  $('#new-name').focus();
  await fillNewTask();
});

// The computer the new task runs on. Only offered when there is a choice.
function renderNodePicker() {
  const host = $('#node-picker');
  const peers = onlinePeers();
  $('#node-line').classList.toggle('hidden', !state.fleet.enabled || peers.length === 0);
  host.innerHTML = '';
  const options = [{ name: '', label: (state.fleet.self || 'this computer') + ' (this computer)' }]
    .concat(peers.map(p => ({ name: p.name, label: p.name + (p.os ? ' · ' + p.os : '') })));
  for (const o of options) {
    const row = el('label', 'agent-pick');
    const rb = el('input');
    rb.type = 'radio';
    rb.name = 'node';
    rb.checked = isLocal(o.name) ? isLocal(state.newNode) : o.name === state.newNode;
    rb.addEventListener('change', async () => { state.newNode = o.name; await fillNewTask(); });
    row.appendChild(rb);
    row.appendChild(el('div', null, o.label));
    host.appendChild(row);
  }
}

// fillNewTask loads what the chosen computer offers: its repositories (with
// their branches), its profiles and its agent CLIs.
async function fillNewTask() {
  const base = nodeBase(state.newNode);
  try { state.newConfig = await api.get(base + '/config'); }
  catch (e) { $('#modal-error').textContent = e.message; state.newConfig = { profiles: [] }; }
  if (!state.newProfile || !(state.newConfig.profiles || []).some(p => p.name === state.newProfile)) {
    state.newProfile = state.newConfig.defaultProfile || '';
  }
  renderModePicker();
  updateSlugPreview();
  const core = new Set(state.newConfig.coreRepos || []);
  const picker = $('#repo-picker');
  picker.innerHTML = '';
  try { state.repos = await api.get(base + '/repos'); } catch { state.repos = []; }
  for (const r of state.repos) {
    // Core repositories make up the app, so they start out selected.
    const on = r.exists && core.has(r.name);
    const row = el('div', 'repo-pick' + (on ? '' : ' off') + (r.exists ? '' : ' missing'));
    const cb = el('input'); cb.type = 'checkbox'; cb.dataset.repo = r.name;
    cb.disabled = !r.exists;
    cb.checked = on;
    cb.addEventListener('change', () => { row.classList.toggle('off', !cb.checked); suggestGit(); });
    row.appendChild(cb);

    const nm = el('div', 'nm');
    nm.appendChild(document.createTextNode(r.name));
    if (core.has(r.name)) nm.appendChild(el('span', 'tag', 'core'));
    // The leading mark keeps the path's first "/" in place under the CSS's
    // right-to-left trick, which would otherwise move it to the end.
    const sm = el('small', null, r.exists ? '\u200e' + r.main : 'main checkout missing');
    // Paths are long and only their end tells repositories apart; the CSS
    // keeps the end, the title keeps all of it.
    sm.title = r.main || '';
    nm.appendChild(sm);
    row.appendChild(nm);

    const sel = el('select');
    sel.dataset.repo = r.name;
    sel.setAttribute('aria-label', 'Base branch for ' + r.name);
    for (const b of (r.recentBranches || [])) {
      const o = el('option', null, b); o.value = b;
      if (b === r.defaultBase) o.selected = true;
      sel.appendChild(o);
    }
    if (!sel.options.length && r.defaultBase) {
      const o = el('option', null, r.defaultBase); o.value = r.defaultBase; sel.appendChild(o);
    }
    row.appendChild(sel);
    // clicking the row toggles the checkbox, except on the select itself
    row.addEventListener('click', ev => {
      if (ev.target === cb || ev.target === sel || sel.contains(ev.target)) return;
      if (cb.disabled) return;
      cb.checked = !cb.checked;
      row.classList.toggle('off', !cb.checked);
      suggestGit();
    });
    picker.appendChild(row);
  }
  if (!state.repos.length) picker.appendChild(el('div', 'hint', 'No repositories found under the base folders (Settings).'));
  try { await loadAgents(base); } catch (e) { console.error('agents:', e); }
  // Default to the last choice, unless that CLI is gone from this machine.
  state.newAgent = state.agentDefault;
  if (!agentInfo(state.newAgent).installed) {
    const first = state.agents.find(a => a.installed);
    if (first) state.newAgent = first.kind;
  }
  renderAgentPicker();
  try { state.newGit = await api.get(base + '/git/identities'); }
  catch { state.newGit = { identities: [], default: '' }; }
  state.gitEdited = false;
  renderGitPicker();
}

// Agents that are not installed stay visible but disabled, with the reason:
// "why can I not pick Antigravity" then answers itself.
function renderAgentPicker() {
  const host = $('#agent-picker');
  host.innerHTML = '';
  for (const a of state.agents) {
    const row = el('label', 'agent-pick' + (a.installed ? '' : ' missing'));
    const rb = el('input');
    rb.type = 'radio';
    rb.name = 'agent';
    rb.value = a.kind;
    rb.disabled = !a.installed;
    rb.checked = a.kind === state.newAgent;
    rb.addEventListener('change', () => { state.newAgent = a.kind; renderAgentHints(); });
    row.appendChild(rb);
    const nm = el('div');
    nm.appendChild(document.createTextNode(a.label));
    nm.appendChild(el('small', null, a.installed ? a.path : (a.detail || 'not installed')));
    row.appendChild(nm);
    host.appendChild(row);
  }
  renderAgentHints();
}

function renderAgentHints() {
  const a = agentInfo(state.newAgent);
  $('#new-start-label').textContent = `Start a ${a.label} session in the workspace`;
  $('#new-danger-flag').textContent = a.dangerFlag;
  const hint = $('#perm-hint');
  hint.innerHTML = '';
  hint.appendChild(document.createTextNode(`Default is ${a.label}'s `));
  hint.appendChild(el('code', null, a.safeLabel));
  hint.appendChild(document.createTextNode(
    ' mode. Skipping lets the agent run anything in this workspace without asking.'));
}

// The folder the task will open, under the chosen computer's workspace root
// (it used to name a fixed path whatever config.json said).
function updateSlugPreview() {
  const s = slugify($('#new-name').value);
  const root = ((state.newConfig && state.newConfig.workspaceRoot) || '')
    .replace(/^(\/Users|\/home)\/[^/]+/, '~').replace(/\/+$/, '');
  $('#slug-preview').textContent = s ? (root ? root + '/' : '') + s : '—';
}

$('#new-name').addEventListener('input', async e => {
  updateSlugPreview();
  // Once the branch has been typed in by hand it is the user's, and a later
  // keystroke in the name must not quietly overwrite it.
  if (state.branchEdited) return;
  const name = e.target.value;
  let b = '';
  try { b = (await api.get('/api/suggest-branch?name=' + encodeURIComponent(name))).branch; }
  catch { return; }
  if (state.branchEdited) return;          // the await gave the user time to type
  if ($('#new-name').value !== name) return; // a newer keystroke owns the field
  $('#new-branch').value = b;
  setBranchHint(b);
});

$('#new-branch').addEventListener('input', () => {
  state.branchEdited = true;
  setBranchHint($('#new-branch').value.trim());
});

// setBranchHint reports only what is locally checkable; whether the branch
// already exists is decided per repo at creation and reported in the result.
function setBranchHint(b) {
  const h = $('#branch-hint');
  h.classList.remove('bad');
  if (!b) {
    h.textContent = 'Created in every selected repository, and what your commits get pushed to. '
      + 'Suggested from the name — edit it and the suggestion stops.';
    return;
  }
  if (/\s/.test(b) || /\.\./.test(b) || /^[-/]/.test(b) || /[/]$/.test(b) || /[~^:?*\[\\]/.test(b)) {
    h.textContent = 'Not a valid git branch name.';
    h.classList.add('bad');
    return;
  }
  h.textContent = 'Branch ' + b + ' in every selected repository. If it already exists there, '
    + 'it is continued rather than reset.';
}

// mirrors store.Slugify on the server
function slugify(name) {
  let out = '', prevDash = false;
  for (const ch of name) {
    if (/[a-z0-9._]/.test(ch)) { out += ch; prevDash = false; }
    else if (/[A-Z]/.test(ch)) { out += ch.toLowerCase(); prevDash = false; }
    else if (!prevDash && out.length) { out += '-'; prevDash = true; }
  }
  return out.replace(/-+$/, '').slice(0, 64);
}

$('#modal-cancel').addEventListener('click', () => $('#modal').classList.add('hidden'));

// Enter in a text field creates the task, as the hint in the footer says.
$('#modal').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.isComposing && e.target.matches('input[type=text]')) {
    e.preventDefault();
    $('#modal-create').click();
  }
});

$('#modal-create').addEventListener('click', async () => {
  const btn = $('#modal-create');
  if (btn.disabled) return;
  const name = $('#new-name').value.trim();
  const repos = [];
  for (const cb of $('#repo-picker').querySelectorAll('input[type=checkbox]')) {
    if (!cb.checked) continue;
    const sel = $('#repo-picker').querySelector(`select[data-repo="${CSS.escape(cb.dataset.repo)}"]`);
    repos.push({ name: cb.dataset.repo, baseRef: sel ? sel.value : '' });
  }
  const err = $('#modal-error');
  err.textContent = '';
  if (!name) { err.textContent = 'Give the task a name.'; $('#new-name').focus(); return; }
  if (!repos.length) { err.textContent = 'Select at least one repository.'; return; }

  btn.disabled = true; btn.textContent = 'Creating…';
  try {
    const t = await api.post(nodeBase(state.newNode) + '/tasks', {
      name, repos,
      agent: state.newAgent,
      branch: $('#new-branch').value.trim(),
      profile: state.newProfile,
      gitIdentity: $('#git-line').classList.contains('hidden') ? '' : $('#new-git').value,
      startAgent: $('#new-start').checked,
      dangerous: $('#new-dangerous').checked,
    });
    $('#modal').classList.add('hidden');
    await loadBoard();
    await openTask(t.slug, state.newNode);
  } catch (e) {
    err.textContent = e.message;
  }
  btn.disabled = false; btn.textContent = 'Create task';
});

/* ------------------------------------------------------------------ boot */

// The board is only shown once it has something to show, which takes a round
// trip. Anything the user navigates to in the meantime wins: showing the board
// on top of a view they explicitly asked for reads as the click being lost.
// Before setup there is nothing to show but how to do it, so the setup view
// replaces the board until config.json exists and loads.
async function boot() {
  const c = await loadConfig();
  document.body.classList.toggle('unconfigured', !!(c && !c.configured));
  if (c && !c.configured) {
    renderSetup();
    showView('setup');
    return;
  }
  await loadBoard();
  if (!state.navigated) showView('board');
}

async function renderSetup() {
  const c = state.config || {};
  $('#setup-path').textContent = c.path || 'config.json';
  const host = $('#setup-problems');
  host.innerHTML = '';
  if (c.error && !/not set up/.test(c.error)) host.appendChild(el('div', 'warn', c.error));
  for (const p of (c.problems || []).filter(p => p.error)) {
    host.appendChild(el('div', 'warn', (p.where ? p.where + ': ' : '') + p.msg));
  }
  let r = null;
  try { r = await api.post('/api/env', {}); } catch { /* ignore */ }
  const env = $('#setup-env');
  env.innerHTML = '';
  if (r) {
    for (const ch of r.checks || []) {
      const row = el('div', 'setup-check');
      row.appendChild(el('span', 'dot ' + (ch.ok ? 'good' : 'fail')));
      row.appendChild(el('b', null, ch.label));
      row.appendChild(document.createTextNode(ch.detail || ''));
      env.appendChild(row);
    }
  }
}

$('#setup-reload').addEventListener('click', async () => {
  const b = $('#setup-reload');
  b.disabled = true;
  try {
    await api.post('/api/config', {});
    // The setup page was shown by boot itself, not chosen by the user, so it
    // must not count as a choice that keeps the board from replacing it.
    state.navigated = false;
    await boot();
    if (state.config && !state.config.configured) toast('config.json is not set up yet.', 'info');
    else if (state.config && state.config.configured) {
      // The board's checks were last run before config.json existed, and the
      // server keeps that answer; ask again rather than show it.
      loadEnv(true);
      toast('FrIDE is set up.', 'ok');
    }
  } catch (e) { toast(e.message); }
  b.disabled = false;
});

boot().catch(e => {
  toast('Failed to load: ' + e.message, 'error', 0);
});

/* ------------------------------------------------------------------ project settings */

state.settings = null;

async function loadSettings() {
  state.settings = await api.get('/api/settings');
  await loadConfig();
  await loadGitIds('/api');
  renderSettings();
}

// Each identity with its token's state, and a field to set one. The token
// goes to this computer's server and is never shown again, here or anywhere.
function renderGitSettings() {
  const body = $('#git-table').querySelector('tbody');
  body.innerHTML = '';
  const def = state.gitDefaults['/api'] || '';
  for (const g of state.gitIds['/api'] || []) {
    const tr = el('tr');
    const name = el('td');
    const head = el('div', null, g.label);
    if (g.id === def) head.appendChild(el('span', 'tag', 'default'));
    if (g.managed) {
      const m = el('span', 'tag muted', 'companion');
      m.title = 'Set by the companion program that runs this FrIDE, not by config.json';
      head.appendChild(m);
    }
    name.appendChild(head);
    name.appendChild(el('div', 'path', g.id + ' · ' + g.kind));
    tr.appendChild(name);
    tr.appendChild(el('td', null, g.host));
    tr.appendChild(el('td', g.name || g.email ? null : 'hint',
      g.name || g.email ? [g.name, g.email && '<' + g.email + '>'].filter(Boolean).join(' ') : "git's own config"));

    const tok = el('td');
    const cell = el('div', 'copycell');
    cell.appendChild(el('span', 'tag' + (g.has_token ? ' green' : ' muted'), g.has_token ? 'set' : 'none'));
    const inp = el('input');
    inp.type = 'password';
    inp.autocomplete = 'new-password';
    inp.spellcheck = false;
    inp.placeholder = g.has_token ? 'replace it' : 'paste the token';
    inp.setAttribute('aria-label', 'Token for ' + g.label);
    const save = el('button', 'mini', 'Save');
    const store = async () => {
      if (!inp.value.trim()) { inp.focus(); return; }
      try { await api.put(`/api/git/identities/${encodeURIComponent(g.id)}/token`, { token: inp.value }); }
      catch (e) { toast(e.message); return; }
      inp.value = '';
      toast(`Token for ${g.label} saved. Sessions started from now on use it.`, 'ok');
      await loadGitIds('/api');
      renderGitSettings();
    };
    save.addEventListener('click', store);
    inp.addEventListener('keydown', e => { if (e.key === 'Enter') store(); });
    cell.appendChild(inp);
    cell.appendChild(save);
    if (g.has_token) {
      const rm = el('button', 'mini', 'Remove');
      rm.addEventListener('click', async () => {
        if (!await ask({ title: `Remove the token for ${g.label}?`,
                         body: `Tasks with this identity can no longer push to ${g.host} until a new one is set.`,
                         ok: 'Remove token', danger: true })) return;
        try { await api.del(`/api/git/identities/${encodeURIComponent(g.id)}/token`); }
        catch (e) { toast(e.message); return; }
        await loadGitIds('/api');
        renderGitSettings();
      });
      cell.appendChild(rm);
    }
    tok.appendChild(cell);
    tr.appendChild(tok);
    body.appendChild(tr);
  }
  if (!(state.gitIds['/api'] || []).length) {
    const tr = el('tr');
    const td = el('td', 'empty', 'No git identities. List them in config.json under git.identities (docs/GIT.md).');
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
  }
}

function renderConfig() {
  const c = state.config;
  if (!c) return;
  $('#cfg-path').textContent = c.path;
  const host = $('#profile-list');
  host.innerHTML = '';
  for (const p of c.profiles || []) {
    const row = el('div', 'profrow');
    const head = el('div');
    head.appendChild(el('b', null, p.name));
    if (p.name === c.defaultProfile) head.appendChild(el('span', 'tag', 'default'));
    row.appendChild(head);
    if (p.description) row.appendChild(el('div', 'desc', p.description));
    row.appendChild(el('div', 'path', 'services: ' + p.services.join(', ') + (p.entry ? ' · entry: ' + p.entry : '')));
    host.appendChild(row);
  }
  if (!(c.profiles || []).length) host.appendChild(el('div', 'hint', 'No profiles yet.'));
  const pr = $('#cfg-problems');
  pr.innerHTML = '';
  for (const p of c.problems || []) {
    pr.appendChild(el('div', p.error ? 'warn' : 'hint', (p.error ? 'error · ' : 'warning · ') + (p.where ? p.where + ': ' : '') + p.msg));
  }
}

$('#cfg-reload').addEventListener('click', async () => {
  const b = $('#cfg-reload');
  b.disabled = true;
  try {
    await api.post('/api/config', {});
    await loadSettings();
    toast('Reloaded config.json.', 'ok');
  } catch (e) { toast(e.message); }
  b.disabled = false;
});

// putSettings sends one change and redraws from the answer; a refusal is said,
// and the page is redrawn from what the server still holds.
async function putSettings(change) {
  try { state.settings = await api.put('/api/settings', change); }
  catch (e) {
    toast(e.message);
    try { state.settings = await api.get('/api/settings'); } catch { /* keep what we have */ }
  }
  renderSettings();
}

function renderSettings() {
  const d = state.settings;
  if (!d) return;
  renderConfig();
  renderGitSettings();

  const bl = $('#base-list');
  bl.innerHTML = '';
  for (const b of d.baseFolders || []) {
    const n = (d.projects || []).filter(p => p.base === b).length;
    const row = el('div', 'baserow');
    row.appendChild(icon('folder'));
    row.appendChild(el('span', null, b));
    row.appendChild(el('span', 'sp'));
    row.appendChild(el('span', 'n', `${n} repositor${n === 1 ? 'y' : 'ies'}`));
    const rm = el('button', 'mini', 'Remove');
    rm.addEventListener('click', async () => {
      if (!await ask({ title: 'Stop scanning this folder?', body: `${b}\n\nIts repositories disappear from New task and from the stack.`,
                       ok: 'Remove folder', danger: true })) return;
      await putSettings({ removeBase: b });
    });
    row.appendChild(rm);
    bl.appendChild(row);
  }
  if (!(d.baseFolders || []).length) bl.appendChild(el('div', 'hint', 'No base folders yet — add the folder your repositories live in.'));

  const body = $('#proj-table').querySelector('tbody');
  body.innerHTML = '';
  for (const p of d.projects || []) {
    const tr = el('tr');

    const name = el('td');
    name.appendChild(el('div', null, p.name));
    name.appendChild(el('div', 'path', p.path));
    tr.appendChild(name);

    const core = el('td');
    if (p.core) core.appendChild(el('span', 'tag green', 'core'));
    tr.appendChild(core);

    // copy list: a comma-separated field, blank meaning "copy nothing"
    const copy = el('td');
    const cell = el('div', 'copycell');
    const inp = el('input');
    inp.type = 'text';
    inp.value = (p.copy || []).join(', ');
    inp.placeholder = '.env, .env.properties';
    inp.spellcheck = false;
    inp.title = 'Files copied from the main checkout into this repo’s worktree in a new workspace';
    const save = async (reset) => {
      const list = reset ? null
        : inp.value.split(',').map(x => x.trim()).filter(Boolean);
      await putSettings(reset ? { project: p.name, resetCopy: true } : { project: p.name, copy: list });
    };
    inp.addEventListener('change', () => save(false));
    cell.appendChild(inp);
    if (p.overridden) {
      const o = el('span', 'ovr on', 'overridden');
      o.title = `.worktreeinclude says: ${(p.default || []).join(', ') || '(empty)'}`;
      cell.appendChild(o);
      const r = el('button', 'mini', 'reset');
      r.title = 'Go back to the repo’s own .worktreeinclude';
      r.addEventListener('click', () => save(true));
      cell.appendChild(r);
    } else {
      cell.appendChild(el('span', 'ovr', 'from .worktreeinclude'));
    }
    copy.appendChild(cell);
    tr.appendChild(copy);

    // IDE: which JetBrains IDE `Start` hands a service of this repo to
    const idec = el('td');
    const sel = el('select');
    const auto = el('option', null, p.ideDetected ? `auto (${p.ideDetected})` : 'auto (none detected)');
    auto.value = '';
    sel.appendChild(auto);
    for (const name of (d.ides || [])) {
      const o = el('option', null, name);
      o.value = name;
      if (name === p.ide) o.selected = true;
      sel.appendChild(o);
    }
    sel.addEventListener('change', () => putSettings({ project: p.name, ide: sel.value }));
    idec.appendChild(sel);
    tr.appendChild(idec);

    body.appendChild(tr);
  }
  if (!(d.projects || []).length) {
    const tr = el('tr');
    const td = el('td', 'empty', 'No git repositories found under the base folders.');
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
  }
}

async function addBase() {
  const v = $('#base-input').value.trim();
  if (!v) { $('#base-input').focus(); return; }
  try {
    state.settings = await api.put('/api/settings', { addBase: v });
    $('#base-input').value = '';
    renderSettings();
  } catch (e) { toast(e.message); }
}
$('#base-add').addEventListener('click', addBase);
$('#base-input').addEventListener('keydown', e => { if (e.key === 'Enter') addBase(); });

$('#nav-settings').addEventListener('click', async () => {
  leaveTask(); showView('settings');
  try { await loadSettings(); } catch (e) { toast(e.message); }
});

/* ------------------------------------------------------------------ orchestrator */

/* The orchestrator is one agent above the per-task ones: it turns a sentence
   into a workspace, and — in orchestrate mode — watches the ones you pick,
   answers what their agents stop on and escalates the rest here.

   The whole view is a poll. A turn happens on the server's own clock (a
   scheduled look, an agent that just stopped), so there is nothing for the page
   to await; it redraws whatever the last poll said. */

const ORCH_MODES = [
  { id: 'chat', label: 'Chat',
    detail: 'It acts only when you say something. Use it to start workspaces.' },
  { id: 'orchestrate', label: 'Orchestrate',
    detail: 'It also watches the workspaces below on a timer, and keeps them moving.' },
];

const ORCH_AUTONOMY = [
  { id: 'auto', label: 'Go ahead',
    detail: 'It answers the agents itself, and asks you only when the decision is really yours.' },
  { id: 'ask', label: 'Ask me first',
    detail: 'It describes what it would do and waits. Replying here is what runs it.' },
];

const orch = {
  poll: null,
  view: null,
  // The pending mode/watch choices, which are only sent on Apply — toggling a
  // checkbox must not silently put an agent in charge of a workspace.
  mode: 'chat',
  autonomy: 'auto',
  watching: new Set(),
  // lastSeen keeps the log from being rebuilt (and scrolled) when nothing has
  // changed, which would otherwise fight with reading it.
  lastSeen: -1,
};

function startOrch() {
  loadOrch(true);
  stopOrch();
  orch.poll = setInterval(() => loadOrch(false), 2000);
}
function stopOrch() { if (orch.poll) { clearInterval(orch.poll); orch.poll = null; } }

async function loadOrch(adopt) {
  let v;
  try { v = await api.get('/api/orchestrator'); }
  catch (e) { $('#orch-error').textContent = e.message; return; }
  orch.view = v;
  if (adopt) {
    // The server's setup is the truth on arrival; after that the boxes belong
    // to whoever is clicking them.
    orch.mode = v.mode;
    orch.autonomy = v.autonomy;
    orch.watching = new Set(v.watching || []);
    renderOrchPickers();
  }
  renderOrchTasks();
  renderOrchStatus(v);
  renderOrchLog(v);
}

function renderOrchPickers() {
  radioGroup($('#orch-mode'), 'orchmode', ORCH_MODES, orch.mode, id => {
    orch.mode = id; renderOrchTasks();
  });
  radioGroup($('#orch-autonomy'), 'orchauto', ORCH_AUTONOMY, orch.autonomy, id => {
    orch.autonomy = id;
  });
}

// radioGroup draws the same labelled-radio list the New task dialog uses, so
// the two read as one control rather than two dialects.
function radioGroup(host, name, items, selected, onPick) {
  host.innerHTML = '';
  for (const it of items) {
    const row = el('label', 'agent-pick');
    const rb = el('input');
    rb.type = 'radio'; rb.name = name; rb.value = it.id;
    rb.checked = it.id === selected;
    rb.addEventListener('change', () => onPick(it.id));
    row.appendChild(rb);
    const nm = el('div');
    nm.appendChild(document.createTextNode(it.label));
    nm.appendChild(el('small', null, it.detail));
    row.appendChild(nm);
    host.appendChild(row);
  }
}

function renderOrchTasks() {
  const host = $('#orch-tasks');
  const tasks = (orch.view && orch.view.tasks) || [];
  host.innerHTML = '';
  if (!tasks.length) {
    host.appendChild(el('div', 'jrmeta',
      'No open workspaces yet — ask for one in the chat.'));
    return;
  }
  for (const t of tasks) {
    const row = el('label', 'orch-task');
    const cb = el('input');
    cb.type = 'checkbox';
    cb.checked = orch.watching.has(t.slug);
    // Only orchestrate mode acts on the selection, so in chat mode the boxes
    // are shown but inert rather than hidden — the choice survives the switch.
    cb.disabled = orch.mode !== 'orchestrate';
    cb.addEventListener('change', () => {
      if (cb.checked) orch.watching.add(t.slug); else orch.watching.delete(t.slug);
    });
    row.appendChild(cb);
    const nm = el('div');
    nm.appendChild(document.createTextNode(t.name));
    const bits = [t.status.replace('_', ' ')];
    if (t.stackUp) bits.push('stack up' + (t.entry ? ' · ' + String(t.entry).replace(/^https?:\/\//, '') : ''));
    if (t.attention) bits.push('agent waiting');
    const sub = el('small', null, bits.join(' · '));
    if (t.attention) sub.classList.add('attn-text');
    nm.appendChild(sub);
    row.appendChild(nm);
    host.appendChild(row);
  }
}

function renderOrchStatus(v) {
  const host = $('#orch-status');
  host.innerHTML = '';
  const on = v.mode === 'orchestrate';
  const badge = el('span', 'badge' + (on ? ' up' : ''), on ? 'orchestrating' : 'chat');
  host.appendChild(badge);
  if (on) {
    host.appendChild(el('span', 'orch-meta',
      ((v.watching || []).join(', ') || 'nothing watched') + ' · ' +
      (v.autonomy === 'ask' ? 'asks before acting' : 'acts on its own')));
  }
  if (v.thinking) host.appendChild(el('span', 'orch-meta think', 'thinking…'));
  else if (v.awaiting) host.appendChild(el('span', 'orch-meta wait', 'waiting for your answer'));
  $('#orch-error').textContent = v.error || '';
}

function renderOrchLog(v) {
  const host = $('#orch-log');
  const msgs = v.messages || [];
  const newest = msgs.length ? msgs[msgs.length - 1].id : 0;
  const thinking = v.thinking ? 1 : 0;
  const key = newest * 2 + thinking;
  if (key === orch.lastSeen) return;
  // Only stick to the bottom when already there, so reading back through the
  // trail is not yanked away by the next scheduled look.
  const atBottom = host.scrollHeight - host.scrollTop - host.clientHeight < 60;
  orch.lastSeen = key;
  host.innerHTML = '';
  if (!msgs.length && !v.thinking) {
    const empty = el('div', 'orch-empty');
    empty.appendChild(icon('bot', 'big'));
    empty.appendChild(el('div', null,
      'Ask for a workspace: name the repositories, the branch, and what it starts from. ' +
      'Then switch to Orchestrate to have it keep them moving.'));
    host.appendChild(empty);
  }
  for (const m of msgs) host.appendChild(orchMessage(m));
  if (v.thinking) host.appendChild(el('div', 'orch-msg think', 'thinking…'));
  if (atBottom || !host.scrollTop) host.scrollTop = host.scrollHeight;
}

function orchMessage(m, opts = {}) {
  const row = el('div', 'orch-msg ' + m.role);
  const head = el('div', 'orch-head');
  head.appendChild(el('span', 'orch-who', {
    user: 'You', orchestrator: 'Orchestrator', event: 'did', error: 'failed',
  }[m.role] || m.role));
  if (m.task) head.appendChild(el('span', 'pill', m.task));
  // Where an inbox thread is involved, say which: the chat interleaves them.
  if (opts.threads !== false && (m.threads || []).length) {
    head.appendChild(el('span', 'pill inbox-pill', 'inbox ' + m.threads.map(id => '#' + id).join(' ')));
  }
  head.appendChild(el('span', 'orch-time', m.time
    ? new Date(m.time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''));
  row.appendChild(head);
  row.appendChild(el('div', 'orch-text', m.text));
  return row;
}

async function sendOrch() {
  const box = $('#orch-input'), text = box.value.trim();
  if (!text) return;
  const btn = $('#orch-send');
  btn.disabled = true;
  try {
    await api.post('/api/orchestrator/say', { text });
    box.value = '';
    growCompose();
    $('#orch-error').textContent = '';
    await loadOrch(false);
  } catch (e) { $('#orch-error').textContent = e.message; }
  btn.disabled = false;
  box.focus();
}

// The compose box grows with what is typed, up to a limit, then scrolls.
function growCompose() {
  const box = $('#orch-input');
  box.style.height = 'auto';
  box.style.height = Math.min(box.scrollHeight, 240) + 'px';
}

$('#orch-send').addEventListener('click', sendOrch);
$('#orch-input').addEventListener('input', growCompose);
$('#orch-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendOrch(); }
});

$('#orch-apply').addEventListener('click', async () => {
  const b = $('#orch-apply');
  b.disabled = true;
  try {
    await api.put('/api/orchestrator/mode', {
      mode: orch.mode, autonomy: orch.autonomy, watching: [...orch.watching],
    });
    $('#orch-error').textContent = '';
    await loadOrch(true);
    toast(orch.mode === 'orchestrate' ? 'Orchestrating the selected workspaces.' : 'Back to chat.', 'ok');
  } catch (e) { $('#orch-error').textContent = e.message; }
  b.disabled = false;
});

$('#orch-reset').addEventListener('click', async () => {
  if (!await ask({ title: 'Clear the conversation?', body: 'The orchestrator forgets the conversation and starts a new one. What it is supervising stays.',
                   ok: 'Clear chat', danger: true })) return;
  try { await api.post('/api/orchestrator/reset'); orch.lastSeen = -1; await loadOrch(true); }
  catch (e) { $('#orch-error').textContent = e.message; }
});


/* ------------------------------------------------------------------ inbox */

/* The inbox is the orchestrator's door to the outside: messages sent with
   `fride in` or POST /api/inbox by a script, a chat bridge or an assistant.
   Each one is a thread, and the thread keeps what came of it — the answer, the
   task it went to, that task's news — because whoever sent it was not here to
   watch. Like the orchestrator's view, this is a poll. */

const INBOX_STATUS = {
  queued: 'queued', routing: 'reading', active: 'following', needs_reply: 'needs a reply',
  answered: 'answered', done: 'done', closed: 'closed', failed: 'failed',
};

const inbox = {
  poll: null,
  threads: [],
  // selected is the open thread; 0 is "New message", which writes to the
  // inbox as if from outside — the quickest way to see what a relay would.
  selected: 0,
  listKey: '',
  logKey: '',
};

function startInbox() {
  stopInbox();
  inbox.listKey = inbox.logKey = '';
  loadInbox();
  inbox.poll = setInterval(loadInbox, 2000);
}
function stopInbox() { if (inbox.poll) { clearInterval(inbox.poll); inbox.poll = null; } }

async function loadInbox() {
  let list;
  try { list = await api.get('/api/inbox'); }
  catch (e) { $('#inbox-error').textContent = e.message; return; }
  inbox.threads = list || [];
  if (inbox.selected && !inbox.threads.some(t => t.id === inbox.selected)) inbox.selected = 0;
  renderInboxList();
  await loadThread();
}

function renderInboxList() {
  const host = $('#inbox-list');
  const key = JSON.stringify([inbox.selected, inbox.threads.map(t =>
    [t.id, t.status, t.updated, (t.tasks || []).join(',')])]);
  if (key === inbox.listKey) return;
  inbox.listKey = key;
  host.innerHTML = '';
  if (!inbox.threads.length) {
    host.appendChild(el('div', 'jrmeta', 'Nothing has arrived yet.'));
    return;
  }
  for (const t of inbox.threads) {
    const row = el('button', 'inbox-row' + (t.id === inbox.selected ? ' active' : ''));
    row.type = 'button';
    const top = el('div', 'inbox-row-h');
    top.appendChild(el('span', 'inbox-id', '#' + t.id));
    top.appendChild(el('span', 'inbox-src', t.source));
    top.appendChild(el('span', 'inbox-st st-' + t.status, INBOX_STATUS[t.status] || t.status));
    row.appendChild(top);
    row.appendChild(el('div', 'inbox-txt', t.text));
    const meta = [whenShort(t.updated)];
    if ((t.tasks || []).length) meta.push(t.tasks.join(', '));
    row.appendChild(el('small', null, meta.join(' · ')));
    row.addEventListener('click', () => selectThread(t.id));
    host.appendChild(row);
  }
}

function selectThread(id) {
  inbox.selected = id;
  inbox.logKey = '';
  $('#inbox-error').textContent = '';
  renderInboxList();
  loadThread();
  $('#inbox-input').focus();
}

async function loadThread() {
  const id = inbox.selected;
  if (!id) { renderNewMessage(); return; }
  let v;
  try { v = await api.get('/api/inbox/' + id); }
  catch (e) { $('#inbox-error').textContent = e.message; return; }
  if (id === inbox.selected) renderThread(v);
}

function renderThread(v) {
  const head = $('#inbox-head');
  head.innerHTML = '';
  head.appendChild(el('span', 'badge' + (v.status === 'active' ? ' up' : ''), INBOX_STATUS[v.status] || v.status));
  head.appendChild(el('span', 'orch-meta', `#${v.id} from ${v.source}` + (v.ref ? ` (${v.ref})` : '')));
  for (const slug of v.tasks || []) {
    const done = (v.finished || []).includes(slug);
    const b = el('button', 'chip' + (done ? '' : ' node remote'), slug + (done ? ' · closed' : ''));
    b.type = 'button';
    b.title = done ? 'This task has been closed' : 'Open this task';
    if (!done) b.addEventListener('click', () => openTask(slug, ''));
    head.appendChild(b);
  }
  if (v.position) head.appendChild(el('span', 'orch-meta think',
    v.position === 1 ? 'next to be read' : `${v.position - 1} ahead of it in the queue`));
  if (v.status === 'routing') head.appendChild(el('span', 'orch-meta think', 'reading…'));
  if (v.status === 'needs_reply') head.appendChild(el('span', 'orch-meta wait', 'waiting for your reply'));
  $('#inbox-error').textContent = v.error ? 'Last attempt failed: ' + v.error : '';

  const log = $('#inbox-log');
  const msgs = v.messages || [];
  const key = [v.id, v.status, msgs.length ? msgs[msgs.length - 1].id : 0].join(':');
  if (key !== inbox.logKey) {
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
    inbox.logKey = key;
    log.innerHTML = '';
    if (!msgs.length) {
      // The trail is the orchestrator's chat filtered to this thread, and a
      // cleared chat takes it along; the message itself is kept on the thread.
      log.appendChild(orchMessage({ role: 'user', text: v.text, time: v.created }, { threads: false }));
    }
    for (const m of msgs) log.appendChild(orchMessage(m, { threads: false }));
    if (v.status === 'routing') log.appendChild(el('div', 'orch-msg think', 'reading…'));
    if (atBottom || !log.scrollTop) log.scrollTop = log.scrollHeight;
  }

  $('#inbox-input').placeholder = v.status === 'needs_reply'
    ? 'Answer its question'
    : 'Reply in this thread — more for its task, or a correction';
  setLabel($('#inbox-send'), 'Reply');
  $('#inbox-close').classList.toggle('hidden', v.status === 'closed');
}

function renderNewMessage() {
  const head = $('#inbox-head');
  head.innerHTML = '';
  head.appendChild(el('span', 'badge', 'new message'));
  head.appendChild(el('span', 'orch-meta', 'Sent as if from outside, with source "ui"'));
  $('#inbox-error').textContent = '';
  const log = $('#inbox-log');
  if (inbox.logKey !== 'new') {
    inbox.logKey = 'new';
    log.innerHTML = '';
    const empty = el('div', 'orch-empty');
    empty.appendChild(icon('inbox', 'big'));
    empty.appendChild(el('div', null,
      'Pick a thread to read what came of it. Or write a message here: the orchestrator ' +
      'routes it to the task it continues, starts a new task for it, or asks you back — ' +
      'exactly as it would for one relayed from your phone.'));
    log.appendChild(empty);
  }
  $('#inbox-input').placeholder = 'Fix the empty state of the search page in the frontend';
  setLabel($('#inbox-send'), 'Send');
  $('#inbox-close').classList.add('hidden');
}

// whenShort is a timestamp at the precision a list needs: the time today, the
// day otherwise.
function whenShort(t) {
  if (!t) return '';
  const d = new Date(t);
  return d.toDateString() === new Date().toDateString()
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { day: 'numeric', month: 'short' });
}

async function sendInbox() {
  const box = $('#inbox-input'), text = box.value.trim();
  if (!text) return;
  const btn = $('#inbox-send');
  btn.disabled = true;
  try {
    const v = inbox.selected
      ? await api.post(`/api/inbox/${inbox.selected}/reply`, { text })
      : await api.post('/api/inbox', { text, source: 'ui' });
    box.value = '';
    growBox(box);
    inbox.selected = v.id;
    inbox.listKey = inbox.logKey = '';
    $('#inbox-error').textContent = '';
    await loadInbox();
  } catch (e) { $('#inbox-error').textContent = e.message; }
  btn.disabled = false;
  box.focus();
}

function growBox(box) {
  box.style.height = 'auto';
  box.style.height = Math.min(box.scrollHeight, 240) + 'px';
}

$('#inbox-send').addEventListener('click', sendInbox);
$('#inbox-input').addEventListener('input', e => growBox(e.target));
$('#inbox-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); sendInbox(); }
});
$('#inbox-new').addEventListener('click', () => selectThread(0));
$('#inbox-close').addEventListener('click', async () => {
  const id = inbox.selected;
  if (!id) return;
  try {
    await api.post(`/api/inbox/${id}/close`);
    inbox.listKey = inbox.logKey = '';
    await loadInbox();
    toast(`Stopped following #${id}. Its tasks are left as they are.`, 'ok');
  } catch (e) { $('#inbox-error').textContent = e.message; }
});


/* ------------------------------------------------------------------ rationale */

// The journal is written by the agent while it works, never here: a reason
// recorded after the fact is a reconstruction, and this card cannot tell the
// two apart. So this is a reader — filter to a kind, open one entry, or open
// the whole document.
const JR_KINDS = ['plan', 'decision', 'assumption', 'issue', 'note'];

async function loadJournal(p = pin()) {
  const host = $('#jr-list'), sel = $('#jr-filter');
  if (!host) return;
  let entries = [];
  try { entries = await api.get(`${p.base}/tasks/${p.slug}/journal`); }
  catch (e) { if (stale(p)) return; host.innerHTML = ''; host.appendChild(el('div', 'warn', e.message)); return; }
  if (stale(p)) return;
  state.journal = entries || [];

  const counts = {};
  for (const e of state.journal) counts[e.kind] = (counts[e.kind] || 0) + 1;
  const keep = sel.value || 'all';
  sel.innerHTML = '';
  sel.appendChild(new Option(`All entries (${state.journal.length})`, 'all'));
  for (const k of JR_KINDS) {
    if (!counts[k]) continue;
    sel.appendChild(new Option(`${k[0].toUpperCase() + k.slice(1)}s (${counts[k]})`, k));
  }
  sel.value = [...sel.options].some(o => o.value === keep) ? keep : 'all';
  renderJournalList();
}

function renderJournalList() {
  const host = $('#jr-list'), kind = $('#jr-filter').value || 'all';
  const all = state.journal || [];
  host.innerHTML = '';
  if (!all.length) {
    host.appendChild(el('div', 'jrmeta',
      'Nothing recorded yet — the agent writes these with `fride note`.'));
    return;
  }
  const rows = all.filter(e => kind === 'all' || e.kind === kind).reverse();
  if (!rows.length) { host.appendChild(el('div', 'jrmeta', 'None of this kind.')); return; }
  for (const e of rows) {
    const row = el('button', 'jrline');
    if (kind === 'all') row.appendChild(el('span', 'jrkind ' + e.kind, e.kind));
    row.appendChild(el('span', 'jrtext', e.text));
    row.title = e.text;
    row.addEventListener('click', () => openEntry(e));
    host.appendChild(row);
  }
}

function openEntry(e) {
  const when = e.time
    ? new Date(e.time).toLocaleString([], {month:'short',day:'numeric',hour:'2-digit',minute:'2-digit'})
    : '';
  const head = $('#jrpop-head');
  head.innerHTML = '';
  head.appendChild(el('span', 'jrkind ' + e.kind, e.kind));
  head.appendChild(el('span', 'jrmeta', [when, e.repo, e.source].filter(Boolean).join(' · ')));
  $('#jrpop-text').textContent = e.text;
  const why = $('#jrpop-why');
  why.innerHTML = '';
  if (e.why) {
    why.appendChild(el('div', 'jrwhylabel', 'Why'));
    why.appendChild(el('div', 'jrwhy', e.why));
  } else {
    const n = el('div', 'jrmeta', 'No reason was recorded with this one.');
    n.style.padding = '14px 0 0';
    why.appendChild(n);
  }
  openModal('jrpop');
}

/* ------------------------------------------------------------------ idea */

// The idea page is written by the agent through the document-idea skill while
// an idea is being explored; this panel only shows it and publishes it. Nothing
// is pushed until the button is clicked. Hidden where no ideas repository is
// configured — the computer the task lives on answers that.
async function loadIdea(p = pin()) {
  const panel = $('#idea-panel'), host = $('#idea-list'), btn = $('#idea-publish');
  let r;
  try { r = await api.get(`${p.base}/tasks/${p.slug}/idea`); }
  catch { if (!stale(p)) panel.classList.add('hidden'); return; }
  if (stale(p)) return;
  panel.classList.toggle('hidden', !r.enabled);
  $('#idea-result').textContent = '';
  host.innerHTML = '';
  if (!r.pages.length) {
    host.appendChild(el('div', 'jrmeta',
      'No idea page yet — the agent writes one under .idea-brain/ when the task is exploring an idea.'));
  }
  for (const pg of r.pages) {
    const row = el('div', 'jrline');
    row.appendChild(el('span', 'jrkind note', pg.status || 'idea'));
    row.appendChild(el('span', 'jrtext', pg.title));
    row.title = pg.description || pg.path;
    host.appendChild(row);
  }
  btn.disabled = !r.pages.length;
}

$('#idea-refresh').addEventListener('click', ev => { ev.stopPropagation(); loadIdea(); });
$('#idea-publish').addEventListener('click', async () => {
  const b = $('#idea-publish'), out = $('#idea-result');
  const p = pin();
  b.disabled = true; b.textContent = 'Publishing…'; out.textContent = '';
  try {
    const r = await api.post(`${p.base}/tasks/${p.slug}/idea/publish`, {});
    if (stale(p)) return;
    if (r.skipped) out.textContent = r.skipped;
    else {
      out.appendChild(document.createTextNode('pushed ' + r.branch));
      if (r.link) {
        const a = el('a', null, ' → ' + r.link);
        a.href = r.link; a.target = '_blank'; a.rel = 'noreferrer';
        out.appendChild(a);
      }
    }
  } catch (e) { if (!stale(p)) out.textContent = e.message; }
  finally { b.disabled = false; b.textContent = 'Publish idea'; }
});

$('#jr-filter').addEventListener('change', renderJournalList);
$('#jr-refresh').addEventListener('click', ev => { ev.stopPropagation(); loadJournal(); });
$('#jrpop-close').addEventListener('click', () => $('#jrpop').classList.add('hidden'));
$('#docpop-close').addEventListener('click', () => $('#docpop').classList.add('hidden'));

async function showDoc() {
  const p = pin();
  const body = $('#docpop-body');
  openModal('docpop');
  // The ledger is configured per computer; the task's own computer answers
  // whether it has one.
  $('#docpop-publish').classList.toggle('hidden', !taskCfg().ledger);
  body.textContent = 'Composing…';
  $('#docpop-path').textContent = '';
  try {
    const r = await api.get(`${p.base}/tasks/${p.slug}/rationale`);
    if (stale(p)) return;
    body.textContent = r.markdown;
    $('#docpop-path').textContent = r.path
      ? 'Writes to ' + r.path
      : 'No repo in this task can host the document.';
  } catch (e) { if (!stale(p)) body.textContent = e.message; }
}

$('#jr-open').addEventListener('click', showDoc);

// Writing costs a file in the repo, and the narrative costs tokens, so both
// stay explicit rather than happening because the document was looked at.
$('#docpop-write').addEventListener('click', async () => {
  const b = $('#docpop-write'), narrate = $('#docpop-narrate').checked;
  const p = pin();
  b.disabled = true;
  b.textContent = narrate ? 'Writing (the narrative takes a moment)…' : 'Writing…';
  try {
    const r = await api.post(`${p.base}/tasks/${p.slug}/rationale`, { narrate });
    $('#docpop-path').textContent = 'Wrote ' + r.path;
    if (narrate) await showDoc();
  } catch (e) { $('#docpop-path').textContent = e.message; }
  b.disabled = false; b.textContent = 'Write to the repo';
});

// Publishing writes the document into the task too, then pushes it to the
// ledger repository configured in config.json.
$('#docpop-publish').addEventListener('click', async () => {
  const b = $('#docpop-publish'), narrate = $('#docpop-narrate').checked;
  const p = pin();
  b.disabled = true;
  b.textContent = 'Publishing…';
  try {
    const r = await api.post(`${p.base}/tasks/${p.slug}/rationale`, { narrate, publish: true });
    const pub = r.published || {};
    const host = $('#docpop-path');
    host.innerHTML = '';
    host.appendChild(document.createTextNode('Wrote ' + r.path + ' · '));
    if (pub.skipped) host.appendChild(document.createTextNode('ledger: ' + pub.skipped));
    else {
      host.appendChild(document.createTextNode(`ledger: pushed ${pub.path} on ${pub.branch}`));
      if (pub.link) {
        const a = el('a', null, ' → ' + pub.link);
        a.href = pub.link; a.target = '_blank'; a.rel = 'noreferrer';
        host.appendChild(a);
      }
      if (pub.waiting) host.appendChild(document.createTextNode(' (' + pub.waiting + ')'));
    }
  } catch (e) { $('#docpop-path').textContent = e.message; }
  b.disabled = false; b.textContent = 'Publish to the ledger';
});
