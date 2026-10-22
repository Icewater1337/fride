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
  poll: null,
  loading: false,
};

const RV_HIDE_KEY = 'fride-review-hide-viewed';

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
  for (const r of d.repos) for (const f of r.files) out.push({ repo: r.repo, f, key: rvKey(r.repo, f) });
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
  hideReview();
  $('#rv-tree').innerHTML = '';
  $('#rv-main').innerHTML = '';
  $('#rv-moved').classList.add('hidden');
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
  if (!reviewOpen()) { rv.drawnSig = ''; return; }
  const sig = rvSig(d);
  if (draw && (force || !rv.drawnSig || sig === rv.drawnSig)) {
    rvDraw(d, !force && !!rv.drawnSig);
    $('#rv-moved').classList.add('hidden');
  } else if (sig !== rv.drawnSig) {
    rvMoved(d);
  } else {
    // Same files, same versions: only the evidence may have moved on, and it
    // is safe to redraw in place.
    rvDrawFocusEvidence(d);
  }
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

function rvDraw(d, keepScroll) {
  const main = $('#rv-main');
  const top = main.scrollTop;
  rv.drawnSig = rvSig(d);
  rv.drawnData = d;
  rvDrawTree(d);
  main.innerHTML = '';
  main.appendChild(el('div', 'rv-summary'));
  rvDrawFocusEvidence(d);
  const hide = $('#rv-hide-viewed').checked;
  let shown = 0;
  for (const r of d.repos) {
    if (r.err) main.appendChild(el('div', 'rv-err', `${r.repo}: ${r.err}`));
    for (const f of r.files) {
      if (hide && f.viewed === 'viewed') continue;
      main.appendChild(rvFile(r, f));
      shown++;
    }
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
}

function rvState(f) {
  if (f.viewed === 'viewed') return { cls: 'viewed', icon: 'check', title: 'Viewed' };
  if (f.viewed === 'changed') return { cls: 'changed', icon: 'refresh', title: 'Changed since you viewed it' };
  return { cls: 'unread', icon: null, title: 'Not viewed yet' };
}

function rvStatusCls(s) { return s === 'A' ? 'new' : s === 'D' ? 'del' : s === 'R' || s === 'C' ? 'ren' : 'mod'; }

function rvDrawTree(d) {
  const tree = $('#rv-tree');
  tree.innerHTML = '';
  const hide = $('#rv-hide-viewed').checked;
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
    // Noise last: it is folded in the diff too.
    const order = [...r.files].sort((a, b) => (!!a.noise - !!b.noise));
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
      row.appendChild(el('span', 'rv-tnum', f.binary ? 'bin' : `+${f.added} −${f.deleted}`));
      row.addEventListener('click', () => rvGo(key, true));
      tree.appendChild(row);
    }
  }
}

// A file is open unless the reviewer closed it, it is viewed, or it is noise.
function rvIsOpen(key, f) {
  if (rv.open.has(key)) return rv.open.get(key);
  return f.viewed !== 'viewed' && !f.noise;
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
  if (f.viewed === 'changed') head.appendChild(rvTag('changed since viewed', 'amber'));
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
  if (f.truncated) {
    body.appendChild(rvNote(f.binary ? 'Binary file.' : 'Too large to show here.', f.status !== 'D' && !f.binary ? () => rvOpenFile(r.repo, f.path) : null));
    return;
  }
  if (f.binary) { body.appendChild(rvNote('Binary file. Not shown.')); return; }
  if (!f.hunks.length) {
    body.appendChild(rvNote(f.status === 'R' ? 'Renamed without changes.' : f.status === 'A' ? 'Empty file.' : 'Only the file mode changed.'));
    return;
  }
  for (const h of f.hunks) body.appendChild(rvHunk(h));
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

function rvHunk(h) {
  const box = el('div', 'rv-hunk');
  box.dataset.hunk = h.id;
  if (h.traces && h.traces.length) box.appendChild(rvTrace(h.traces, h.unattributed || 0));
  const head = el('div', 'rv-hhead');
  head.appendChild(el('span', 'rng', `@@ −${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@`));
  if (h.header) head.appendChild(el('span', 'fn', h.header));
  box.appendChild(head);

  const table = el('div', 'rv-lines');
  const fill = () => {
    let o = h.oldStart, n = h.newStart;
    const frag = document.createDocumentFragment();
    for (const l of h.lines) {
      const k = l[0];
      const row = el('div', 'rv-l ' + (k === '+' ? 'add' : k === '-' ? 'del' : 'ctx'));
      row.appendChild(el('span', 'ln', k === '+' ? '' : String(o)));
      row.appendChild(el('span', 'ln', k === '-' ? '' : String(n)));
      row.appendChild(el('span', 'sg', k === ' ' ? '' : k));
      row.appendChild(el('span', 'tx', l.slice(1)));
      if (k !== '+') o++;
      if (k !== '-') n++;
      frag.appendChild(row);
    }
    table.appendChild(frag);
  };
  if (h.mechanical) {
    const fold = el('button', 'rv-fold');
    fold.appendChild(el('span', null, h.mechanical === 'whitespace' ? 'Whitespace only' : h.mechanical));
    fold.appendChild(el('span', 'dim', `${h.lines.length} lines · show`));
    fold.addEventListener('click', () => { fold.remove(); fill(); });
    box.appendChild(fold);
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

/* ------------------------------------------------------------------ focus and evidence */

const RV_FOCUS_SHOWN = 7;

function rvDrawFocusEvidence(d) {
  const host = $('#rv-main .rv-summary');
  if (!host) return;
  host.innerHTML = '';
  host.appendChild(rvFocus(d));
  host.appendChild(rvEvidence(d));
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
    txt.appendChild(el('div', 'ttl', it.title));
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
    more.addEventListener('click', () => { rv.showAllFocus = !rv.showAllFocus; rvDrawFocusEvidence(rv.data); });
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
        const out = el('pre', 'rv-cout', c.output);
        row.appendChild(out);
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
  return s;
}

/* ------------------------------------------------------------------ actions */

async function rvSetViewed(repo, f, viewed) {
  const p = pin();
  const key = rvKey(repo, f);
  try {
    await api.put(`${p.base}/tasks/${p.slug}/changes/viewed`, { repo, path: f.path, hash: f.hash, viewed });
  } catch (e) { toast(e.message); return; }
  if (stale(p)) return;
  f.viewed = viewed ? 'viewed' : '';
  // The reviewer's last word on this file is the mark: forget a hand-made
  // open or close, so a viewed file folds and an unviewed one opens.
  rv.open.delete(key);
  const box = rvFileBox(key);
  if (box && rv.data) {
    const r = rv.data.repos.find(x => x.repo === repo);
    if ($('#rv-hide-viewed').checked && viewed) {
      box.remove();
    } else {
      box.replaceWith(rvFile(r, f));
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

// rvGo scrolls to a file (opening it when asked) and, optionally, a hunk.
function rvGo(key, open, hunk) {
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
  const target = hunk ? box.querySelector(`.rv-hunk[data-hunk="${CSS.escape(hunk)}"]`) || box : box;
  target.scrollIntoView({ block: 'start', behavior: 'smooth' });
  if (hunk && target !== box) {
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
