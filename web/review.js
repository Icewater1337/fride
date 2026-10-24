/* The Review tab: what the task's agents changed, read file by file.

   Loaded after app.js and built from its helpers ($, el, icon, api, pin,
   stale, toast). The server does the reading (GET /tasks/<slug>/changes);
   this file only draws it and keeps the reviewer's place.

   The page is never redrawn under the reader. A background poll compares the
   change against what is on screen and offers a refresh when it moved on: an
   agent still at work would otherwise shift the diff while you read it. */

const rv = {
  data: null,        // the last /changes answer drawn, or fetched for the badge
  drawnSig: '',      // signature of what is on screen
  current: '',       // key of the file the keys act on
  open: new Map(),   // key -> true/false: a file the reviewer opened or closed by hand
  showAllFocus: false,
  showDismissed: false,
  poll: null,
  soon: null,        // a quicker look while an AI pass or a revert check runs
  loading: false,
};

const RV_HIDE_KEY = 'fride-review-hide-viewed';
const RV_TOUR_KEY = 'fride-review-tour';

// The tour's layers, in reading order (review.Layers on the server).
const RV_LAYERS = [
  ['schema', 'Schema and data', 'What the data looks like'],
  ['logic', 'Logic', 'The code over it'],
  ['api', 'Interface', 'What exposes it: handlers, routes, the API'],
  ['consumer', 'Callers', 'What uses that interface: UI, CLI'],
  ['test', 'Tests', 'What pins it down'],
  ['other', 'Docs, build and config', ''],
];
const rvLayerRank = l => { const i = RV_LAYERS.findIndex(x => x[0] === l); return i < 0 ? RV_LAYERS.length : i; };

function rvKey(repo, f) { return repo + '/' + f.path; }

// The signature of a change: which files, at which version. Two answers with
// the same signature draw the same page.
function rvSig(d) {
  if (!d) return '';
  const parts = [];
  for (const r of d.repos) for (const f of r.files) parts.push(r.repo + '/' + f.path + '@' + f.hash);
  return parts.join('|');
}

function rvFiles(d) {
  const out = [];
  if (!d) return out;
  for (const r of d.repos) for (const f of r.files) out.push({ repo: r.repo, r, f, key: rvKey(r.repo, f) });
  return out;
}

// What is left to read: files not viewed in their current version. Folded
// noise does not count; nobody reads a lockfile line by line.
function rvLeft(d) {
  return rvFiles(d).filter(x => x.f.viewed !== 'viewed' && !x.f.noise).length;
}

function reviewOpen() { return !$('#review-view').classList.contains('hidden'); }

function reviewReset() {
  rv.data = null;
  rv.drawnSig = '';
  rv.current = '';
  rv.open.clear();
  rv.showAllFocus = false;
  rv.showDismissed = false;
  hideReview();
  $('#rv-tree').innerHTML = '';
  $('#rv-main').innerHTML = '';
  $('#rv-moved').classList.add('hidden');
  $('#rv-risk').classList.add('hidden');
  rvBadge(null);
}

function hideReview() {
  $('#review-view').classList.add('hidden');
  $('#review-tab').classList.remove('active');
  $('#review-tab').setAttribute('aria-selected', 'false');
}

function showReview() {
  $('#file-view').classList.add('hidden');
  $('#term-host').classList.add('hidden');
  $('#term-empty').classList.add('hidden');
  for (const [, e] of state.terms) if (e.tab) { e.tab.classList.remove('active'); e.tab.setAttribute('aria-selected', 'false'); }
  $('#review-view').classList.remove('hidden');
  $('#review-tab').classList.add('active');
  $('#review-tab').setAttribute('aria-selected', 'true');
  // Drawn from what the badge poll already fetched, then read again: the
  // page appears at once, and is current a moment later.
  if (rv.data && !rv.drawnSig) rvDraw(rv.data);
  loadReview(pin(), true).catch(e => toast('Review: ' + e.message));
}

$('#review-tab').addEventListener('click', showReview);
$('#review-tab').addEventListener('keydown', e => {
  if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); showReview(); }
});
$('#rv-refresh').addEventListener('click', () => loadReview(pin(), true, true).catch(e => toast(e.message)));
$('#rv-moved').addEventListener('click', () => loadReview(pin(), true, true).catch(e => toast(e.message)));

try { $('#rv-hide-viewed').checked = localStorage.getItem(RV_HIDE_KEY) === '1'; } catch {}
$('#rv-hide-viewed').addEventListener('change', e => {
  try { localStorage.setItem(RV_HIDE_KEY, e.target.checked ? '1' : '0'); } catch {}
  if (rv.data) rvDraw(rv.data, true);
});
try { $('#rv-tour').checked = localStorage.getItem(RV_TOUR_KEY) === '1'; } catch {}
$('#rv-tour').addEventListener('change', e => {
  try { localStorage.setItem(RV_TOUR_KEY, e.target.checked ? '1' : '0'); } catch {}
  if (rv.data) rvDraw(rv.data);
});

// loadReview fetches the change. draw: put it on screen if the tab is open.
// force: redraw even if the reader is mid-page (the Refresh button).
async function loadReview(p = pin(), draw = false, force = false) {
  if (rv.loading && !force) return;
  rv.loading = true;
  if (force) $('#rv-refresh').classList.add('spin');
  let d;
  try { d = await api.get(`${p.base}/tasks/${p.slug}/changes`); }
  finally { rv.loading = false; $('#rv-refresh').classList.remove('spin'); }
  if (stale(p)) return;
  rv.data = d;
  rvBadge(d);
  rvStartPoll(p);
  if (rvBusy(d)) rvSoon(p);
  if (!reviewOpen()) { rv.drawnSig = ''; return; }
  const sig = rvSig(d);
  // Nothing on screen yet draws whatever the call: the tab may have opened
  // while this load was already in flight, and a refusal would leave it empty.
  if (!rv.drawnSig || (draw && (force || sig === rv.drawnSig))) {
    rvDraw(d, !force && !!rv.drawnSig);
    $('#rv-moved').classList.add('hidden');
  } else if (sig !== rv.drawnSig) {
    rvMoved(d);
  } else {
    // Same files, same versions: only the marks, the evidence and the
    // checks may have moved on, and those are safe to redraw in place.
    rvDrawTree(d);
    rvProgress(d);
    rvDrawSummary(d);
  }
}

// rvBusy: an AI pass or a revert check is running, worth a quicker look.
function rvBusy(d) {
  return (d.ai && d.ai.state === 'running') || Object.values(d.revert || {}).some(x => x.state === 'running');
}

function rvSoon(p) {
  if (rv.soon) return;
  rv.soon = setTimeout(() => {
    rv.soon = null;
    if (!stale(p)) loadReview(p, false).catch(() => {});
  }, 4000);
}

// rvMoved tells the reader the change moved on, without redrawing under them.
function rvMoved(d) {
  const before = new Map(rvFiles(rv.drawnData).map(x => [x.key, x.f.hash]));
  let n = 0;
  for (const x of rvFiles(d)) if (before.get(x.key) !== x.f.hash) n++;
  for (const k of before.keys()) if (!rvFiles(d).some(x => x.key === k)) n++;
  const m = $('#rv-moved');
  m.textContent = '';
  m.appendChild(icon('refresh'));
  m.appendChild(el('span', null, `${n} file${n === 1 ? '' : 's'} changed since. Refresh`));
  m.classList.remove('hidden');
}

