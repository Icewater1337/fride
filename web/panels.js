/* ------------------------------------------------- orchestrator panels */

// Tabs the orchestrator builds for itself (docs/PANELS.md). Each runs in a
// frame sandboxed WITHOUT allow-same-origin: it has no origin, so it cannot
// call the API, read this page or its storage. Everything goes through the
// bridge below, which is the one place a panel's calls are made, marked for
// the audit log, and, with panels.confirmRisky, confirmed.
//
// The sandbox is for isolation, not for safety from the panel: Mathias chose
// to give panels the whole API. What it buys is that a broken panel breaks
// only its own frame, and that every call it makes is written down.

const panelsUI = {
  list: [],
  confirmRisky: false,
  current: '',          // the panel shown in #panel, '' when none
  frames: new Map(),    // contentWindow → {panel, frame, errorHost}
  subs: new Map(),      // `${panel}|${sub}` → {win, topic}
  last: new Map(),      // topic → JSON text last sent
  poll: null,
  listPoll: null,
};

// The theme tokens a panel gets, copied from this page so they follow it.
const PANEL_TOKENS = ['bg', 'bg2', 'bg3', 'bg4', 'fg', 'dim', 'faint', 'line', 'line-strong',
  'accent', 'accent-hover', 'accent-fg', 'accent-soft', 'on-accent', 'green', 'green-soft',
  'amber', 'amber-soft', 'red', 'red-soft', 'slate', 'radius-sm', 'radius', 'radius-lg',
  'font', 'mono', 'shadow-sm', 'shadow-md'];

function panelTheme() {
  const cs = getComputedStyle(document.documentElement);
  const vars = PANEL_TOKENS.map(t => `--${t}:${cs.getPropertyValue('--' + t).trim()};`).join('');
  return {
    theme: activeTheme(),
    css: `:root{${vars}color-scheme:${activeTheme()}}` +
      'body{margin:0;padding:16px;font:14px/1.45 var(--font);background:var(--bg);color:var(--fg)}' +
      'a{color:var(--accent-fg)}' +
      'button{font:inherit;padding:5px 12px;border-radius:var(--radius-sm);border:1px solid var(--line-strong);' +
      'background:var(--bg2);color:var(--fg);cursor:pointer}' +
      'button.primary{background:var(--accent);border-color:var(--accent);color:var(--on-accent)}' +
      'table{border-collapse:collapse}td,th{padding:4px 8px;border-bottom:1px solid var(--line);text-align:left}' +
      'code,pre{font-family:var(--mono)}',
  };
}

// The frame's document: the bridge, the theme and the chart library first,
// then the panel's own HTML. Its doctype is moved to the front, since one
// after the prelude would drop the frame into quirks mode.
function panelDoc(id, html) {
  const o = location.origin;
  const boot = `window.FRIDE_PANEL=${JSON.stringify(id)};window.FRIDE_THEME=${JSON.stringify(panelTheme())};`;
  return '<!doctype html><meta charset="utf-8">' +
    `<script>${boot.replace(/</g, '\\u003c')}</script>` +
    `<script src="${o}/panel-kit.js"></script>` +
    `<script src="${o}/vendor/chart.umd.min.js"></script>` +
    String(html).replace(/^\s*<!doctype[^>]*>/i, '');
}

// mountPanel draws a panel into host: a frame, and above it a bar that shows
// the panel's error, if it throws, with a way to tell the orchestrator.
function mountPanel(host, id, html, cls) {
  const errorHost = el('div', 'panel-err hidden');
  const frame = el('iframe', cls || 'panel-frame');
  frame.setAttribute('sandbox', 'allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox');
  frame.setAttribute('title', id);
  frame.srcdoc = panelDoc(id, html);
  host.appendChild(errorHost);
  host.appendChild(frame);
  // contentWindow exists once the frame is in the document.
  panelsUI.frames.set(frame.contentWindow, { panel: id, frame, errorHost });
  return frame;
}

function unmountFrames(root) {
  for (const [win, f] of panelsUI.frames) {
    if (!root || root.contains(f.frame)) {
      panelsUI.frames.delete(win);
      for (const [k, s] of panelsUI.subs) if (s.win === win) panelsUI.subs.delete(k);
    }
  }
}

function showPanelError(f, msg) {
  const h = f.errorHost;
  h.innerHTML = '';
  h.classList.remove('hidden');
  h.appendChild(icon('alert'));
  h.appendChild(el('div', 'msg', 'This panel failed: ' + String(msg).split('\n')[0]));
  const tell = el('button', 'ghost sm', 'Tell orchestrator');
  tell.addEventListener('click', async () => {
    tell.disabled = true;
    try {
      await api.post('/api/orchestrator/ui-event', {
        panel: f.panel, text: 'The panel failed with this error; please fix it.', data: { error: msg },
      });
      toast('Sent to the orchestrator.', 'ok');
    } catch (e) { toast(e.message); tell.disabled = false; }
  });
  h.appendChild(tell);
  const x = el('button', 'iconbtn xs');
  x.title = 'Dismiss';
  x.appendChild(icon('x'));
  x.addEventListener('click', () => h.classList.add('hidden'));
  h.appendChild(x);
}

