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

const state = {
  tasks: [],
  repos: [],
  agents: [],          // agent CLIs and whether this machine has them
  agentDefault: 'claude',
  newAgent: 'claude',  // the choice in the New task modal
  taskAgent: 'claude', // the open task's agent
  current: null,       // task slug
  terms: new Map(),    // sessionID -> {term, fit, ws, pane}
  activeTerm: null,
  filePath: '',
  showHidden: false,
  poll: null,
  navigated: false,   // the user has chosen a view; the boot must not override it
  viaIde: new Set(),   // services to open in their IDE on the next Start
  config: null,        // /api/config: profiles, core repos, whether set up
  newProfile: '',      // the choice in the New task modal
  fleet: { enabled: false, self: '', peers: [], mirrors: [], state: {} },
  currentNode: '',     // the computer the open task lives on ('' = this one)
  newNode: '',         // the computer chosen in the New task modal
};

/* ------------------------------------------------------------------ fleet */

// A task lives on one computer; its API is reached through this computer's
// server, which forwards to the owner. '' or this computer's own name means here.
function isLocal(node) { return !node || node === state.fleet.self; }
function nodeBase(node) { return isLocal(node) ? '/api' : '/api/node/' + encodeURIComponent(node); }
function tb() { return nodeBase(state.currentNode); }

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
  for (const v of document.querySelectorAll('.view')) v.classList.remove('active');
  $('#' + name).classList.add('active');
  $('#nav-board').classList.toggle('active', name === 'board');
  $('#nav-orch').classList.toggle('active', name === 'orch');
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
  host.innerHTML = '';
  // Done tasks are finished work — keeping them here would defeat the purpose
  const live = state.tasks.filter(t => t.status !== 'done' && !t.hidden);
  if (!live.length) { host.appendChild(el('span', 'empty', 'no open tasks')); return; }
  for (const t of live) {
    const here = t.slug === state.current && (isLocal(t.node) ? isLocal(state.currentNode) : t.node === state.currentNode);
    const chip = el('div', 'chip' + (here ? ' active' : '') +
                          (t.attention ? ' attn' : ''));
    if (t.attention) chip.appendChild(el('span', 'dot attn'));
    else if (t.stack && t.stack.up) chip.appendChild(el('span', 'dot stack'));
    chip.appendChild(el('span', null, t.name));
    chip.title = t.attention ? `${t.agent || 'the agent'} is waiting for input`
               : (t.stack && t.stack.up ? 'stack running' : t.status.replace('_', ' '));
    chip.addEventListener('click', () => openTask(t.slug, t.node));
    host.appendChild(chip);
  }
}

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
function renderColumns(tasks) {
  const hidden = tasks.filter(t => t.hidden);
  $('#hidden-count').textContent = hidden.length ? `${hidden.length} hidden` : 'nothing hidden';
  $('#toggle-hidden').textContent = state.showHidden ? 'Hide hidden' : 'Show hidden';
  $('#toggle-hidden').disabled = !hidden.length && !state.showHidden;
  for (const s of ['open', 'in_progress', 'done']) {
    const col = $('#col-' + s);
    col.innerHTML = '';
    // Reviewing hidden tasks shows ONLY those, so unhiding is a short list to
    // work through rather than hunting them among the visible ones.
    const items = tasks.filter(t => t.status === s && (state.showHidden ? t.hidden : !t.hidden));
    $('#count-' + s).textContent = items.length;
    for (const t of items) col.appendChild(card(t));
  }
}

async function loadBoard() {
  state.tasks = await api.get('/api/fleet/tasks');
  renderSwitcher();
  renderColumns(state.tasks);
  // Cached server-side, so this is a local round-trip unless the probe is stale.
  loadEnv(false);
}