function rvStartPoll(p) {
  if (rv.poll && rv.poll.gen === p.gen) return;
  if (rv.poll) clearInterval(rv.poll.id);
  const id = setInterval(() => {
    if (stale(p)) { clearInterval(id); return; }
    if (!$('#task').classList.contains('active') || document.hidden) return;
    loadReview(p, false).catch(() => {});
  }, 20000);
  rv.poll = { id, gen: p.gen };
}

function rvBadge(d) {
  const c = $('#review-count');
  if (!d) { c.classList.add('hidden'); return; }
  const left = rvLeft(d);
  c.textContent = left;
  c.classList.toggle('hidden', left === 0);
  c.title = `${left} file${left === 1 ? '' : 's'} to review`;
}

/* ------------------------------------------------------------------ drawing */

// rvOrdered is the files as the main view lists them: by repo, or in tour
// order across repos.
function rvOrdered(d) {
  const all = rvFiles(d);
  if (!$('#rv-tour').checked) return all;
  const repoIdx = new Map(d.repos.map((r, i) => [r.repo, i]));
  return all.map((x, i) => ({ ...x, i })).sort((a, b) =>
    rvLayerRank(a.f.layer) - rvLayerRank(b.f.layer) || repoIdx.get(a.repo) - repoIdx.get(b.repo) || a.i - b.i);
}

function rvDraw(d, keepScroll) {
  const main = $('#rv-main');
  const top = main.scrollTop;
  rv.drawnSig = rvSig(d);
  rv.drawnData = d;
  rvDrawTree(d);
  main.innerHTML = '';
  main.appendChild(el('div', 'rv-size'));
  main.appendChild(el('div', 'rv-summary'));
  rvDrawSummary(d);
  const hide = $('#rv-hide-viewed').checked;
  const tour = $('#rv-tour').checked;
  for (const r of d.repos) if (r.err) main.appendChild(el('div', 'rv-err', `${r.repo}: ${r.err}`));
  let shown = 0, layer = null;
  for (const x of rvOrdered(d)) {
    if (hide && x.f.viewed === 'viewed') continue;
    if (tour && x.f.layer !== layer) {
      layer = x.f.layer;
      const def = RV_LAYERS[rvLayerRank(layer)] || [layer, layer, ''];
      const h = el('div', 'rv-layer');
      h.appendChild(el('span', 'n', String(rvLayerRank(layer) + 1)));
      h.appendChild(el('b', null, def[1]));
      if (def[2]) h.appendChild(el('span', 'dim', def[2]));
      main.appendChild(h);
    }
    main.appendChild(rvFile(x.r, x.f));
    shown++;
  }
  if (!shown) {
    const empty = el('div', 'rv-empty');
    empty.appendChild(icon('check', 'big'));
    empty.appendChild(el('h3', null, rvFiles(d).length ? 'Everything is viewed' : 'Nothing changed yet'));
    empty.appendChild(el('p', null, rvFiles(d).length
      ? 'Every file has been read in its current version. A file the agent changes again comes back here.'
      : 'No repository of this task differs from its base.'));
    main.appendChild(empty);
  }
  if (d.capped) {
    main.appendChild(el('div', 'rv-note', 'Some files are too large to show here. They list their size, and open in full from their header.'));
  }
  rvProgress(d);
  if (keepScroll) main.scrollTop = top;
  rvMark(rv.current);
}

function rvProgress(d) {
  const all = rvFiles(d).filter(x => !x.f.noise);
  const done = all.filter(x => x.f.viewed === 'viewed').length;
  $('#rv-progress').textContent = all.length
    ? `${done} of ${all.length} file${all.length === 1 ? '' : 's'} viewed` : 'No changes';
  $('#rv-bar-fill').style.width = all.length ? `${Math.round(100 * done / all.length)}%` : '0';
  rvBadge(d);
  rvRisk(d);
}

// rvRisk is the header's risk chip; its title lists what the score is made of.
function rvRisk(d) {
  const chip = $('#rv-risk');
  const r = d.risk;
  if (!r || !rvFiles(d).length) { chip.classList.add('hidden'); return; }
  chip.className = 'rv-risk lvl-' + r.level;
  chip.textContent = '';
  chip.appendChild(icon('gauge'));
  chip.appendChild(el('span', null, `Risk ${r.score}`));
  chip.title = `Merge risk ${r.score}/100 (${r.level})` +
    (r.reasons.length ? ':\n' + r.reasons.map(x => '· ' + x).join('\n') : ': nothing stands out');
}

function rvState(f) {
  if (f.viewed === 'viewed') return { cls: 'viewed', icon: 'check', title: 'Viewed' };
  if (f.viewed === 'changed') return { cls: 'changed', icon: 'refresh', title: 'Changed since you viewed it' };
  return { cls: 'unread', icon: null, title: 'Not viewed yet' };
}

function rvStatusCls(s) { return s === 'A' ? 'new' : s === 'D' ? 'del' : s === 'R' || s === 'C' ? 'ren' : 'mod'; }

function rvDrawTree(d) {
  const tree = $('#rv-tree');
  const top = tree.scrollTop;
  tree.innerHTML = '';
  const hide = $('#rv-hide-viewed').checked;
  const tour = $('#rv-tour').checked;
  for (const r of d.repos) {
    const files = r.files.filter(f => !f.noise);
    const head = el('div', 'rv-repo');
    head.appendChild(el('b', null, r.repo));
    const viewed = files.filter(f => f.viewed === 'viewed').length;
    head.appendChild(el('span', 'rv-count', `${viewed}/${files.length}`));
    const base = el('div', 'rv-base', `${r.branch || 'HEAD'} ← ${r.base}`);
    base.title = r.mergeBase ? `Compared with the merge-base ${r.mergeBase.slice(0, 10)} of ${r.base}` : r.base;
    tree.appendChild(head);
    tree.appendChild(base);
    if (!r.files.length) tree.appendChild(el('div', 'rv-tclean', r.err ? 'could not be read' : 'no changes'));
    // Noise last: it is folded in the diff too. In a tour, by layer.
    const order = r.files.map((f, i) => ({ f, i })).sort((a, b) =>
      (!!a.f.noise - !!b.f.noise) || (tour ? rvLayerRank(a.f.layer) - rvLayerRank(b.f.layer) : 0) || a.i - b.i).map(x => x.f);
    for (const f of order) {
      if (hide && f.viewed === 'viewed') continue;
      const key = rvKey(r.repo, f);
      const row = el('div', 'rv-trow');
      row.dataset.key = key;
      row.title = f.oldPath ? `${f.oldPath} → ${f.path}` : f.path;
      const st = rvState(f);
      const mark = el('span', 'rv-tmark ' + st.cls);
      if (st.icon) mark.appendChild(icon(st.icon));
      mark.title = f.noise ? 'Folded: ' + f.noiseWhy : st.title;
      row.appendChild(mark);
      row.appendChild(el('span', 'st ' + rvStatusCls(f.status), f.status));
      const slash = f.path.lastIndexOf('/');
      const name = el('span', 'rv-tname');
      if (slash >= 0) name.appendChild(el('span', 'dir', f.path.slice(0, slash + 1)));
      name.appendChild(el('span', null, f.path.slice(slash + 1)));
      row.appendChild(name);
      if (f.noise) row.classList.add('noise');
      const open = rvThreadsOf(d, r.repo, f.path).filter(t => t.blocking && !t.satisfied).length;
      if (open) { const b = el('span', 'rv-tblock', String(open)); b.title = `${open} blocking comment${open === 1 ? '' : 's'} open`; row.appendChild(b); }
      row.appendChild(el('span', 'rv-tnum', f.binary ? 'bin' : `+${f.added} −${f.deleted}`));
      row.addEventListener('click', () => rvGo(key, true));
      tree.appendChild(row);
    }
  }
  tree.scrollTop = top;
  if (rv.current) for (const r of tree.querySelectorAll('.rv-trow')) r.classList.toggle('current', r.dataset.key === rv.current);
}

