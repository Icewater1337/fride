/* The quality record, under the review queue: what became of the tasks'
   changes after they merged (internal/quality), and how the AI review did
   when it was replayed on the ones that went wrong. It is this computer's
   record, so it is read from this computer's API only. */

const qa = { by: 'month', busy: false };

async function loadQuality() {
  const d = await api.get('/api/quality?by=' + qa.by);
  renderQuality(d);
}

function qaDate(s) { return s ? new Date(s).toLocaleDateString() : ''; }
function qaPct(f) { return f < 0 ? '–' : Math.round(100 * f) + '%'; }

function qaTable(heads, rows) {
  const wrap = el('div', 'tablewrap');
  const t = el('table', 'table');
  const tr = el('tr');
  for (const h of heads) tr.appendChild(el('th', null, h));
  const thead = el('thead');
  thead.appendChild(tr);
  t.appendChild(thead);
  const body = el('tbody');
  for (const r of rows) body.appendChild(r);
  t.appendChild(body);
  wrap.appendChild(t);
  return wrap;
}

function qaRow(cells) {
  const tr = el('tr');
  for (const c of cells) {
    const td = el('td');
    if (c instanceof Node) td.appendChild(c); else td.textContent = c;
    tr.appendChild(td);
  }
  return tr;
}

function renderQuality(d) {
  const host = $('#quality');
  host.innerHTML = '';

  const head = el('div', 'qa-head');
  head.appendChild(el('h3', 'sh', 'After merge'));
  const by = el('select', 'qa-by');
  for (const v of ['month', 'week']) {
    const o = el('option', null, 'By ' + v);
    o.value = v;
    by.appendChild(o);
  }
  by.value = qa.by;
  by.addEventListener('change', () => { qa.by = by.value; loadQuality().catch(e => toast(e.message)); });
  head.appendChild(by);
  const again = el('button', 'ghost sm');
  again.appendChild(icon('refresh'));
  again.appendChild(el('span', null, d.refreshing || qa.busy ? 'Looking…' : 'Look again'));
  again.disabled = d.refreshing || qa.busy;
  again.title = 'Fetch each base and look for merges, reverts and follow-up fixes';
  again.addEventListener('click', async () => {
    qa.busy = true;
    again.disabled = true;
    setLabel(again, 'Looking…');
    try {
      const res = await api.post('/api/quality/refresh');
      for (const n of res.notes || []) toast(n, 'info');
    } catch (e) { toast(e.message); }
    qa.busy = false;
    loadQuality().catch(e => toast(e.message));
  });
  head.appendChild(again);
  host.appendChild(head);

  const looked = d.refreshed ? ' Last looked ' + new Date(d.refreshed).toLocaleString() + '.' : ' Not looked yet.';
  host.appendChild(el('p', 'hint', 'Changes made in FrIDE tasks, by the period they merged, and how many were later ' +
    'reverted, followed by fixes, or caused an incident. The latest periods are still filling in; follow-ups ' +
    'are found by heuristics (docs/REVIEW.md).' + looked));

  const t = d.total;
  if (!t.merged) {
    host.appendChild(el('p', 'hint', d.pending
      ? `${d.pending} recorded change(s), none seen merged yet.`
      : 'No change of a FrIDE task recorded yet.'));
  } else {
    const rows = d.rows.map(r => qaRow([r.period, r.repo, r.merged, r.reverted, r.followedUp, r.incident,
      `${r.bad} (${Math.round(100 * r.bad / r.merged)}%)`]));
    const tot = qaRow(['All', '', t.merged, t.reverted, t.followedUp, t.incident,
      `${t.bad} (${Math.round(100 * t.bad / t.merged)}%)`]);
    tot.className = 'qa-total';
    rows.push(tot);
    host.appendChild(qaTable(['Period', 'Repository', 'Merged', 'Reverted', 'Followed up', 'Incident', 'Went wrong'], rows));
    if (d.pending) host.appendChild(el('p', 'hint', `${d.pending} more recorded change(s) not seen merged yet.`));
    host.appendChild(qaChanges(d.changes));
  }

  host.appendChild(el('h3', 'sh', 'Reviewer evals'));
  const c = d.cases || {};
  const n = Object.values(c).reduce((a, b) => a + b, 0);
  const sched = d.schedule && d.schedule.evalHours
    ? ` Replayed every ${d.schedule.evalHours} h, ${d.schedule.evalMax} case(s) at a time.`
    : ' Replayed when you run fride review eval, or on the schedule set by quality.evalHours in config.json.';
  host.appendChild(el('p', 'hint', n
    ? `${n} case(s): ${c.reverted || 0} reverted, ${c.followup || 0} followed up, ${c.incident || 0} incident, ` +
      `${c.votes || 0} with voted AI findings. Each replay asks the AI review whether it would have caught them, ` +
      `and spends model calls.` + sched
    : 'Nothing in the eval set yet: it fills with changes that went wrong and AI findings you voted on.'));
  if (n) {
    const run = el('button', 'ghost sm');
    run.appendChild(icon('sparkle'));
    run.appendChild(el('span', null, d.evaluating ? 'Replaying…' : `Replay ${Math.min(n, d.schedule.evalMax)} case(s)`));
    run.disabled = d.evaluating;
    run.title = 'Runs the AI review on the cases replayed longest ago: three model turns per case';
    run.addEventListener('click', async () => {
      try {
        await api.post('/api/quality/evals', {});
        toast('Replaying in the background: the result shows here when it is done.', 'info');
      } catch (e) { toast(e.message); }
      loadQuality().catch(() => {});
    });
    host.appendChild(run);
  }
  if (d.runs && d.runs.length) {
    host.appendChild(qaTable(['Run', 'Cases', 'Caught', 'Missed', 'Errors', 'Caught share', 'Precision on votes'],
      d.runs.map(r => qaRow([new Date(r.started).toLocaleString(), r.cases, r.caught, r.missed, r.errors,
        qaPct(r.recall), qaPct(r.precision)]))));
  }
}