function card(t) {
  const c = el('div', 'card' + (t.hidden ? ' hidden-task' : ''));
  c.draggable = true;
  c.dataset.slug = t.slug;
  c.appendChild(el('h3', null, t.name));
  const meta = el('div', 'meta');
  if (state.fleet.enabled) {
    meta.appendChild(el('span', 'pill node' + (isLocal(t.node) ? ' here' : ''), isLocal(t.node) ? 'this computer' : t.node));
  }
  for (const r of (t.repos || [])) meta.appendChild(el('span', 'pill', r.name));
  if (t.stack && t.stack.up) {
    meta.appendChild(el('span', 'pill up', t.stack.entryUrl
      ? 'UP ' + t.stack.entryUrl.replace(/^https?:\/\//, '')
      : `UP ${t.stack.running}/${t.stack.total}`));
  }
  c.appendChild(meta);
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
  const btns = el('div', 'cardbtns');
  const hb = el('button', 'mini', t.hidden ? 'unhide' : 'hide');
  hb.title = t.hidden
    ? 'Put this task back on the board'
    : 'Take this task off the board. Nothing is deleted: worktrees, branches and any running stack stay.';
  hb.addEventListener('click', async ev => {
    ev.stopPropagation();
    hb.disabled = true;
    try { await api.put(`${nodeBase(t.node)}/tasks/${t.slug}/hidden`, { hidden: !t.hidden }); }
    catch (e) { alert(e.message); }
    await loadBoard();
  });
  btns.appendChild(hb);

  const db = el('button', 'mini danger-mini', 'delete');
  db.title = 'Delete this task AND its workspace folder, including every worktree in it';
  db.addEventListener('click', ev => { ev.stopPropagation(); deleteTask(t, () => loadBoard()); });
  btns.appendChild(db);
  c.appendChild(btns);

  c.addEventListener('click', () => openTask(t.slug, t.node));
  c.addEventListener('dragstart', e => {
    c.classList.add('dragging');
    e.dataTransfer.setData('text/plain', JSON.stringify({ slug: t.slug, node: t.node || '' }));
  });
  c.addEventListener('dragend', () => c.classList.remove('dragging'));
  return c;
}

for (const col of document.querySelectorAll('.column')) {
  col.addEventListener('dragover', e => { e.preventDefault(); col.classList.add('dragover'); });
  col.addEventListener('dragleave', () => col.classList.remove('dragover'));
  col.addEventListener('drop', async e => {
    e.preventDefault();
    col.classList.remove('dragover');
    let ref = {};
    try { ref = JSON.parse(e.dataTransfer.getData('text/plain')); } catch { return; }
    if (!ref.slug) return;
    await setStatus(ref.slug, col.dataset.status, ref.node);
    await loadBoard();
  });
}

async function setStatus(slug, status, node) {
  try { await api.put(`${nodeBase(node)}/tasks/${slug}/status`, { status }); }
  catch (err) { alert(err.message); }
}

/* ------------------------------------------------------------------ task detail */

function findTask(slug, node) {
  return state.tasks.find(x => x.slug === slug && (isLocal(x.node) ? isLocal(node) : x.node === node));
}

async function openTask(slug, node) {
  state.current = slug;
  state.currentNode = isLocal(node) ? '' : node;
  state.filePath = '';
  state.viaIde.clear();
  stopOrch(); // reachable from the orchestrator view via the task switcher
  disposeTerms();
  showView('task');
  renderSwitcher();
  await refreshTask();
  await loadFiles('');
  const t = state.tasks.find(x => x.slug === slug);
  // each of these is independent — one failing must not abandon the others
  try { await syncTerminals(); } catch (e) { console.error('terminals:', e); }
  loadUsage().catch(e => console.error('usage:', e));
  loadJournal().catch(e => console.error('journal:', e));
  startPolling();
}

async function refreshTask() {
  const t = await api.get(tb() + '/tasks/' + state.current);
  $('#task-title').textContent = t.name;
  const badge = $('#task-status-badge');
  badge.textContent = t.status.replace('_', ' ');
  badge.className = 'badge ' + t.status;
  for (const b of document.querySelectorAll('[data-set-status]')) {
    b.classList.toggle('on', b.dataset.setStatus === t.status);
  }
  if (!state.agents.length) { try { await loadAgents(); } catch {} }
  state.taskAgent = t.agent || 'claude';
  const ag = agentInfo(state.taskAgent);
  const newagent = $('#btn-newagent');
  newagent.textContent = '+ ' + state.taskAgent;
  newagent.title = `New ${ag.label} session in the workspace`;
  // The profile is a property of the task, but it only takes effect at the
  // next stack up: the running stack was exec'd with the old commands, which
  // cannot be changed underneath it. The badge says so rather than implying it
  // switched.
  const modeBadge = $('#task-mode-badge');
  const profiles = (state.config && state.config.profiles) || [];
  const curProfile = (t.stack && t.stack.profile) || t.profile || (state.config && state.config.defaultProfile) || '';
  modeBadge.textContent = 'profile: ' + (curProfile || '?');
  modeBadge.classList.toggle('hidden', profiles.length === 0);
  modeBadge.onclick = async () => {
    if (profiles.length < 2) { alert('config.json has only one profile.'); return; }
    const names = profiles.map(p => p.name);
    const asked = prompt('Boot this task with which profile?\n\n' +
      profiles.map(p => `${p.name}${p.description ? ' — ' + p.description : ''}`).join('\n'), curProfile);
    if (asked === null || asked.trim() === curProfile) return;
    if (!names.includes(asked.trim())) { alert('No profile ' + asked); return; }
    try {
      await api.put(`${tb()}/tasks/${state.current}/profile`, { profile: asked.trim() });
    } catch (e) { alert(e.message); return; }
    await refreshTask();
    if (t.stack && t.stack.up) {
      alert('Switched to ' + asked.trim() + '. The running stack keeps the profile it started with — ' +
            'stop and start it to pick this up.');
    }
  };

  renderNodeBadge(t);

  const perm = $('#task-perm-badge');
  perm.textContent = t.dangerous ? ag.dangerLabel : ag.safeLabel;
  perm.classList.remove('hidden');
  perm.classList.toggle('perm', !!t.dangerous);
  perm.title = 'Click to switch permission mode for NEW agent sessions ' +
               '(a running one keeps the mode it started with)';
  perm.onclick = async () => {
    const next = !t.dangerous;
    if (next && !confirm(`Start future ${ag.label} sessions with ${ag.dangerFlag}?\n\n` +
                         'The agent will run anything in this workspace without asking.')) return;
    await api.put(`${tb()}/tasks/${state.current}/permissions`, { dangerous: next });
    await refreshTask();
  };
  renderStack(t.stack);
  if (!state.settings) { try { state.settings = await api.get('/api/settings'); } catch {} }
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
  const stats = await loadGit();
  renderRepos(t, stats);
  return t;
}

function renderStack(s) {
  const tb = $('#stack-table').querySelector('tbody');
  tb.innerHTML = '';
  const entry = $('#stack-entry');
  entry.innerHTML = '';
  if (s && s.error) {
    entry.appendChild(el('span', 'warn', s.error));
  } else if (!s || !s.k) {
    entry.appendChild(el('span', 'hint', 'not running — press Start'));
  } else if (s.entryUrl && !isLocal(state.currentNode)) {
    // The stack runs on another computer. Its URLs name that computer's
    // localhost; "Open here" serves the same ports on this one's.
    if (mirroredHere(state.currentNode, state.current)) {
      const a = el('a', null, s.entryUrl + '  (open the app — served from ' + state.currentNode + ')');
      a.href = s.entryUrl; a.target = '_blank';
      entry.appendChild(a);
      const stop = el('button', 'mini', 'stop opening here');
      stop.addEventListener('click', async () => {
        await api.del(`/api/fleet/mirror?node=${encodeURIComponent(state.currentNode)}&task=${encodeURIComponent(state.current)}`);
        await loadFleet();
        renderStack(s);
      });
      entry.appendChild(stop);
    } else {
      const open = el('button', 'primary mini', 'Open here');
      open.title = `Serve this stack's ports on this computer's localhost, so ${s.entryUrl} works here as it does on ${state.currentNode}`;
      open.addEventListener('click', async () => {
        open.disabled = true;
        try {
          const r = await api.post('/api/fleet/mirror', { node: state.currentNode, task: state.current });
          if (r.problems && r.problems.length) alert('Some ports could not be opened here:\n' + r.problems.join('\n'));
          await loadFleet();
          renderStack(s);
          if (r.entryUrl) window.open(r.entryUrl, '_blank');
        } catch (e) { alert(e.message); open.disabled = false; }
      });
      entry.appendChild(open);
      entry.appendChild(el('span', 'hint', ` running on ${state.currentNode}`));
    }
  } else if (s.entryUrl) {
    const a = el('a', null, s.entryUrl + '  (open the app)');
    a.href = s.entryUrl; a.target = '_blank';
    entry.appendChild(a);
  } else if (s.up) {
    entry.appendChild(el('span', 'hint', 'starting — the app link appears once its entry service listens'));
  }
  for (const sv of (s && s.services) || []) {
    const tr = el('tr');
    const nm = el('td');
    nm.appendChild(el('span', null, sv.name));
    nm.appendChild(el('small', 'svcmode ' + sv.mode, sv.mode));
    tr.appendChild(nm);
    tr.appendChild(el('td', 'port', sv.port ? ':' + sv.port : ''));
    const st = el('td', 'state');
    if (sv.running) st.appendChild(el('span', 'up', 'UP'));
    else if (sv.note) st.appendChild(el('span', 'busy', sv.note));
    else st.appendChild(el('span', 'down', sv.mode === 'absent' || sv.mode === 'off' ? '' : '—'));
    if (sv.log) {
      const lb = el('button', 'mini', 'log');
      lb.title = 'Show the end of this service\u2019s log';
      lb.addEventListener('click', () => showStackLog(sv.name));
      st.appendChild(lb);
    }
    tr.appendChild(st);
    tb.appendChild(tr);
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

function renderNodeBadge(t) {
  const badge = $('#task-node-badge');
  const mv = $('#btn-move');
  const on = state.fleet.enabled;
  badge.classList.toggle('hidden', !on);
  badge.textContent = 'on ' + (isLocal(state.currentNode) ? (state.fleet.self || 'this computer') : state.currentNode);
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
  $('#movepop').classList.remove('hidden');
});
$('#move-cancel').addEventListener('click', () => $('#movepop').classList.add('hidden'));
$('#move-go').addEventListener('click', async () => {
  const to = (document.querySelector('input[name=moveto]:checked') || {}).value;
  if (!to) return;
  const b = $('#move-go');
  b.disabled = true;
  $('#move-status').textContent = `Stopping the agent and the stack, then sending the task to ${to}…`;
  try {
    const r = await api.post(`${tb()}/tasks/${state.current}/move`, {
      to, startAgent: $('#move-agent').checked, startStack: $('#move-stack').checked,
    });
    $('#movepop').classList.add('hidden');
    const slug = state.current;
    await loadBoard();
    await openTask(slug, to === state.fleet.self ? '' : to);
    if (r.notes && r.notes.length) console.log('moved:', r.notes.join('\n'));
  } catch (e) {
    $('#move-status').textContent = e.message;
  }
  b.disabled = false;
});

function mirroredHere(node, task) {
  return (state.fleet.mirrors || []).some(m => m.node === node && m.task === task);
}

async function showStackLog(name) {
  const box = $('#stack-log');
  $('#stack-log-name').textContent = name;
  box.classList.remove('hidden');
  try {
    const r = await api.get(`${tb()}/tasks/${state.current}/stack/log?service=${encodeURIComponent(name)}&lines=200`);
    const pre = $('#stack-log-body');
    pre.textContent = r.log || '(empty)';
    pre.scrollTop = pre.scrollHeight;
  } catch (e) { $('#stack-log-body').textContent = e.message; }
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
    right.appendChild(el('span', 'br', (t.branches && t.branches[r.name]) || r.branch));
    if (st && (st.added || st.deleted)) {
      const ch = el('span', 'churn');
      ch.appendChild(el('span', 'add', '+' + st.added));
      ch.appendChild(document.createTextNode(' '));
      ch.appendChild(el('span', 'del', '\u2212' + st.deleted));
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

async function loadGit() {
  const host = $('#git-body');
  let stats;
  try { stats = await api.get(`${tb()}/tasks/${state.current}/git`); }
  catch (e) { host.textContent = e.message; return null; }
  host.innerHTML = '';
  let any = false;
  for (const st of stats || []) {
    if (!st.changed || !st.changed.length) continue;
    any = true;
    const box = el('div', 'grepo');
    box.appendChild(el('div', 'gname', `${st.repo} — ${st.changed.length} file(s)`));
    for (const f of st.changed.slice(0, 12)) {
      const row = el('div', 'gfile');
      row.appendChild(el('span', 'st ' + statusClass(f.status), f.status.trim() || '?'));
      const p = el('span', 'pth', f.path);
      p.title = f.path;
      row.appendChild(p);
      box.appendChild(row);
    }
    if (st.changed.length > 12) {
      box.appendChild(el('div', 'clean', `…and ${st.changed.length - 12} more`));
    }
    host.appendChild(box);
  }
  if (!any) host.appendChild(el('div', 'clean', 'all repos clean'));
  return stats;
}

$('#git-refresh').addEventListener('click', async () => {
  const stats = await loadGit();
  const t = await api.get(tb() + '/tasks/' + state.current);
  renderRepos(t, stats);
});

const fmt = n => (n || 0).toLocaleString();

async function loadUsage() {
  const host = $('#usage-body');
  host.textContent = 'loading…';
  let u;
  try { u = await api.get(`${tb()}/tasks/${state.current}/usage`); }
  catch (e) { host.textContent = e.message; return; }
  updateResumeButton(u);
  if (!u || !u.messages) {
    // Token counts are read out of Claude Code's own transcripts, so for a task
    // running another CLI the panel is empty by construction, not by accident.
    host.textContent = state.taskAgent === 'claude'
      ? 'no Claude activity recorded for this workspace yet'
      : `token counts come from Claude Code transcripts — this task runs ${agentInfo(state.taskAgent).label}`;
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

// Kept as the hook loadUsage() already calls; the list itself is driven by the
// server's view of which conversations are still unclaimed.
function updateResumeButton() {
  renderResumable().catch(e => console.error('resumable:', e));
}

$('#usage-refresh').addEventListener('click', loadUsage);

function startPolling() {
  stopPolling();
  state.poll = setInterval(async () => {
    if (!state.current) return;
    try {
      const s = await api.get(`${tb()}/tasks/${state.current}/stack`);
      renderStack(s);
      // While a setup is in flight the task view carries the phase, so keep
      // the detail panel honest without a full re-render.
      const inst = $('#install-state');
      if (inst && !inst.classList.contains('hidden')) await refreshTask();
    } catch { /* task may have been deleted */ }
  }, 4000);
}
function stopPolling() { if (state.poll) { clearInterval(state.poll); state.poll = null; } }

// The switcher must stay live on every view — the whole point is to notice a
// task that needs you while you are looking at a different one.
setInterval(async () => {
  try {
    const tasks = await refreshTasks();
      if ($('#board').classList.contains('active')) renderColumns(tasks);
    for (const [, e] of state.terms) markTabAttention(e);
  } catch { /* server restarting */ }
}, 3000);

// A prerequisite can break mid-session (docker stopped, a token expired), so
// this does not wait for the board to be reloaded. The server caches the probe.
setInterval(() => {
  if ($('#board').classList.contains('active')) loadEnv(false);
}, 120000);

// per-tab dot, so you can tell WHICH agent needs you inside a task
function markTabAttention(entry) {
  const tab = [...document.querySelectorAll('.tab')].find(t => t.dataset.id === entry.sess.id);
  if (!tab) return;
  const has = tab.querySelector('.tdot');
  const want = entry.sess.kind === 'agent' && entry.attention && state.activeTerm !== entry.sess.id;
  if (want && !has) tab.insertBefore(el('span', 'tdot'), tab.firstChild);
  if (!want && has) has.remove();
}

/* ------------------------------------------------------------------ terminals */

function disposeTerms() {
  for (const [, t] of state.terms) {
    try { t.ws.close(); } catch {}
    try { t.term.dispose(); } catch {}
    t.pane.remove();
  }
  state.terms.clear();
  state.activeTerm = null;
  for (const tab of document.querySelectorAll('.tab')) tab.remove();
}

async function syncTerminals() {
  const sessions = (await api.get(`${tb()}/tasks/${state.current}/terminals`)) || [];
  for (const s of sessions) {
    const e = state.terms.get(s.id);
    if (e) { e.attention = s.attention; e.sess = s; }
  }
  for (const s of sessions) if (!state.terms.has(s.id)) attachTerm(s);
  if (!state.activeTerm && sessions.length) selectTerm(sessions[0].id);
}

function attachTerm(sess) {
  const pane = el('div', 'term-pane');
  $('#term-host').appendChild(pane);

  const term = new Terminal({
    fontFamily: 'ui-monospace, Menlo, monospace',
    fontSize: 13,
    cursorBlink: true,
    scrollback: 5000,
    theme: termTheme(),
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(pane);

  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const nodeQ = isLocal(state.currentNode) ? '' : `&node=${encodeURIComponent(state.currentNode)}`;
  const ws = new WebSocket(`${proto}://${location.host}/ws/term?id=${encodeURIComponent(sess.id)}${nodeQ}`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = () => { doFit(); };
  ws.onmessage = ev => {
    const data = typeof ev.data === 'string' ? ev.data : new Uint8Array(ev.data);
    term.write(data);
  };
  ws.onclose = () => term.write('\r\n\x1b[90m[disconnected]\x1b[0m\r\n');

  term.onData(d => { if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'data', data: d })); });

  function doFit() {
    try { fit.fit(); } catch { return; }
    if (ws.readyState === 1) {
      ws.send(JSON.stringify({ type: 'resize', rows: term.rows, cols: term.cols }));
    }
  }

  const entry = { term, fit, ws, pane, doFit, sess, attention: false };
  state.terms.set(sess.id, entry);

  // tab
  const tab = el('button', 'tab');
  tab.dataset.id = sess.id;
  // the server titles a permission-skipping agent "<agent> !"
  if (sess.title && sess.title.endsWith('!')) tab.classList.add('danger-tab');
  tab.appendChild(el('span', null, sess.title || sess.kind));
  const x = el('span', 'x', '×');
  x.addEventListener('click', async ev => {
    ev.stopPropagation();
    await api.del(`${tb()}/tasks/${state.current}/terminals/${sess.id}`);
    const e = state.terms.get(sess.id);
    if (e) { try { e.ws.close(); } catch {} e.term.dispose(); e.pane.remove(); }
    state.terms.delete(sess.id);
    tab.remove();
    const first = state.terms.keys().next();
    state.activeTerm = null;
    if (!first.done) selectTerm(first.value);
  });
  tab.appendChild(x);
  tab.addEventListener('click', () => selectTerm(sess.id));
  tab.addEventListener('dblclick', ev => {
    ev.preventDefault();
    ev.stopPropagation();
    startRename(tab, sess);
  });
  tab.title = 'double-click to rename';
  $('#term-tabs').insertBefore(tab, $('#btn-newagent'));
}

// startRename swaps the tab label for an input. Enter or blur commits, Escape
// cancels; the label is restored either way so a failed request cannot leave the
// tab stuck in edit mode.
function startRename(tab, sess) {
  const label = tab.firstChild;
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
    const span = el('span', null, commit && next ? next : current);
    tab.replaceChild(span, input);
    if (!commit || !next || next === current) return;
    try {
      await api.put(`${tb()}/tasks/${state.current}/terminals/${sess.id}`, { title: next });
      sess.title = next;
    } catch (e) {
      span.textContent = current;   // server rejected it — do not lie in the UI
      console.error('rename:', e);
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
  $('#term-host').classList.remove('hidden');
  for (const [sid, e] of state.terms) {
    const on = sid === id;
    e.pane.classList.toggle('active', on);
    if (on) { e.doFit(); e.term.focus(); e.attention = false; markTabAttention(e); }
  }
  for (const tab of document.querySelectorAll('.tab')) {
    tab.classList.toggle('active', tab.dataset.id === id);
  }
}

window.addEventListener('resize', () => {
  const e = state.terms.get(state.activeTerm);
  if (e) e.doFit();
});

$('#btn-newagent').addEventListener('click', async () => {
  const b = $('#btn-newagent');
  b.disabled = true;
  try {
    const s = await api.post(`${tb()}/tasks/${state.current}/terminals`, { kind: 'agent' });
    attachTerm(s);
    selectTerm(s.id);
  } catch (e) { alert(e.message); }
  b.disabled = false;
});

// Sessions die with the server, but the conversation does not: Claude Code keeps
// a transcript per working directory, so --continue picks the agent back up.
// One button per recorded conversation that is not already live in a tab.
// Clicking resumes that specific transcript (claude --resume <id>), and the
// button goes away because the server then reports it as claimed.
//
// Claude Code only: the list is built from ~/.claude/projects, so a task running
// another agent has no buttons here at all.
//
// The task is pinned for the whole call. Every task renders into the same
// #resume-list, and this runs un-awaited off loadUsage(), so switching tasks
// while the fetch is in flight used to let the OLD task's buttons land in the
// NEW task's panel — and clicking one then asked the new task to resume a
// conversation belonging to the old one, which Claude Code answers with
// "No conversation found with session ID" and an immediately dead tab.
async function renderResumable() {
  const host = $('#resume-list');
  if (!host) return;
  const slug = state.current;
  host.innerHTML = '';
  if (!agentInfo(state.taskAgent).resumable) return;
  let list = [];
  try { list = await api.get(`${tb()}/tasks/${slug}/resumable`); } catch { return; }
  if (state.current !== slug) return; // switched away mid-flight: not ours to draw
  for (const c of list) {
    const b = el('button', 'mini resume' + (c.inUse ? ' inuse' : ''));
    const when = c.lastActivity ? new Date(c.lastActivity) : null;
    const stamp = when ? when.toLocaleString([], { month: 'short', day: 'numeric',
                                                  hour: '2-digit', minute: '2-digit' }) : '';
    // The title first, because it is the only part that says which conversation
    // this is. One tab routinely records several without you opening anything \u2014
    // /clear starts a new one, and so does changing the model or updating the
    // CLI \u2014 and as a row of bare timestamps they read as duplicates of the
    // session you are already in.
    const meta = `${stamp}${c.messages ? ' \u00b7 ' + c.messages + ' msgs' : ''}`;
    b.textContent = '\u27f2 ';
    if (c.title) {
      b.appendChild(el('span', 'rtitle', c.title));
      b.appendChild(el('span', 'rmeta', meta));
    } else {
      b.appendChild(document.createTextNode(meta));
    }
    // Still being written, by a tab that changed session id under us or by a
    // terminal outside fride. Shown rather than hidden \u2014 it is the
    // conversation you are probably looking for \u2014 but not startable: a second
    // --resume on it would give one transcript two writers.
    if (c.inUse) {
      b.disabled = true;
      // Appended, not assigned: setting textContent would discard the title and
      // meta spans built above and flatten the button back to one string.
      b.appendChild(el('span', 'rmeta', '\u00b7 in use'));
      b.title = `Conversation ${c.id} is open in another session`;
      host.appendChild(b);
      continue;
    }
    b.title = `Resume conversation ${c.id}`;
    b.addEventListener('click', async () => {
      // slug, not state.current: a button belongs to the task it was drawn for.
      if (state.current !== slug) return;
      b.disabled = true; b.textContent = 'resuming…';
      try {
        const sess = await api.post(`${tb()}/tasks/${slug}/terminals`,
                                    { kind: 'agent', resumeId: c.id });
        attachTerm(sess); selectTerm(sess.id);
      } catch (e) { alert(e.message); }
      await renderResumable();
    });
    host.appendChild(b);
  }
}
$('#btn-newterm').addEventListener('click', async () => {
  const s = await api.post(`${tb()}/tasks/${state.current}/terminals`, { kind: 'shell', dir: state.filePath });
  attachTerm(s);
  selectTerm(s.id);
});

/* ------------------------------------------------------------------ files */

async function loadFiles(path) {
  state.filePath = path;
  $('#files-path').textContent = '/' + (path || '');
  const list = $('#files-list');
  list.innerHTML = '';
  let entries;
  try { entries = await api.get(`${tb()}/tasks/${state.current}/files?path=${encodeURIComponent(path)}`); }
  catch (e) { list.appendChild(el('div', 'warn', e.message)); return; }
  for (const en of entries || []) {
    const row = el('div', 'file-row' + (en.isDir ? ' dir' : ''));
    row.appendChild(el('span', 'ic', en.isDir ? '▸' : '·'));
    row.appendChild(el('span', null, en.name));
    row.addEventListener('click', () => en.isDir ? loadFiles(en.path) : openFile(en.path));
    list.appendChild(row);
  }
}

$('#files-reveal').addEventListener('click', async () => {
  const b = $('#files-reveal');
  b.disabled = true;
  try { await api.post(`${tb()}/tasks/${state.current}/reveal?path=${encodeURIComponent(state.filePath || '')}`); }
  catch (e) { alert(e.message); }
  b.disabled = false;
});

$('#files-up').addEventListener('click', () => {
  const p = state.filePath;
  if (!p) return;
  const parts = p.split('/');
  parts.pop();
  loadFiles(parts.join('/'));
});

async function openFile(path) {
  let fc;
  try { fc = await api.get(`${tb()}/tasks/${state.current}/file?path=${encodeURIComponent(path)}`); }
  catch (e) { alert(e.message); return; }
  $('#file-view-name').textContent = path + (fc.truncated ? '  (truncated)' : '');
  $('#file-view-body').textContent = fc.withheld
    ? '[withheld: this file holds credentials]\n\n' +
      'The API is unauthenticated on loopback, so secret-bearing config is not served.\n' +
      'Open a shell tab and read it there if you need it.'
    : (fc.binary ? '[binary file]' : fc.content);
  $('#file-view').classList.remove('hidden');
  $('#term-host').classList.add('hidden');
  for (const tab of document.querySelectorAll('.tab')) tab.classList.remove('active');
}

$('#file-close').addEventListener('click', () => {
  $('#file-view').classList.add('hidden');
  $('#term-host').classList.remove('hidden');
  if (state.activeTerm) selectTerm(state.activeTerm);
});

/* ------------------------------------------------------------------ task actions */

// Which projects to hand to their IDE on the next Start. Reset per task.
function ideServices(t) {
  const name = (t.stack && t.stack.profile) || '';
  const p = ((state.config && state.config.profiles) || []).find(x => x.name === name);
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
  host.appendChild(el('div', 'idehdr', 'open in IDE instead of starting here:'));
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
  const b = $('#btn-up'); b.disabled = true; b.textContent = 'Starting...';
  try {
    const s = await api.post(`${tb()}/tasks/${state.current}/stack/up`, { viaIde: [...state.viaIde] });
    renderStack(s);
    await refreshTask();
  } catch (e) { alert(e.message); }
  b.disabled = false; b.textContent = 'Start';
});

$('#btn-down').addEventListener('click', async () => {
  const b = $('#btn-down'); b.disabled = true; b.textContent = 'Stopping...';
  try { renderStack(await api.post(`${tb()}/tasks/${state.current}/stack/down`)); }
  catch (e) { alert(e.message); }
  b.disabled = false; b.textContent = 'Stop';
});

for (const b of document.querySelectorAll('[data-set-status]')) {
  b.addEventListener('click', async () => {
    await setStatus(state.current, b.dataset.setStatus, state.currentNode);
    await refreshTask();
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
  let warn = `Delete "${detail.name || t.name}"?\n\nThis removes the whole workspace folder:\n  ${detail.workspace}\n` +
             `including every worktree in it. Commits already pushed are safe; anything else is not.`;
  if (dirty.length) {
    warn += `\n\nUncommitted changes: ` + dirty.map(([r, n]) => `${r} (${n})`).join(', ');
  }
  if (!confirm(warn)) return;
  try {
    await api.del(`${nodeBase(t.node)}/tasks/${t.slug}?purge=1`);
  } catch (e) { alert(e.message); return; }
  if (done) await done();
}

$('#btn-delete').addEventListener('click', async () => {
  const t = findTask(state.current, state.currentNode) || { slug: state.current, name: state.current, node: state.currentNode };
  await deleteTask(t, async () => {
    disposeTerms(); stopPolling();
    state.current = null;
    await loadBoard();
    showView('board');
  });
});

$('#back').addEventListener('click', async () => {
  stopPolling();
  await loadBoard();
  showView('board');
});

/* ------------------------------------------------------------------ instances */

async function loadInstances() {
  const rows = await api.get('/api/instances');
  const tb = $('#instances-table').querySelector('tbody');
  tb.innerHTML = '';
  for (const r of rows || []) {
    const tr = el('tr');
    tr.appendChild(el('td', null, String(r.k)));
    tr.appendChild(el('td', null, r.name + (r.exists ? '' : ' (deleted)')));
    tr.appendChild(el('td', null, Object.entries(r.ports || {}).map(([n, p]) => `${n}:${p}`).join(' ')));
    tr.appendChild(el('td', null, (r.listening || []).map(p => ':' + p).join(' ') || '(down)'));
    tb.appendChild(tr);
  }
}

$('#toggle-hidden').addEventListener('click', async () => {
  state.showHidden = !state.showHidden;
  await loadBoard();
});

$('#nav-board').addEventListener('click', async () => { stopPolling(); stopOrch(); await loadBoard(); showView('board'); });
$('#nav-instances').addEventListener('click', async () => { stopPolling(); stopOrch(); await loadInstances(); showView('instances'); });
$('#nav-computers').addEventListener('click', async () => { stopPolling(); stopOrch(); await loadComputers(); showView('computers'); });

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
    const td = el('td', 'hint', 'No other computers yet. Run `fride serve --fleet` on another one, signed in to the same account.');
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
      await api.del(`/api/fleet/mirror?node=${encodeURIComponent(m.node)}&task=${encodeURIComponent(m.task)}`);
      await loadComputers();
    });
    row.appendChild(x);
    mh.appendChild(row);
  }
  if (!(f.mirrors || []).length) mh.appendChild(el('div', 'hint', 'Nothing from another computer is open here.'));
}

$('#nav-orch').addEventListener('click', async () => { stopPolling(); showView('orch'); startOrch(); });

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
  bar.classList.remove('hidden');
  bar.className = 'envbar ' + (r.ok ? 'ok' : 'bad');
  bar.innerHTML = '';

  const head = el('div', 'envhead');
  head.appendChild(el('span', 'dot ' + (r.ok ? 'good' : 'fail')));
  head.appendChild(document.createTextNode(
    r.ok ? 'Environment ready' : 'Set this up before creating a task'));
  head.appendChild(el('span', 'spacer'));
  const btn = el('button', 'mini', 'Re-check');
  btn.title = 'Run the checks again (and re-read config.json)';
  btn.addEventListener('click', async () => {
    btn.disabled = true; btn.textContent = 'Checking...';
    await loadEnv(true);
  });
  head.appendChild(btn);
  bar.appendChild(head);

  if (r.ok) {
    head.insertBefore(
      el('span', '', r.checks.map(c => c.label).join(' · ')),
      head.querySelector('.spacer'));
    return;
  }
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
  const v = n => getComputedStyle(document.documentElement).getPropertyValue(n).trim();
  return {
    background: v('--term-bg') || '#000000',
    foreground: v('--term-fg') || '#e4e6eb',
    cursor: v('--term-cursor') || '#7aa2f7',
    selectionBackground: v('--term-sel') || 'rgba(122,162,247,.3)',
  };
}

function applyTheme(mode) {
  const root = document.documentElement;
  if (mode) root.setAttribute('data-theme', mode);
  else root.removeAttribute('data-theme');

  const dark = activeTheme() === 'dark';
  const btn = $('#theme-toggle');
  if (btn) {
    // The icon shows what you get by pressing it, not what you are in.
    btn.textContent = dark ? '\u2600' : '\u263D';
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
    nm.appendChild(document.createTextNode(p.name + (p.name === cfg.defaultProfile ? ' (default)' : '')));
    nm.appendChild(el('small', null, p.description || p.services.join(', ')));
    row.appendChild(nm);
    host.appendChild(row);
  }
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
  await fillNewTask();
  $('#modal').classList.remove('hidden');
  $('#new-name').focus();
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
    cb.addEventListener('change', () => row.classList.toggle('off', !cb.checked));
    row.appendChild(cb);

    const nm = el('div', 'nm');
    nm.appendChild(document.createTextNode(r.name + (core.has(r.name) ? '  · core' : '')));
    nm.appendChild(el('small', null, r.exists ? r.main : 'main checkout missing'));
    row.appendChild(nm);

    const sel = el('select');
    sel.dataset.repo = r.name;
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
    });
    picker.appendChild(row);
  }
  try { await loadAgents(base); } catch (e) { console.error('agents:', e); }
  // Default to the last choice, unless that CLI is gone from this machine.
  state.newAgent = state.agentDefault;
  if (!agentInfo(state.newAgent).installed) {
    const first = state.agents.find(a => a.installed);
    if (first) state.newAgent = first.kind;
  }
  renderAgentPicker();
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

$('#new-name').addEventListener('input', async e => {
  const s = slugify(e.target.value);
  $('#slug-preview').textContent = s ? '~/work/workspaces/' + s : '—';
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

$('#modal-create').addEventListener('click', async () => {
  const btn = $('#modal-create');
  const name = $('#new-name').value.trim();
  const repos = [];
  for (const cb of $('#repo-picker').querySelectorAll('input[type=checkbox]')) {
    if (!cb.checked) continue;
    const sel = $('#repo-picker').querySelector(`select[data-repo="${cb.dataset.repo}"]`);
    repos.push({ name: cb.dataset.repo, baseRef: sel ? sel.value : '' });
  }
  const err = $('#modal-error');
  err.textContent = '';
  if (!name) { err.textContent = 'Give the task a name.'; return; }
  if (!repos.length) { err.textContent = 'Select at least one repository.'; return; }

  btn.disabled = true; btn.textContent = 'Creating...';
  try {
    const t = await api.post(nodeBase(state.newNode) + '/tasks', {
      name, repos,
      agent: state.newAgent,
      branch: $('#new-branch').value.trim(),
      profile: state.newProfile,
      startAgent: $('#new-start').checked,
      dangerous: $('#new-dangerous').checked,
    });
    $('#modal').classList.add('hidden');
    await loadBoard();
    await openTask(t.slug, state.newNode);
  } catch (e) {
    err.textContent = e.message;
  }
  btn.disabled = false; btn.textContent = 'Create';
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
      row.appendChild(el('b', null, ch.label + ' '));
      row.appendChild(document.createTextNode(ch.detail || ''));
      env.appendChild(row);
    }
  }
}

$('#setup-reload').addEventListener('click', async () => {
  await api.post('/api/config', {});
  await boot();
});

boot().catch(e => {
  document.body.appendChild(el('div', 'error', 'failed to load: ' + e.message));
});





/* ------------------------------------------------------------------ project settings */

state.settings = null;

async function loadSettings() {
  state.settings = await api.get('/api/settings');
  await loadConfig();
  renderSettings();
}

function renderConfig() {
  const c = state.config;
  if (!c) return;
  $('#cfg-path').textContent = c.path;
  const host = $('#profile-list');
  host.innerHTML = '';
  for (const p of c.profiles || []) {
    const row = el('div', 'profrow');
    row.appendChild(el('b', null, p.name + (p.name === c.defaultProfile ? ' (default)' : '')));
    if (p.description) row.appendChild(el('span', 'hint', ' — ' + p.description));
    row.appendChild(el('div', 'path', 'services: ' + p.services.join(', ') + (p.entry ? ' · entry: ' + p.entry : '')));
    host.appendChild(row);
  }
  if (!(c.profiles || []).length) host.appendChild(el('div', 'hint', 'no profiles yet'));
  const pr = $('#cfg-problems');
  pr.innerHTML = '';
  for (const p of c.problems || []) {
    pr.appendChild(el('div', p.error ? 'warn' : 'hint', (p.error ? 'error · ' : 'warning · ') + (p.where ? p.where + ': ' : '') + p.msg));
  }
}

$('#cfg-reload').addEventListener('click', async () => {
  await api.post('/api/config', {});
  await loadSettings();
});

function renderSettings() {
  const d = state.settings;
  if (!d) return;
  renderConfig();

  const bl = $('#base-list');
  bl.innerHTML = '';
  for (const b of d.baseFolders || []) {
    const n = (d.projects || []).filter(p => p.base === b).length;
    const row = el('div', 'baserow');
    row.appendChild(el('span', null, b));
    row.appendChild(el('span', 'n', `${n} repositor${n === 1 ? 'y' : 'ies'}`));
    row.appendChild(el('span', 'sp'));
    const rm = el('button', 'mini', 'remove');
    rm.addEventListener('click', async () => {
      if (!confirm(`Stop scanning ${b}?\n\nIts repositories disappear from New task and from the stack.`)) return;
      state.settings = await api.put('/api/settings', { removeBase: b });
      renderSettings();
    });
    row.appendChild(rm);
    bl.appendChild(row);
  }

  const tb = $('#proj-table').querySelector('tbody');
  tb.innerHTML = '';
  for (const p of d.projects || []) {
    const tr = el('tr');

    const name = el('td');
    name.appendChild(el('div', null, p.name));
    name.appendChild(el('div', 'path', p.path));
    tr.appendChild(name);

    tr.appendChild(el('td', null, p.core ? 'yes' : ''));

    // copy list: a comma-separated field, blank meaning "copy nothing"
    const copy = el('td');
    const cell = el('div', 'copycell');
    const inp = el('input');
    inp.type = 'text';
    inp.value = (p.copy || []).join(', ');
    inp.placeholder = '.env, .env.properties';
    inp.title = 'Files copied from the main checkout into this repo\u2019s worktree in a new workspace';
    const save = async (reset) => {
      const list = reset ? null
        : inp.value.split(',').map(x => x.trim()).filter(Boolean);
      state.settings = await api.put('/api/settings',
        reset ? { project: p.name, resetCopy: true } : { project: p.name, copy: list });
      renderSettings();
    };
    inp.addEventListener('change', () => save(false));
    cell.appendChild(inp);
    if (p.overridden) {
      const o = el('span', 'ovr', 'overridden');
      o.title = `.worktreeinclude says: ${(p.default || []).join(', ') || '(empty)'}`;
      cell.appendChild(o);
      const r = el('button', 'mini', 'reset');
      r.title = 'Go back to the repo\u2019s own .worktreeinclude';
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
    sel.addEventListener('change', async () => {
      state.settings = await api.put('/api/settings', { project: p.name, ide: sel.value });
      renderSettings();
    });
    idec.appendChild(sel);
    tr.appendChild(idec);

    tb.appendChild(tr);
  }
}

$('#base-add').addEventListener('click', async () => {
  const v = $('#base-input').value.trim();
  if (!v) return;
  try {
    state.settings = await api.put('/api/settings', { addBase: v });
    $('#base-input').value = '';
    renderSettings();
  } catch (e) { alert(e.message); }
});

$('#nav-settings').addEventListener('click', async () => {
  stopPolling(); stopOrch(); showView('settings');
  try { await loadSettings(); } catch (e) { alert(e.message); }
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
  lastSeen: 0,
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
      'no open workspaces yet — ask for one in the chat'));
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
    if (t.stackUp) bits.push('stack :' + t.entry);
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
      (v.watching || []).join(', ') + ' · ' +
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
  if (!msgs.length) {
    host.appendChild(el('div', 'orch-empty',
      'Ask for a workspace: name the repositories, the branch, and what it starts from. ' +
      'Then switch to Orchestrate to have it keep them moving.'));
  }
  for (const m of msgs) host.appendChild(orchMessage(m));
  if (v.thinking) host.appendChild(el('div', 'orch-msg think', 'thinking…'));
  if (atBottom || !host.scrollTop) host.scrollTop = host.scrollHeight;
}

function orchMessage(m) {
  const row = el('div', 'orch-msg ' + m.role);
  const head = el('div', 'orch-head');
  head.appendChild(el('span', 'orch-who', {
    user: 'you', orchestrator: 'orchestrator', event: 'did', error: 'failed',
  }[m.role] || m.role));
  if (m.task) head.appendChild(el('span', 'pill', m.task));
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
    $('#orch-error').textContent = '';
    await loadOrch(false);
  } catch (e) { $('#orch-error').textContent = e.message; }
  btn.disabled = false;
  box.focus();
}

$('#orch-send').addEventListener('click', sendOrch);
$('#orch-input').addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendOrch(); }
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
  } catch (e) { $('#orch-error').textContent = e.message; }
  b.disabled = false;
});