// A file is open unless the reviewer closed it, it is viewed, or it is noise.
function rvIsOpen(key, f) {
  if (rv.open.has(key)) return rv.open.get(key);
  return f.viewed !== 'viewed' && !f.noise;
}

function rvThreadsOf(d, repo, path) {
  return ((d && d.threads) || []).filter(t => t.repo === repo && t.path === path);
}

function rvFile(r, f) {
  const key = rvKey(r.repo, f);
  const box = el('section', 'rv-file');
  box.dataset.key = key;
  const head = el('div', 'rv-fhead');
  const caret = el('button', 'iconbtn xs rv-caret');
  caret.appendChild(icon('chevron-right'));
  caret.setAttribute('aria-label', 'Show or hide the diff');
  head.appendChild(caret);
  head.appendChild(el('span', 'st ' + rvStatusCls(f.status), f.status));
  const path = el('span', 'rv-fpath');
  if (f.oldPath) { path.appendChild(el('span', 'old', f.oldPath)); path.appendChild(el('span', 'arrow', ' → ')); }
  path.appendChild(document.createTextNode(f.path));
  head.appendChild(path);
  head.appendChild(el('span', 'rv-repo-tag', r.repo));
  if (f.untracked) head.appendChild(rvTag('untracked', 'muted', 'Not yet known to git'));
  if (f.withheld) head.appendChild(rvTag('withheld', 'amber', 'Holds credentials; not served'));
  if (f.noise) head.appendChild(rvTag(f.noise, 'muted', f.noiseWhy));
  if (f.viewed === 'changed') head.appendChild(rvTag('changed since viewed', 'amber',
    'The hunks you already read are folded as "read before"'));
  head.appendChild(el('span', 'spacer'));
  head.appendChild(el('span', 'rv-fnum', f.binary ? 'binary' : `+${f.added} −${f.deleted}`));
  if (f.status !== 'D') {
    const openBtn = el('button', 'iconbtn xs');
    openBtn.title = 'Open the whole file';
    openBtn.setAttribute('aria-label', 'Open the whole file');
    openBtn.appendChild(icon('file-text'));
    openBtn.addEventListener('click', ev => { ev.stopPropagation(); rvOpenFile(r.repo, f.path); });
    head.appendChild(openBtn);
  }
  const vlabel = el('label', 'rv-viewed');
  const vbox = el('input');
  vbox.type = 'checkbox';
  vbox.checked = f.viewed === 'viewed';
  vlabel.appendChild(vbox);
  vlabel.appendChild(document.createTextNode('Viewed'));
  vlabel.addEventListener('click', ev => ev.stopPropagation());
  vbox.addEventListener('change', () => rvSetViewed(r.repo, f, vbox.checked));
  head.appendChild(vlabel);
  box.appendChild(head);

  const body = el('div', 'rv-fbody');
  box.appendChild(body);
  const setOpen = on => {
    box.classList.toggle('open', on);
    if (on && !body.childElementCount) rvFillBody(body, r, f);
  };
  setOpen(rvIsOpen(key, f));
  head.addEventListener('click', () => {
    const on = !box.classList.contains('open');
    rv.open.set(key, on);
    setOpen(on);
    rvMark(key);
  });
  return box;
}

function rvTag(text, kind, title) {
  const t = el('span', 'tag ' + (kind || ''), text);
  if (title) t.title = title;
  return t;
}

function rvFillBody(body, r, f) {
  if (f.withheld) {
    body.appendChild(rvNote('Withheld: this file holds credentials. Open a shell tab to read it.'));
    return;
  }
  // Comments on a version of the file that is gone: shown first, still open.
  const threads = rvThreadsOf(rv.data, r.repo, f.path);
  const ids = new Set(f.hunks.map(h => h.id));
  const gone = threads.filter(t => !ids.has(t.hunk));
  if (gone.length) {
    const wrap = el('div', 'rv-gone');
    wrap.appendChild(el('div', 'dim', 'Comments on an earlier version of this file'));
    for (const t of gone) wrap.appendChild(rvThreadBox(t));
    body.appendChild(wrap);
  }
  if (f.truncated) {
    body.appendChild(rvNote(f.binary ? 'Binary file.' : 'Too large to show here.', f.status !== 'D' && !f.binary ? () => rvOpenFile(r.repo, f.path) : null));
    return;
  }
  if (f.binary) { body.appendChild(rvNote('Binary file. Not shown.')); return; }
  if (!f.hunks.length) {
    body.appendChild(rvNote(f.status === 'R' ? 'Renamed without changes.' : f.status === 'A' ? 'Empty file.' : 'Only the file mode changed.'));
    return;
  }
  for (const h of f.hunks) body.appendChild(rvHunk(h, r, f, threads.filter(t => t.hunk === h.id)));
}

function rvNote(text, open) {
  const n = el('div', 'rv-fnote', text);
  if (open) {
    const b = el('button', 'ghost sm', 'Open the file');
    b.addEventListener('click', open);
    n.appendChild(b);
  }
  return n;
}

const RV_PROV = {
  a: ['agent', 'Written by an agent edit, as it stands'],
  m: ['mixed', 'An agent wrote a line like it, and it was changed since'],
  h: ['none', 'No agent edit wrote it: by hand, or by a command'],
};

function rvHunk(h, r, f, threads) {
  const box = el('div', 'rv-hunk');
  box.dataset.hunk = h.id;
  if (h.traces && h.traces.length) box.appendChild(rvTrace(h.traces, h.unattributed || 0));
  const head = el('div', 'rv-hhead');
  head.appendChild(el('span', 'rng', `@@ −${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@`));
  if (h.header) head.appendChild(el('span', 'fn', h.header));
  box.appendChild(head);

  // Which lines moved, and where to or from.
  const moved = new Map();
  for (const m of h.moves || []) {
    for (let i = m.line; i < m.line + m.n; i++) moved.set(i, `${h.lines[i][0] === '+' ? 'Moved from' : 'Moved to'} ${m.path}:${m.at}`);
  }
  const table = el('div', 'rv-lines');
  const fill = () => {
    let o = h.oldStart, n = h.newStart;
    const frag = document.createDocumentFragment();
    const after = new Map(); // line number -> its threads
    const atEnd = [];
    for (const t of threads) {
      if (t.line > 0) { if (!after.has(t.line)) after.set(t.line, []); after.get(t.line).push(t); }
      else atEnd.push(t);
    }
    h.lines.forEach((l, i) => {
      const k = l[0];
      const row = el('div', 'rv-l ' + (k === '+' ? 'add' : k === '-' ? 'del' : 'ctx'));
      const pv = el('span', 'pv');
      const prov = RV_PROV[(h.prov || '')[i]];
      if (prov) { pv.classList.add(prov[0]); pv.title = prov[1]; }
      row.appendChild(pv);
      const lnOld = el('span', 'ln', k === '+' ? '' : String(o));
      const lnNew = el('span', 'ln', k === '-' ? '' : String(n));
      row.appendChild(lnOld);
      row.appendChild(lnNew);
      row.appendChild(el('span', 'sg', k === ' ' ? '' : k));
      row.appendChild(el('span', 'tx', l.slice(1)));
      if (moved.has(i)) { row.classList.add('mv'); row.title = moved.get(i); }
      const line = k === '-' ? 0 : n;
      for (const ln of [lnOld, lnNew]) {
        if (!ln.textContent) continue;
        ln.classList.add('cmt');
        ln.title = 'Comment on this line';
        ln.addEventListener('click', () => rvCommentForm(row, r, f, h, line, l.slice(1)));
      }
      frag.appendChild(row);
      for (const t of after.get(line) || []) if (line) frag.appendChild(rvThreadBox(t));
      if (k !== '+') o++;
      if (k !== '-') n++;
    });
    for (const t of atEnd) frag.appendChild(rvThreadBox(t));
    // A thread whose line is not in this hunk any more still shows, last.
    const placed = new Set();
    for (let x = h.newStart; x < n; x++) placed.add(x);
    for (const [ln, ts] of after) if (!placed.has(ln)) for (const t of ts) frag.appendChild(rvThreadBox(t));
    table.appendChild(frag);
  };
  // Folded, never hidden: a whitespace-only or moved hunk, and one the
  // reviewer already read in the version they marked viewed.
  let fold = '';
  if (h.mechanical === 'whitespace') fold = 'Whitespace only';
  else if (h.mechanical === 'moved') {
    const m = (h.moves || [])[0];
    fold = m ? `Moved code · ${h.lines[m.line][0] === '+' ? 'from' : 'to'} ${m.path}:${m.at}` : 'Moved code';
  } else if (h.seen && f.viewed === 'changed') fold = 'Read before: unchanged since you viewed the file';
  if (fold && !threads.length) {
    const b = el('button', 'rv-fold');
    b.appendChild(el('span', null, fold));
    b.appendChild(el('span', 'dim', `${h.lines.length} lines · show`));
    b.addEventListener('click', () => { b.remove(); fill(); });
    box.appendChild(b);
  } else {
    fill();
  }
  box.appendChild(table);
  return box;
}