// qaChanges lists the merged changes, what went wrong with each and why it
// is believed, and lets a person record an incident on one.
function qaChanges(changes) {
  const box = el('details', 'qa-changes');
  const bad = changes.filter(c => (c.reverted || []).length || (c.followUps || []).length || (c.incidents || []).length);
  box.appendChild(el('summary', null, `${changes.length} merged change(s), ${bad.length} went wrong`));
  if (bad.length) box.open = true;
  const rows = changes.map(c => {
    const what = el('div');
    const ttl = el('div', 'qa-ttl', c.name || c.slug);
    what.appendChild(ttl);
    what.appendChild(el('div', 'hint', `${c.repo}${c.merged.pr || ''} · ${c.branch} · ${c.merged.how}`));
    const out = el('div', 'qa-out');
    if (!bad.includes(c)) out.appendChild(rvTag('fine so far', 'muted'));
    for (const e of c.reverted || []) {
      out.appendChild(rvTag('reverted', 'red', `${e.commit.slice(0, 10)} "${e.subject}": ${e.detail}`));
    }
    for (const e of c.followUps || []) {
      out.appendChild(rvTag('followed up', 'amber', `${e.commit.slice(0, 10)} "${e.subject}": ${e.detail}`));
    }
    for (const i of c.incidents || []) {
      out.appendChild(rvTag('incident', 'red', `${qaDate(i.at)}: ${i.why}${i.path ? ' (' + i.path + ')' : ''}`));
    }
    const rec = el('button', 'ghost sm', 'Incident…');
    rec.title = 'Record that this change caused trouble in use';
    rec.addEventListener('click', () => qaIncident(c));
    return qaRow([what, qaDate(c.merged.at), out, rec]);
  });
  box.appendChild(qaTable(['Change', 'Merged', 'Outcome', ''], rows));
  return box;
}

async function qaIncident(c) {
  const body = el('div', 'qa-form');
  body.appendChild(el('p', null, `${c.name || c.slug} (${c.repo}${c.merged.pr || ''}), merged ${qaDate(c.merged.at)}.`));
  const why = el('textarea');
  why.rows = 3;
  why.placeholder = 'What happened';
  body.appendChild(why);
  const path = el('input');
  path.type = 'text';
  path.placeholder = 'The file it was in, if known (path in the repository)';
  body.appendChild(path);
  if (!await ask({ title: 'Record an incident', body, ok: 'Record' })) return;
  if (!why.value.trim()) { toast('Say what happened.'); return; }
  try {
    await api.post('/api/quality/incidents', { target: c.id, repo: c.repo, path: path.value.trim(), why: why.value.trim() });
    toast('Incident recorded; the change is in the eval set now.', 'ok');
  } catch (e) { toast(e.message); }
  loadQuality().catch(e => toast(e.message));
}

// The Reviews view is where this lives; review.js opens the view.
$('#nav-reviews').addEventListener('click', () => { loadQuality().catch(e => toast(e.message)); });