$('#orch-reset').addEventListener('click', async () => {
  if (!confirm('Forget the conversation and start a new one?')) return;
  try { await api.post('/api/orchestrator/reset'); orch.lastSeen = 0; await loadOrch(true); }
  catch (e) { $('#orch-error').textContent = e.message; }
});


/* ------------------------------------------------------------------ rationale */

// The journal is written by the agent while it works, never here: a reason
// recorded after the fact is a reconstruction, and this card cannot tell the
// two apart. So this is a reader — filter to a kind, open one entry, or open
// the whole document.
const JR_KINDS = ['plan', 'decision', 'assumption', 'issue', 'note'];

async function loadJournal() {
  const host = $('#jr-list'), sel = $('#jr-filter');
  if (!host) return;
  let entries = [];
  try { entries = await api.get(`${tb()}/tasks/${state.current}/journal`); }
  catch (e) { host.innerHTML = ''; host.appendChild(el('div', 'warn', e.message)); return; }
  state.journal = entries;

  const counts = {};
  for (const e of entries) counts[e.kind] = (counts[e.kind] || 0) + 1;
  const keep = sel.value || 'all';
  sel.innerHTML = '';
  sel.appendChild(new Option(`all (${entries.length})`, 'all'));
  for (const k of JR_KINDS) {
    if (!counts[k]) continue;
    sel.appendChild(new Option(`${k} (${counts[k]})`, k));
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
      'nothing recorded yet — the agent writes these with `fride note`'));
    return;
  }
  const rows = all.filter(e => kind === 'all' || e.kind === kind).reverse();
  if (!rows.length) { host.appendChild(el('div', 'jrmeta', 'none of this kind')); return; }
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
  head.appendChild(el('span', 'jrmeta', [when, e.repo, e.source].filter(Boolean).join(' \u00b7 ')));
  $('#jrpop-text').textContent = e.text;
  const why = $('#jrpop-why');
  why.innerHTML = '';
  if (e.why) {
    why.appendChild(el('div', 'jrwhylabel', 'why'));
    why.appendChild(el('div', 'jrwhy', e.why));
  } else {
    why.appendChild(el('div', 'jrmeta', 'no reason was recorded with this one'));
  }
  $('#jrpop').classList.remove('hidden');
}