function rvTime(t) {
  if (!t) return '';
  const d = new Date(t);
  if (isNaN(d)) return '';
  const today = new Date();
  const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return d.toDateString() === today.toDateString() ? hm : `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${hm}`;
}

// rvTrace is the band over a hunk: which agent edits wrote it, and why. The
// line shows the edit that wrote most of it; opening it shows every one.
function rvTrace(traces, unattributed) {
  const band = el('div', 'rv-trace');
  const lead = traces[0];
  const line = el('div', 'rv-tline');
  line.appendChild(icon('sparkle'));
  line.appendChild(el('b', null, lead.sidechain ? 'subagent' : 'agent'));
  line.appendChild(el('span', 'dim', `${lead.tool} · ${rvTime(lead.time)}`));
  line.appendChild(el('span', 'rv-intent', lead.intent || lead.prompt || 'No words before this edit.'));
  const extra = [];
  if (traces.length > 1) extra.push(`+${traces.length - 1} edit${traces.length > 2 ? 's' : ''}`);
  if (unattributed) extra.push(`${unattributed} line${unattributed === 1 ? '' : 's'} from no edit`);
  if (extra.length) line.appendChild(el('span', 'dim', extra.join(' · ')));
  line.title = 'Show what the agent was doing';
  band.appendChild(line);

  const more = el('div', 'rv-tmore');
  for (const tr of traces) {
    const one = el('div', 'rv-tone');
    const h = el('div', 'rv-tonehead');
    h.appendChild(el('b', null, tr.sidechain ? 'Subagent' : 'Agent'));
    h.appendChild(el('span', 'dim', `${tr.tool} · ${rvTime(tr.time)} · ${tr.lines} line${tr.lines === 1 ? '' : 's'} of this hunk`));
    one.appendChild(h);
    const grid = el('div', 'rv-tgrid');
    if (tr.intent) { grid.appendChild(el('div', 'k', 'Said')); grid.appendChild(el('div', 'v', tr.intent)); }
    if (tr.prompt) { grid.appendChild(el('div', 'k', tr.sidechain ? 'Its task' : 'Asked')); grid.appendChild(el('div', 'v', tr.prompt)); }
    grid.appendChild(el('div', 'k', 'Session'));
    grid.appendChild(el('div', 'v mono', tr.session));
    one.appendChild(grid);
    more.appendChild(one);
  }
  if (unattributed) {
    more.appendChild(el('div', 'rv-tnone',
      `${unattributed} added line${unattributed === 1 ? ' matches' : 's match'} no agent edit: written by hand, by a command, or rewritten since.`));
  }
  band.appendChild(more);
  line.addEventListener('click', () => band.classList.toggle('open'));
  return band;
}

/* ------------------------------------------------------------------ comments */

// rvCommentForm opens a new comment under a line.
function rvCommentForm(row, r, f, h, line, quote) {
  const next = row.nextElementSibling;
  if (next && next.classList.contains('rv-cform')) { next.querySelector('textarea').focus(); return; }
  const form = el('div', 'rv-cform');
  const ta = el('textarea');
  ta.rows = 2;
  ta.placeholder = line ? `Comment on line ${line}` : 'Comment on this removed line';
  form.appendChild(ta);
  const bar = el('div', 'rv-cbar');
  const blk = el('label', 'rv-toggle');
  const blkBox = el('input');
  blkBox.type = 'checkbox';
  blk.appendChild(blkBox);
  blk.appendChild(document.createTextNode('Blocking'));
  blk.title = 'Keeps the task out of Done until you mark the comment Satisfied';
  bar.appendChild(blk);
  bar.appendChild(el('span', 'spacer'));
  const cancel = el('button', 'ghost sm', 'Cancel');
  cancel.addEventListener('click', () => form.remove());
  bar.appendChild(cancel);
  const save = el('button', 'ghost sm', 'Comment');
  const send = el('button', 'primary sm');
  send.appendChild(icon('send'));
  send.appendChild(el('span', null, 'Comment and send to agent'));
  send.title = 'Typed into the task\'s agent as a follow-up';
  const submit = async toAgent => {
    const text = ta.value.trim();
    if (!text) { ta.focus(); return; }
    const p = pin();
    save.disabled = send.disabled = true;
    let t;
    try {
      t = await api.post(`${p.base}/tasks/${p.slug}/changes/threads`,
        { repo: r.repo, path: f.path, hunk: h.id, line, quote, text, blocking: blkBox.checked, send: toAgent });
    } catch (e) { toast(e.message); save.disabled = send.disabled = false; return; }
    if (stale(p)) return;
    rvThreadChanged(t);
  };
  save.addEventListener('click', () => submit(false));
  send.addEventListener('click', () => submit(true));
  ta.addEventListener('keydown', e => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); submit(false); }
    if (e.key === 'Escape') form.remove();
  });
  bar.appendChild(save);
  bar.appendChild(send);
  form.appendChild(bar);
  row.after(form);
  ta.focus();
}

