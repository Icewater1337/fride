// The bridge inside an orchestrator panel (docs/PANELS.md). It runs in a
// sandboxed frame with no origin of its own, so it cannot reach FrIDE's API
// itself: every call is a postMessage to the FrIDE page around it (panels.js),
// which makes the call, marks it as the panel's for the audit log, and posts
// the answer back.
(function () {
  'use strict';
  const panel = window.FRIDE_PANEL || '';
  let seq = 0;
  const waiting = new Map();   // request id → {resolve, reject}
  const subs = new Map();      // subscription id → callback

  function call(op, args) {
    return new Promise((resolve, reject) => {
      const id = ++seq;
      waiting.set(id, { resolve, reject });
      parent.postMessage(Object.assign({ fride: 1, id, op }, args), '*');
    });
  }

  window.addEventListener('message', e => {
    // Only the FrIDE page around us talks to the bridge.
    if (e.source !== parent) return;
    const m = e.data;
    if (!m || m.fride !== 1) return;
    if (m.event === 'topic') {
      const cb = subs.get(m.sub);
      if (cb) { try { cb(m.data); } catch (err) { report(err); } }
      return;
    }
    if (m.event === 'theme') { applyTheme(m.theme, m.css); return; }
    const w = waiting.get(m.id);
    if (!w) return;
    waiting.delete(m.id);
    if (m.ok) w.resolve(m.result); else w.reject(new Error(m.error || 'failed'));
  });

  function applyTheme(theme, css) {
    let st = document.getElementById('fride-theme');
    if (!st) {
      st = document.createElement('style');
      st.id = 'fride-theme';
      (document.head || document.documentElement).prepend(st);
    }
    st.textContent = css;
    document.documentElement.setAttribute('data-theme', theme);
    fride.theme = theme;
  }

  function report(err) {
    const msg = err && (err.stack || err.message) || String(err);
    parent.postMessage({ fride: 1, op: 'error', error: String(msg).slice(0, 2000) }, '*');
  }
  window.addEventListener('error', e => report(e.error || e.message));
  window.addEventListener('unhandledrejection', e => report(e.reason));

  function h(tag, attrs, ...children) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v == null || v === false) continue;
      if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
      else if (k === 'style' && typeof v === 'object') Object.assign(n.style, v);
      else if (k === 'class') n.className = v;
      else n.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat()) {
      if (c == null || c === false) continue;
      n.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return n;
  }

  const esc = s => String(s).replace(/[&<>"']/g,
    c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  function ago(d) {
    const s = Math.round((Date.now() - new Date(d).getTime()) / 1000);
    if (!isFinite(s)) return '';
    if (s < 60) return 'just now';
    if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 86400) return Math.round(s / 3600) + ' h ago';
    return Math.round(s / 86400) + ' d ago';
  }

  const fride = window.fride = {
    panel,
    theme: 'light',
    api: (method, path, body) => call('api', { method, path, body }),
    ask: (text, data) => call('ask', { text, data }),
    subscribe(topic, cb) {
      const sub = ++seq;
      subs.set(sub, cb);
      parent.postMessage({ fride: 1, op: 'subscribe', sub, topic }, '*');
      return () => {
        subs.delete(sub);
        parent.postMessage({ fride: 1, op: 'unsubscribe', sub }, '*');
      };
    },
    state: {
      get: () => call('state.get', {}),
      set: value => call('state.set', { value }),
    },
    h, esc, fmt: { ago },
  };
  if (window.FRIDE_THEME) applyTheme(window.FRIDE_THEME.theme, window.FRIDE_THEME.css);
})();