$('#jr-filter').addEventListener('change', renderJournalList);
$('#jr-refresh').addEventListener('click', loadJournal);
$('#jrpop-close').addEventListener('click', () => $('#jrpop').classList.add('hidden'));
$('#docpop-close').addEventListener('click', () => $('#docpop').classList.add('hidden'));

async function showDoc() {
  const body = $('#docpop-body');
  $('#docpop').classList.remove('hidden');
  body.textContent = 'composing...';
  $('#docpop-path').textContent = '';
  try {
    const r = await api.get(`${tb()}/tasks/${state.current}/rationale`);
    body.textContent = r.markdown;
    $('#docpop-path').textContent = r.path
      ? 'Writes to ' + r.path
      : 'No repo in this task can host the document.';
  } catch (e) { body.textContent = e.message; }
}

$('#jr-open').addEventListener('click', showDoc);

// Writing costs a file in the repo, and the narrative costs tokens, so both
// stay explicit rather than happening because the document was looked at.
$('#docpop-write').addEventListener('click', async () => {
  const b = $('#docpop-write'), narrate = $('#docpop-narrate').checked;
  b.disabled = true;
  b.textContent = narrate ? 'writing (the narrative takes a moment)...' : 'writing...';
  try {
    const r = await api.post(`${tb()}/tasks/${state.current}/rationale`, { narrate });
    $('#docpop-path').textContent = 'wrote ' + r.path;
    if (narrate) await showDoc();
  } catch (e) { $('#docpop-path').textContent = e.message; }
  b.disabled = false; b.textContent = 'Write to the repo';
});