// rvThreadBox draws one thread under its line: the comments, and what can be
// done with it.
function rvThreadBox(t) {
  const box = el('div', 'rv-thread' + (t.blocking && !t.satisfied ? ' blocking' : '') + (t.satisfied ? ' satisfied' : ''));
  box.dataset.thread = t.id;
  const head = el('div', 'rv-thead');
  head.appendChild(el('b', null, t.blocking ? 'Blocking' : 'Comment'));
  if (t.satisfied) head.appendChild(rvTag('satisfied', 'green'));
  if (t.outdated) head.appendChild(rvTag('on an earlier version', 'muted', 'The hunk it was made on has changed since'));
  if (t.quote) { const q = el('code', 'rv-tquote', t.quote.trim()); q.title = t.quote; head.appendChild(q); }
  head.appendChild(el('span', 'spacer'));
  const mk = (label, title, body) => {
    const b = el('button', 'ghost xs', label);
    b.title = title;
    b.addEventListener('click', () => rvThreadPut(t, body));
    return b;
  };
  if (t.blocking) head.appendChild(t.satisfied
    ? mk('Reopen', 'Back to blocking: the task cannot be Done until it is satisfied', { satisfied: false })
    : mk('Satisfied', 'The request was met: this thread no longer blocks Done', { satisfied: true }));
  else head.appendChild(mk('Make blocking', 'Keep the task out of Done until this is satisfied', { blocking: true }));
  box.appendChild(head);
  for (const c of t.comments) {
    const one = el('div', 'rv-tcomment');
    one.appendChild(el('div', 'txt', c.text));
    const meta = el('div', 'dim', rvTime(c.at));
    if (c.sent) meta.appendChild(document.createTextNode(` · sent to the agent ${rvTime(c.sent)}`));
    if (c.sendErr) { const e = el('span', 'err', ` · not sent: ${c.sendErr}`); meta.appendChild(e); }
    one.appendChild(meta);
    box.appendChild(one);
  }
  const last = t.comments[t.comments.length - 1];
  const reply = el('div', 'rv-treply');
  const ta = el('textarea');
  ta.rows = 1;
  ta.placeholder = 'Reply';
  reply.appendChild(ta);
  const r1 = el('button', 'ghost xs', 'Reply');
  r1.addEventListener('click', () => { if (ta.value.trim()) rvThreadPut(t, { text: ta.value }); });
  reply.appendChild(r1);
  const r2 = el('button', 'ghost xs');
  r2.appendChild(icon('send'));
  const idle = last.sent ? 'Send again' : 'Send to agent';
  r2.appendChild(el('span', null, idle));
  ta.addEventListener('input', () => { r2.lastChild.textContent = ta.value.trim() ? 'Reply and send' : idle; });
  r2.title = 'Type the latest comment into the task\'s agent as a follow-up';
  r2.addEventListener('click', () => rvThreadPut(t, { text: ta.value, send: true }));
  reply.appendChild(r2);
  box.appendChild(reply);
  return box;
}

async function rvThreadPut(t, body) {
  const p = pin();
  let nt;
  try { nt = await api.put(`${p.base}/tasks/${p.slug}/changes/threads/${t.id}`, body); }
  catch (e) { toast(e.message); return; }
  if (stale(p)) return;
  if (body.send) {
    const last = nt.comments[nt.comments.length - 1];
    toast(last.sendErr ? 'Not sent: ' + last.sendErr : 'Sent to the agent');
  }
  rvThreadChanged(nt);
}

// rvThreadChanged puts a new or changed thread on screen: its file is drawn
// again (kept open), the summary too, and the risk is read back from the server.
function rvThreadChanged(t) {
  if (!rv.data) return;
  const list = rv.data.threads || (rv.data.threads = []);
  const i = list.findIndex(x => x.id === t.id);
  if (i >= 0) list[i] = { ...t, outdated: list[i].outdated }; else list.push(t);
  const key = t.repo + '/' + t.path;
  const box = rvFileBox(key);
  const x = rvFiles(rv.data).find(y => y.key === key);
  if (box && x) {
    rv.open.set(key, true);
    box.replaceWith(rvFile(x.r, x.f));
  }
  rvDrawTree(rv.data);
  rvDrawSummary(rv.data);
  loadReview(pin(), false).catch(() => {});
}

/* ------------------------------------------------------------------ summary */

const RV_FOCUS_SHOWN = 7;

function rvDrawSummary(d) {
  const size = $('#rv-main .rv-size');
  if (size) { size.innerHTML = ''; if (d.size && d.size.large) size.appendChild(rvSize(d)); }
  const host = $('#rv-main .rv-summary');
  if (!host) return;
  host.innerHTML = '';
  host.appendChild(rvFocus(d));
  host.appendChild(rvEvidence(d));
  host.appendChild(rvAI(d));
  host.appendChild(rvThreads(d));
}

function rvSection(title, iconName, aside) {
  const s = el('div', 'rv-card');
  const h = el('div', 'rv-card-h');
  h.appendChild(icon(iconName));
  h.appendChild(el('h4', null, title));
  if (aside) h.appendChild(aside);
  s.appendChild(h);
  return s;
}

function rvFocus(d) {
  const items = d.focus || [];
  const s = rvSection('Review focus', 'shield-alert', el('span', 'dim', items.length ? `${items.length} spot${items.length === 1 ? '' : 's'}` : ''));
  if (!items.length) {
    s.appendChild(el('div', 'rv-quiet', 'Nothing stands out. Read the files in order.'));
    return s;
  }
  const list = el('ol', 'rv-focus');
  const shown = rv.showAllFocus ? items : items.slice(0, RV_FOCUS_SHOWN);
  for (const it of shown) {
    const li = el('li', 'sev-' + it.severity);
    li.appendChild(el('span', 'dot'));
    const txt = el('div', 'txt');
    const ttl = el('div', 'ttl', it.title);
    if (it.kind === 'ai' || it.kind === 'critical') ttl.prepend(rvTag('AI', 'muted', 'From the AI review pass, verified'), ' ');
    txt.appendChild(ttl);
    txt.appendChild(el('div', 'why', it.reason));
    li.appendChild(txt);
    if (it.path) {
      li.classList.add('link');
      li.title = 'Go to ' + it.path;
      li.addEventListener('click', () => rvGo(it.repo + '/' + it.path, true, it.hunk));
    } else if (it.kind === 'assumption' || it.kind === 'issue') {
      li.title = 'From the decision log (Rationale panel)';
    }
    list.appendChild(li);
  }
  s.appendChild(list);
  if (items.length > RV_FOCUS_SHOWN) {
    const more = el('button', 'ghost sm', rv.showAllFocus ? 'Show fewer' : `Show ${items.length - RV_FOCUS_SHOWN} more`);
    more.addEventListener('click', () => { rv.showAllFocus = !rv.showAllFocus; rvDrawSummary(rv.data); });
    s.appendChild(more);
  }
  return s;
}

function rvEvidence(d) {
  const ev = d.evidence || { commands: [], tests: [] };
  const ok = ev.commands.filter(c => c.done && c.ok).length;
  const bad = ev.commands.filter(c => c.done && !c.ok).length;
  const aside = el('span', 'dim', ev.commands.length ? `${ok} passed · ${bad} failed` : '');
  const s = rvSection('Evidence', 'activity', aside);
  if (!d.traced) {
    s.appendChild(el('div', 'rv-quiet',
      'No Claude Code session was found in this workspace. Evidence and the agent links on hunks come from its transcripts.'));
  } else if (!ev.commands.length) {
    s.appendChild(el('div', 'rv-quiet', 'The agent ran no test, build or lint command in this workspace.'));
  }
  if (ev.commands.length) {
    const list = el('div', 'rv-cmds');
    for (const c of ev.commands) {
      const row = el('div', 'rv-cmd');
      const head = el('div', 'rv-chead');
      const st = el('span', 'rv-cst ' + (!c.done ? 'run' : c.ok ? 'ok' : 'bad'));
      st.appendChild(icon(!c.done ? 'history' : c.ok ? 'check' : 'x'));
      st.title = !c.done ? 'No result recorded' : c.ok ? 'Passed' : 'Failed';
      head.appendChild(st);
      head.appendChild(rvTag(c.kind, 'muted'));
      const cmd = el('code', 'rv-ctext', c.command);
      cmd.title = c.command;
      head.appendChild(cmd);
      if (c.runs > 1) head.appendChild(el('span', 'dim', `×${c.runs}`));
      head.appendChild(el('span', 'dim', rvTime(c.time)));
      head.appendChild(c.afterLastEdit
        ? rvTag('after last edit', 'green', 'Ran on the code as it is now')
        : rvTag('before last edit', 'amber', 'The code changed after this ran'));
      row.appendChild(head);
      if (c.output) {
        row.appendChild(el('pre', 'rv-cout', c.output));
        head.classList.add('link');
        head.addEventListener('click', () => row.classList.toggle('open'));
      }
      list.appendChild(row);
    }
    s.appendChild(list);
  }
  const tests = el('div', 'rv-tests');
  tests.appendChild(el('span', 'k', 'Tests in this change: '));
  if (!ev.tests.length) tests.appendChild(el('span', 'none', 'none'));
  ev.tests.forEach((t, i) => {
    const a = el('a', null, t);
    a.href = '#';
    a.addEventListener('click', e => { e.preventDefault(); rvGo(t, true); });
    if (i) tests.appendChild(document.createTextNode(', '));
    tests.appendChild(a);
  });
  s.appendChild(tests);
  s.appendChild(rvReverts(d));
  return s;
}