// A call that deletes, moves, restarts or drives a terminal. Asked about only
// with panels.confirmRisky set.
function riskyPanelCall(method, path) {
  return method === 'DELETE' || /\/(terminals|move|restore|reset|stack\/down|trash)(\/|$)/.test(path);
}

async function panelFetch(panel, method, path, body) {
  method = String(method || 'GET').toUpperCase();
  path = String(path || '');
  // Only this server's API: a panel that wants the wider web has no use for
  // FrIDE's page to fetch it.
  if (!path.startsWith('/api/')) throw new Error('fride.api: path must start with /api/');
  if (panelsUI.confirmRisky && riskyPanelCall(method, path)) {
    const ok = await ask({ title: `Panel “${panel}” wants to ${method} ${path}`,
      body: 'This call can delete or change running work. Allow it?', ok: 'Allow', danger: true });
    if (!ok) throw new Error('the user declined ' + method + ' ' + path);
  }
  const init = { method, headers: { 'X-Fride-Panel': panel } };
  if (body !== undefined && method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const r = await fetch(path, init);
  const text = await r.text();
  let v = text;
  try { v = text ? JSON.parse(text) : null; } catch { /* not JSON: hand back the text */ }
  if (!r.ok) throw new Error((v && v.error) || text || r.statusText);
  return v;
}

const PANEL_TOPICS = { tasks: '/api/tasks', orchestrator: '/api/orchestrator', panels: '/api/panels' };

window.addEventListener('message', async e => {
  const f = panelsUI.frames.get(e.source);
  const m = e.data;
  if (!f || !m || m.fride !== 1) return;   // not one of our frames
  const reply = (ok, v) => e.source.postMessage(
    ok ? { fride: 1, id: m.id, ok: true, result: v } : { fride: 1, id: m.id, ok: false, error: String(v) }, '*');
  const inline = f.panel.startsWith('inline-');
  try {
    switch (m.op) {
      case 'api': reply(true, await panelFetch(f.panel, m.method, m.path, m.body)); break;
      case 'ask':
        reply(true, await panelFetch(f.panel, 'POST', '/api/orchestrator/ui-event',
          { panel: f.panel, text: m.text, data: m.data === undefined ? null : m.data }));
        break;
      case 'state.get':
        if (inline) throw new Error('an inline widget has no state');
        reply(true, await panelFetch(f.panel, 'GET', `/api/panels/${f.panel}/state`));
        break;
      case 'state.set':
        if (inline) throw new Error('an inline widget has no state');
        reply(true, await panelFetch(f.panel, 'PUT', `/api/panels/${f.panel}/state`, m.value));
        break;
      case 'subscribe': {
        const path = PANEL_TOPICS[m.topic] || m.topic;
        if (!String(path).startsWith('/api/')) throw new Error('unknown topic ' + m.topic);
        // The first read is the panel's own, so the audit log shows who
        // follows what; the shared poll after it is not repeated there.
        const data = await panelFetch(f.panel, 'GET', path);
        panelsUI.subs.set(f.panel + '|' + m.sub, { win: e.source, topic: path, sub: m.sub });
        panelsUI.last.set(path, JSON.stringify(data));
        e.source.postMessage({ fride: 1, event: 'topic', sub: m.sub, data }, '*');
        pollTopics();
        break;
      }
      case 'unsubscribe': panelsUI.subs.delete(f.panel + '|' + m.sub); break;
      case 'error':
        showPanelError(f, m.error);
        if (!inline) api.post(`/api/panels/${f.panel}/error`, { error: m.error }).catch(() => {});
        break;
    }
  } catch (err) { if (m.id) reply(false, err.message); }
});

// pollTopics fetches every subscribed topic and sends what changed. One fetch
// per topic however many panels follow it, and only when one is started.
async function pollTopics(start) {
  if (start === undefined) {
    if (!panelsUI.poll && panelsUI.subs.size) panelsUI.poll = setInterval(() => pollTopics(true), 2500);
    return;
  }
  if (!panelsUI.subs.size) { clearInterval(panelsUI.poll); panelsUI.poll = null; panelsUI.last.clear(); return; }
  const topics = new Set([...panelsUI.subs.values()].map(s => s.topic));
  for (const t of topics) {
    let text;
    try { text = JSON.stringify(await api.get(t)); } catch { continue; }
    if (panelsUI.last.get(t) === text) continue;
    panelsUI.last.set(t, text);
    const data = JSON.parse(text);
    for (const s of panelsUI.subs.values()) {
      if (s.topic === t) s.win.postMessage({ fride: 1, event: 'topic', sub: s.sub, data }, '*');
    }
  }
}

// Frames follow the theme: the toggle stamps data-theme on <html>, and with
// no choice stored the system's preference decides.
function sendPanelTheme() {
  const t = panelTheme();
  for (const win of panelsUI.frames.keys()) win.postMessage({ fride: 1, event: 'theme', theme: t.theme, css: t.css }, '*');
}
new MutationObserver(sendPanelTheme).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', sendPanelTheme);

/* the tabs in the top bar */

async function loadPanels() {
  let v;
  try { v = await api.get('/api/panels'); } catch { return; }
  panelsUI.confirmRisky = !!v.confirmRisky;
  const before = JSON.stringify(panelsUI.list.map(p => [p.id, p.title, p.version, p.hidden]));
  panelsUI.list = v.panels || [];
  if (JSON.stringify(panelsUI.list.map(p => [p.id, p.title, p.version, p.hidden])) !== before) renderPanelNav();
  // The panel on screen got a new version, or was removed: redraw or leave.
  if (panelsUI.current) {
    const p = panelsUI.list.find(x => x.id === panelsUI.current);
    if (!p) { panelsUI.current = ''; unmountFrames($('#panel')); goBoard(); }
    else if (p.version !== panelsUI.shownVersion) openPanel(p.id);
  }
}

function renderPanelNav() {
  const nav = $('#nav-panels');
  nav.innerHTML = '';
  const shown = panelsUI.list.filter(p => !p.hidden);
  const hidden = panelsUI.list.filter(p => p.hidden);
  for (const p of shown) {
    const b = el('button', 'navbtn panelbtn' + (p.id === panelsUI.current ? ' active' : ''));
    b.dataset.panel = p.id;
    b.title = p.summary || p.title;
    b.appendChild(icon('sparkle'));
    b.appendChild(el('span', 'lbl', p.title));
    if (p.error) b.appendChild(el('span', 'panel-dot', ''));
    b.addEventListener('click', () => openPanel(p.id));
    nav.appendChild(b);
  }
  if (hidden.length) {
    const more = el('button', 'navbtn', '+' + hidden.length);
    more.title = 'Hidden panels';
    more.addEventListener('click', () => showMenu(more, {
      title: 'Hidden panels',
      items: hidden.map(p => ({ label: p.title, detail: p.summary, icon: 'sparkle',
        onSelect: async () => { await api.put(`/api/panels/${p.id}/hidden`, { hidden: false }); await loadPanels(); openPanel(p.id); } })),
    }));
    nav.appendChild(more);
  }
}

async function openPanel(id) {
  let p;
  try { p = await api.get('/api/panels/' + encodeURIComponent(id)); }
  catch (e) { toast(e.message); return; }
  leaveTask(); stopOrch();
  panelsUI.current = id;
  panelsUI.shownVersion = p.version;
  showView('panel');
  $('#panel-title').textContent = p.title;
  $('#panel-meta').textContent = `v${p.version}` + (p.note ? ' · ' + p.note : '') +
    (p.updated ? ' · ' + new Date(p.updated).toLocaleString() : '');
  const host = $('#panel-host');
  unmountFrames(host);
  host.innerHTML = '';
  const frame = mountPanel(host, id, p.html);
  if (p.error) showPanelError(panelsUI.frames.get(frame.contentWindow), p.error);
  $('#panel-menu').onclick = () => showMenu($('#panel-menu'), {
    title: p.title,
    items: [
      { label: 'Reload', icon: 'refresh', onSelect: () => openPanel(id) },
      { label: 'Hide this tab', detail: 'The orchestrator keeps it, and will not reopen it unasked.', icon: 'x',
        onSelect: async () => { await api.put(`/api/panels/${id}/hidden`, { hidden: true }); panelsUI.current = ''; unmountFrames(host); await loadPanels(); goBoard(); } },
      ...(p.history || []).map(v => ({ label: `Restore v${v.version}`, detail: v.note || '', icon: 'history',
        onSelect: async () => { try { await api.post(`/api/panels/${id}/restore`, { version: v.version }); await loadPanels(); } catch (e) { toast(e.message); } } })),
      { label: 'Delete', icon: 'trash', onSelect: async () => {
        if (!await ask({ title: `Delete the panel “${p.title}”?`, body: 'Its source, versions and state go.', ok: 'Delete', danger: true })) return;
        await fetch('/api/panels/' + encodeURIComponent(id), { method: 'DELETE' });
        panelsUI.current = ''; unmountFrames(host); await loadPanels(); goBoard();
      } },
    ],
  });
  renderPanelNav();
}

// Leaving the panel view stops its frame, and with it its subscriptions.
function leavePanel() {
  if (!panelsUI.current) return;
  panelsUI.current = '';
  const host = $('#panel-host');
  unmountFrames(host);
  host.innerHTML = '';
  renderPanelNav();
}

loadPanels();
panelsUI.listPoll = setInterval(loadPanels, 5000);