// rvReverts: per repo, whether its changed tests fail with the change reverted.
function rvReverts(d) {
  const wrap = el('div', 'rv-reverts');
  const h = el('div', 'rv-subh', 'Revert check');
  h.title = 'Runs the repository\'s test command (repos.<name>.test in config.json) against the merge-base with only ' +
    'the changed tests in place. Tests that still pass do not test the change.';
  wrap.appendChild(h);
  const withTests = new Set((d.evidence.tests || []).map(t => t.split('/')[0]));
  for (const r of d.repos) {
    if (!r.files.length) continue;
    const c = (d.revert || {})[r.repo];
    const row = el('div', 'rv-cmd');
    const head = el('div', 'rv-chead');
    let st, cls, text;
    if (!c) { cls = 'run'; st = 'history'; text = withTests.has(r.repo) ? 'Not run' : 'No test changed'; }
    else if (c.state === 'running') { cls = 'run'; st = 'history'; text = 'Running…'; }
    else if (c.state === 'failed') { cls = 'bad'; st = 'alert'; text = 'Could not run: ' + c.err; }
    else if (c.caught) { cls = 'ok'; st = 'check'; text = `The tests fail without the change (exit ${c.exitCode}): they test it`; }
    else { cls = 'bad'; st = 'x'; text = 'The tests pass without the change: they do not test it'; }
    const dot = el('span', 'rv-cst ' + cls);
    dot.appendChild(icon(st));
    head.appendChild(dot);
    head.appendChild(el('b', 'rv-rrepo', r.repo));
    const t = el('span', 'rv-ctext', text);
    t.title = c && c.command ? `${text}\n\n${c.command}\nTests: ${(c.tests || []).join(', ')}` : text;
    head.appendChild(t);
    if (c && c.stale) head.appendChild(rvTag('stale', 'amber', 'The change moved on since this ran'));
    if (c && c.finished) head.appendChild(el('span', 'dim', rvTime(c.finished)));
    if ((!c || c.state !== 'running') && withTests.has(r.repo)) {
      const b = el('button', 'ghost xs', c ? 'Run again' : 'Run');
      b.addEventListener('click', ev => { ev.stopPropagation(); rvRevert(r.repo, b); });
      head.appendChild(b);
    }
    row.appendChild(head);
    if (c && c.output) {
      row.appendChild(el('pre', 'rv-cout', c.output));
      head.classList.add('link');
      head.addEventListener('click', () => row.classList.toggle('open'));
    }
    wrap.appendChild(row);
  }
  return wrap;
}

async function rvRevert(repo, btn) {
  const p = pin();
  btn.disabled = true;
  try { await api.post(`${p.base}/tasks/${p.slug}/changes/revert`, { repo }); }
  catch (e) { toast(e.message); btn.disabled = false; return; }
  if (!stale(p)) loadReview(p, false).catch(() => {});
}

// rvAI: the AI pass. Groups by intent, rated; findings a second model confirmed.
function rvAI(d) {
  const a = d.ai || {};
  const aside = el('span', 'dim');
  const s = rvSection('AI review', 'sparkle', aside);
  const run = (label) => {
    const b = el('button', 'ghost sm');
    b.appendChild(icon('sparkle'));
    b.appendChild(el('span', null, label));
    b.title = 'Groups the hunks by intent (haiku), rates each group and lists findings (opus), and keeps only the ' +
      'findings that quote their hunk and that a second look confirms (sonnet). Read-only, in this workspace.';
    b.addEventListener('click', async () => {
      const p = pin();
      b.disabled = true;
      try { d.ai = await api.post(`${p.base}/tasks/${p.slug}/changes/ai`); }
      catch (e) { toast(e.message); b.disabled = false; return; }
      if (stale(p)) return;
      rvDrawSummary(d);
      rvSoon(p);
    });
    return b;
  };
  if (!a.state) {
    s.appendChild(el('div', 'rv-quiet', 'Not run yet. It groups the change by intent, rates each group for reading effort ' +
      'and blast radius, and lists only findings a second model could reproduce.'));
    s.appendChild(run('Run AI review'));
    return s;
  }
  if (a.state === 'running') {
    aside.textContent = 'running since ' + rvTime(a.started);
    s.appendChild(el('div', 'rv-quiet', 'Reading the change. This takes a few minutes; the page updates when it is done.'));
    return s;
  }
  if (a.state === 'failed') {
    s.appendChild(el('div', 'rv-err', a.err || 'The pass failed.'));
    s.appendChild(run('Run again'));
    return s;
  }
  aside.textContent = rvTime(a.finished);
  if (a.stale) {
    const st = el('div', 'rv-stale');
    st.appendChild(el('span', null, 'The change moved on since this pass. Findings on hunks that changed are marked.'));
    st.appendChild(run('Run again'));
    s.appendChild(st);
  }
  const groups = el('div', 'rv-groups');
  for (const g of a.groups || []) {
    const row = el('div', 'rv-group');
    const top = el('div', 'rv-ghead');
    top.appendChild(el('b', null, g.title));
    if (g.significance) top.appendChild(rvScale('Effort', g.significance, 'Significance: how much reading it needs, 1 to 5'));
    if (g.criticality) top.appendChild(rvScale('Impact', g.criticality, 'Criticality: the blast radius if it is wrong, 1 to 5'));
    top.appendChild(el('span', 'dim', `${g.hunks.length} hunk${g.hunks.length === 1 ? '' : 's'}`));
    row.appendChild(top);
    if (g.intent) row.appendChild(el('div', 'why', g.intent));
    if (g.why) row.appendChild(el('div', 'why crit', g.why));
    if (g.check && g.check.length) {
      const ul = el('ul', 'rv-check');
      for (const c of g.check) ul.appendChild(el('li', null, c));
      row.appendChild(ul);
    }
    row.addEventListener('click', e => { if (!e.target.closest('button')) rvGoHunk(g.hunks[0]); });
    groups.appendChild(row);
  }
  s.appendChild(groups);

  const all = a.findings || [];
  const live = all.filter(f => !f.dismissed);
  const list = el('div', 'rv-findings');
  for (const f of rv.showDismissed ? all : live) list.appendChild(rvFinding(f));
  if (!all.length) list.appendChild(el('div', 'rv-quiet', 'No finding survived verification.'));
  s.appendChild(list);
  const foot = el('div', 'dim rv-afoot');
  const notes = [];
  if (a.unverified) notes.push(a.unverified === 1
    ? '1 finding left out: it did not quote its hunk exactly, or a second look did not confirm it'
    : `${a.unverified} findings left out: they did not quote their hunk exactly, or a second look did not confirm them`);
  if (notes.length) foot.appendChild(el('span', null, notes.join(' · ')));
  if (all.length > live.length) {
    const b = el('button', 'ghost xs', rv.showDismissed ? 'Hide dismissed' : `Show ${all.length - live.length} dismissed`);
    b.addEventListener('click', () => { rv.showDismissed = !rv.showDismissed; rvDrawSummary(rv.data); });
    foot.appendChild(b);
  }
  if (!a.stale) foot.appendChild(run('Run again'));
  s.appendChild(foot);
  return s;
}

function rvScale(label, n, title) {
  const t = rvTag(`${label} ${n}/5`, n >= 4 ? 'red' : n === 3 ? 'amber' : 'muted', title);
  return t;
}

function rvFinding(f) {
  const row = el('div', 'rv-finding grade-' + f.grade + (f.dismissed ? ' dismissed' : ''));
  row.appendChild(el('span', 'dot'));
  const txt = el('div', 'txt');
  const ttl = el('div', 'ttl', f.title);
  if (f.stale) ttl.appendChild(rvTag('hunk changed', 'amber', 'The hunk this is about is no longer in the change'));
  txt.appendChild(ttl);
  txt.appendChild(el('div', 'why', f.detail));
  const where = el('a', 'rv-fwhere', `${f.repo}/${f.path}`);
  where.href = '#';
  where.addEventListener('click', e => { e.preventDefault(); rvGo(f.repo + '/' + f.path, true, f.hunk); });
  txt.appendChild(where);
  if (f.verdict) txt.appendChild(el('div', 'dim', 'Confirmed: ' + f.verdict));
  row.appendChild(txt);
  const acts = el('div', 'rv-facts');
  txt.appendChild(acts);
  const vote = (v, label, title) => {
    const b = el('button', 'ghost xs' + (f.vote === v ? ' on' : ''), label);
    b.title = title;
    b.addEventListener('click', () => rvMarkFinding(f, { vote: f.vote === v ? '' : v }));
    return b;
  };
  acts.appendChild(vote('up', 'Helpful', 'This finding was worth reading'));
  acts.appendChild(vote('down', 'Not helpful', 'This finding was noise'));
  const dis = el('button', 'ghost xs', f.dismissed ? 'Restore' : 'Dismiss');
  dis.title = f.dismissed ? 'Show it in the focus list again' : 'Take it off the focus list; it stays dismissed if raised again';
  dis.addEventListener('click', () => rvMarkFinding(f, { dismissed: !f.dismissed }));
  acts.appendChild(dis);
  return row;
}

async function rvMarkFinding(f, body) {
  const p = pin();
  try { await api.put(`${p.base}/tasks/${p.slug}/changes/ai/finding`, { id: f.id, ...body }); }
  catch (e) { toast(e.message); return; }
  if (stale(p)) return;
  if ('vote' in body) f.vote = body.vote;
  if ('dismissed' in body) f.dismissed = body.dismissed;
  rvDrawSummary(rv.data);
  loadReview(p, false).catch(() => {});
}

// rvThreads: every comment of the task, open blocking ones first.
function rvThreads(d) {
  const ts = (d.threads || []).slice();
  const open = ts.filter(t => t.blocking && !t.satisfied).length;
  const s = rvSection('Comments', 'send', el('span', 'dim', open ? `${open} blocking` : ts.length ? `${ts.length}` : ''));
  if (!ts.length) {
    s.appendChild(el('div', 'rv-quiet', 'Click a line number in the diff to comment. A comment can go to the agent as a ' +
      'follow-up, and a blocking one keeps the task out of Done until you mark it Satisfied.'));
    return s;
  }
  const rank = t => t.blocking && !t.satisfied ? 0 : t.satisfied ? 2 : 1;
  ts.sort((a, b) => rank(a) - rank(b));
  const list = el('div', 'rv-tlist');
  for (const t of ts) {
    const row = el('div', 'rv-tsum ' + (t.blocking && !t.satisfied ? 'blocking' : t.satisfied ? 'satisfied' : ''));
    row.appendChild(el('span', 'dot'));
    const txt = el('div', 'txt');
    const where = el('div', 'ttl', `${t.repo}/${t.path}${t.line ? ':' + t.line : ''}`);
    if (t.outdated) where.appendChild(rvTag('earlier version', 'muted'));
    txt.appendChild(where);
    const last = t.comments[t.comments.length - 1];
    txt.appendChild(el('div', 'why', last.text));
    const sent = !!last.sent;
    txt.appendChild(el('div', 'dim', (t.blocking ? (t.satisfied ? 'Satisfied' : 'Blocking') : 'Comment') +
      (sent ? ' · sent to the agent' : last.sendErr ? ' · not sent' : '')));
    row.appendChild(txt);
    row.title = 'Go to the comment';
    row.addEventListener('click', () => rvGo(t.repo + '/' + t.path, true, t.hunk, t.id));
    list.appendChild(row);
  }
  s.appendChild(list);
  return s;
}

// rvSize: the size guard. A change past a few hundred lines is better read as
// a stack; the split writes one to a new branch and leaves this one alone.
function rvSize(d) {
  const box = el('div', 'rv-sizebox');
  box.appendChild(icon('alert'));
  const txt = el('div', 'txt');
  txt.appendChild(el('b', null, `A large change: ${d.size.lines} lines in ${d.size.files} files.`));
  txt.appendChild(el('span', null, ' Past about 400 lines, a review misses most defects. Split it into a stack of commits ' +
    (d.ai && d.ai.state === 'done' && !d.ai.stale ? 'by the AI pass\'s intent groups' : 'by layer') +
    ', on a new branch next to this one. The stack is checked to end at exactly this working tree.'));
  box.appendChild(txt);
  const acts = el('div', 'rv-sizeacts');
  for (const r of d.repos) {
    if (r.files.filter(f => !f.noise).length < 2) continue;
    const b = el('button', 'ghost sm');
    b.appendChild(icon('branch'));
    b.appendChild(el('span', null, `Split ${r.repo}`));
    b.addEventListener('click', async () => {
      const p = pin();
      b.disabled = true;
      let st;
      try { st = await api.post(`${p.base}/tasks/${p.slug}/changes/split`, { repo: r.repo }); }
      catch (e) { toast(e.message); b.disabled = false; return; }
      if (stale(p)) return;
      const done = el('div', 'rv-stack');
      done.appendChild(el('b', null, `${st.branch}: ${st.commits.length} commit${st.commits.length === 1 ? '' : 's'}`));
      const ol = el('ol');
      for (const c of st.commits) ol.appendChild(el('li', null, `${c.title} (${c.files.length} file${c.files.length === 1 ? '' : 's'}) ${c.sha.slice(0, 8)}`));
      done.appendChild(ol);
      done.appendChild(el('span', 'dim', `Your branch ${r.branch} and its working tree are unchanged.`));
      b.replaceWith(done);
    });
    acts.appendChild(b);
  }
  box.appendChild(acts);
  return box;
}

/* ------------------------------------------------------------------ actions */

async function rvSetViewed(repo, f, viewed) {
  const p = pin();
  const key = rvKey(repo, f);
  try {
    await api.put(`${p.base}/tasks/${p.slug}/changes/viewed`,
      { repo, path: f.path, hash: f.hash, hunks: f.hunks.map(h => h.id), viewed });
  } catch (e) { toast(e.message); return; }
  if (stale(p)) return;
  f.viewed = viewed ? 'viewed' : '';
  // The page may be drawn from an earlier answer than rv.data: mark both.
  const cur = rvFiles(rv.data).find(x => x.key === key);
  if (cur) cur.f.viewed = f.viewed;
  // The reviewer's last word on this file is the mark: forget a hand-made
  // open or close, so a viewed file folds and an unviewed one opens.
  rv.open.delete(key);
  const box = rvFileBox(key);
  if (box && cur) {
    if ($('#rv-hide-viewed').checked && viewed) {
      box.remove();
    } else {
      box.replaceWith(rvFile(cur.r, cur.f));
    }
  }
  rvDrawTree(rv.data);
  rvProgress(rv.data);
  rvMark(key);
  if (viewed) {
    const next = rvNextUnviewed(key);
    if (next) rvGo(next, false);
  }
}

function rvOpenFile(repo, path) {
  state.fileFromReview = true;
  openFile(repo + '/' + path);
}

function rvFileBox(key) {
  return [...document.querySelectorAll('#rv-main .rv-file')].find(b => b.dataset.key === key);
}

function rvGoHunk(id) {
  for (const x of rvFiles(rv.data)) if (x.f.hunks.some(h => h.id === id)) return rvGo(x.key, true, id);
}

// rvGo scrolls to a file (opening it when asked) and, optionally, a hunk or
// a comment thread in it.
function rvGo(key, open, hunk, thread) {
  let box = rvFileBox(key);
  if (!box && rv.data) {
    // Hidden as viewed: show it after all; the reader asked for it.
    $('#rv-hide-viewed').checked = false;
    rvDraw(rv.data, true);
    box = rvFileBox(key);
  }
  if (!box) return;
  if (open && !box.classList.contains('open')) box.querySelector('.rv-fhead').click();
  rvMark(key);
  let target = box;
  if (hunk) {
    const hb = box.querySelector(`.rv-hunk[data-hunk="${CSS.escape(hunk)}"]`);
    // A folded hunk is opened: the reader came for its lines.
    if (hb) { const fold = hb.querySelector('.rv-fold'); if (fold && (thread || open)) fold.click(); target = hb; }
  }
  if (thread) target = box.querySelector(`.rv-thread[data-thread="${CSS.escape(thread)}"]`) || target;
  target.scrollIntoView({ block: thread ? 'center' : 'start', behavior: 'smooth' });
  if (target !== box) {
    target.classList.add('flash');
    setTimeout(() => target.classList.remove('flash'), 1600);
  }
}

function rvMark(key) {
  rv.current = key || '';
  for (const b of document.querySelectorAll('#rv-main .rv-file')) b.classList.toggle('current', b.dataset.key === key);
  for (const r of document.querySelectorAll('#rv-tree .rv-trow')) {
    const on = r.dataset.key === key;
    r.classList.toggle('current', on);
    if (on) r.scrollIntoView({ block: 'nearest' });
  }
}

function rvOrder() {
  return [...document.querySelectorAll('#rv-main .rv-file')].map(b => b.dataset.key);
}

function rvNextUnviewed(after) {
  const keys = rvOrder();
  const files = new Map(rvFiles(rv.data).map(x => [x.key, x.f]));
  const start = Math.max(0, keys.indexOf(after) + 1);
  for (let i = 0; i < keys.length; i++) {
    const k = keys[(start + i) % keys.length];
    const f = files.get(k);
    if (f && f.viewed !== 'viewed' && !f.noise && k !== after) return k;
  }
  return '';
}

// Keys act only while the review is on screen and nothing takes typing.
document.addEventListener('keydown', e => {
  if (!reviewOpen() || !$('#task').classList.contains('active')) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const t = e.target;
  if (t && (t.closest('input, textarea, select, [contenteditable], .xterm') || document.querySelector('.modal:not(.hidden)'))) return;
  const keys = rvOrder();
  const i = keys.indexOf(rv.current);
  switch (e.key) {
    case 'j': if (keys.length) rvGo(keys[Math.min(keys.length - 1, i + 1)], false); break;
    case 'k': if (keys.length) rvGo(keys[Math.max(0, i - 1)], false); break;
    case 'n': { const k = rvNextUnviewed(rv.current); if (k) rvGo(k, true); break; }
    case 'v': {
      if (!rv.current || !rv.data) return;
      const x = rvFiles(rv.data).find(y => y.key === rv.current);
      if (x) rvSetViewed(x.repo, x.f, x.f.viewed !== 'viewed');
      break;
    }
    case 'r': loadReview(pin(), true, true).catch(err => toast(err.message)); break;
    default: return;
  }
  e.preventDefault();
});

/* ------------------------------------------------------------------ the queue across tasks */

// Every computer's open tasks with something to review: this one's, and in a
// fleet each online peer's, through the node proxy.
async function loadQueue() {
  const nodes = [''];
  if (state.fleet.enabled) for (const p of onlinePeers()) nodes.push(p.name);
  const lists = await Promise.all(nodes.map(n =>
    api.get(`${nodeBase(n)}/review/queue`).then(items => items.map(it => ({ ...it, node: n }))).catch(() => [])));
  const rank = { you: 0, agent: 1, none: 2 };
  const items = lists.flat().sort((a, b) => rank[a.turn] - rank[b.turn] || b.risk.score - a.risk.score);
  renderQueue(items);
}

function renderQueue(items) {
  const host = $('#reviews-list');
  host.innerHTML = '';
  if (!items.length) {
    const empty = el('div', 'rv-empty');
    empty.appendChild(icon('check', 'big'));
    empty.appendChild(el('h3', null, 'Nothing to review'));
    empty.appendChild(el('p', null, 'No open task has changes or blocking comments waiting.'));
    host.appendChild(empty);
    return;
  }
  for (const it of items) {
    const row = el('div', 'rq-row turn-' + it.turn);
    row.tabIndex = 0;
    const turn = el('span', 'rq-turn', it.turn === 'you' ? 'Your turn' : it.turn === 'agent' ? 'Agent' : 'Reviewed');
    row.appendChild(turn);
    const txt = el('div', 'txt');
    const ttl = el('div', 'ttl', it.name);
    if (state.fleet.enabled) ttl.appendChild(el('span', 'dim', ' · ' + (isLocal(it.node) ? 'this computer' : it.node)));
    txt.appendChild(ttl);
    txt.appendChild(el('div', 'why', it.why));
    row.appendChild(txt);
    const nums = el('div', 'rq-nums');
    nums.appendChild(el('span', null, `${it.files - it.left}/${it.files} viewed`));
    if (it.changed) nums.appendChild(rvTag(`${it.changed} changed since`, 'amber'));
    if (it.blocking) nums.appendChild(rvTag(`${it.blocking} blocking`, 'red'));
    const risk = el('span', 'rv-risk lvl-' + it.risk.level);
    risk.appendChild(icon('gauge'));
    risk.appendChild(el('span', null, `Risk ${it.risk.score}`));
    risk.title = it.risk.reasons.join('\n') || 'nothing stands out';
    nums.appendChild(risk);
    row.appendChild(nums);
    const go = async () => {
      await openTask(it.slug, it.node);
      // A re-tour: only what is left, with what was read before folded.
      $('#rv-hide-viewed').checked = true;
      showReview();
    };
    row.addEventListener('click', go);
    row.addEventListener('keydown', e => { if (e.key === 'Enter') go(); });
    host.appendChild(row);
  }
}

$('#nav-reviews').addEventListener('click', () => {
  leaveTask();
  showView('reviews');
  loadQueue().catch(e => toast(e.message));
  if (rv.queuePoll) return;
  rv.queuePoll = setInterval(() => {
    if (!$('#reviews').classList.contains('active')) { clearInterval(rv.queuePoll); rv.queuePoll = null; return; }
    if (!document.hidden) loadQueue().catch(() => {});
  }, 30000);
});
