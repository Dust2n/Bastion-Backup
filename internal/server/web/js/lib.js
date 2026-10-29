// Bastion web UI – shared helpers, icons, charts and live events.
// DOM is built with h() so data is never interpreted as HTML.

export const $ = (s, root = document) => root.querySelector(s);
export const state = { me: null };

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'style') el.style.cssText = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

// ---------- icons (static markup, never user data) ----------
const P = {
  home: '<path d="M3 11l9-8 9 8"/><path d="M5 10v10h14V10"/>',
  monitor: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>',
  calendar: '<rect x="3" y="5" width="18" height="16" rx="2"/><path d="M3 10h18M8 3v4M16 3v4"/>',
  list: '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  db: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v6c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 11v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/>',
  send: '<path d="M22 2L11 13M22 2l-7 20-4-9-9-4z"/>',
  life: '<circle cx="12" cy="12" r="9"/><circle cx="12" cy="12" r="4"/><path d="M5.6 5.6l3.6 3.6M14.8 14.8l3.6 3.6M18.4 5.6l-3.6 3.6M9.2 14.8l-3.6 3.6"/>',
  gear: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 00.3 1.8l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-1.8-.3 1.7 1.7 0 00-1 1.5V21a2 2 0 11-4 0v-.1a1.7 1.7 0 00-1.1-1.5 1.7 1.7 0 00-1.8.3l-.1.1a2 2 0 11-2.8-2.8l.1-.1a1.7 1.7 0 00.3-1.8 1.7 1.7 0 00-1.5-1H3a2 2 0 110-4h.1a1.7 1.7 0 001.5-1.1 1.7 1.7 0 00-.3-1.8l-.1-.1a2 2 0 112.8-2.8l.1.1a1.7 1.7 0 001.8.3h.1a1.7 1.7 0 001-1.5V3a2 2 0 114 0v.1a1.7 1.7 0 001 1.5 1.7 1.7 0 001.8-.3l.1-.1a2 2 0 112.8 2.8l-.1.1a1.7 1.7 0 00-.3 1.8v.1a1.7 1.7 0 001.5 1H21a2 2 0 110 4h-.1a1.7 1.7 0 00-1.5 1z"/>',
  logout: '<path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4M16 17l5-5-5-5M21 12H9"/>',
  play: '<path d="M6 4l14 8-14 8z"/>',
  download: '<path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3"/>',
  upload: '<path d="M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M17 8l-5-5-5 5M12 3v12"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M21 21l-4.3-4.3"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
  moon: '<path d="M21 12.8A9 9 0 1111.2 3a7 7 0 009.8 9.8z"/>',
  check: '<path d="M20 6L9 17l-5-5"/>',
  x: '<path d="M18 6L6 18M6 6l12 12"/>',
  alert: '<path d="M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0zM12 9v4M12 17h.01"/>',
  folder: '<path d="M22 19a2 2 0 01-2 2H4a2 2 0 01-2-2V5a2 2 0 012-2h5l2 3h9a2 2 0 012 2z"/>',
  file: '<path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><path d="M14 2v6h6"/>',
  link: '<path d="M10 13a5 5 0 007.5.5l3-3a5 5 0 00-7-7l-1.7 1.7M14 11a5 5 0 00-7.5-.5l-3 3a5 5 0 007 7l1.7-1.7"/>',
  hdd: '<path d="M22 12H2M5.5 5.1L2 12v6a2 2 0 002 2h16a2 2 0 002-2v-6l-3.5-6.9A2 2 0 0016.8 4H7.2a2 2 0 00-1.7 1.1zM6 16h.01M10 16h.01"/>',
  refresh: '<path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.5 9a9 9 0 0114.8-3.4L23 10M1 14l4.7 4.4A9 9 0 0020.5 15"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  shield: '<path d="M12 2L4 6v6c0 5 3.4 9.4 8 10 4.6-.6 8-5 8-10V6z"/><path d="M8.5 12l2.5 2.5 4.5-5"/>',
  restore: '<path d="M3 12a9 9 0 109-9 9.7 9.7 0 00-6.7 2.8L3 8"/><path d="M3 3v5h5M12 7v5l3 3"/>',
  trash: '<path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6"/>',
  pin: '<path d="M12 17v5M5 17h14l-2-4V4H7v9z"/>',
};
export function icon(name, cls) {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('class', 'ic' + (cls ? ' ' + cls : ''));
  s.innerHTML = P[name] || '';
  return s;
}

// ---------- API ----------
export async function api(method, path, body) {
  const opt = { method, headers: { 'X-Bastion': '1' } };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const r = await fetch(path, opt);
  if (r.status === 401 && !path.startsWith('/api/login')) { window.dispatchEvent(new Event('bastion-logout')); throw new Error('nicht angemeldet'); }
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
  const ct = r.headers.get('Content-Type') || '';
  return ct.includes('json') ? r.json() : r.text();
}

// ---------- formatting ----------
const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
export function bytes(n) {
  if (!n) return '0 B';
  let i = 0; while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)).toString().replace('.', ',') + ' ' + units[i];
}
export const num = (n) => (n || 0).toLocaleString('de-DE');
export function dt(ts) { return ts ? new Date(ts * 1000).toLocaleString('de-DE', { dateStyle: 'short', timeStyle: 'short' }) : '–'; }
export function ago(ts) {
  if (!ts) return 'nie';
  const s = Date.now() / 1000 - ts;
  if (s < 90) return 'gerade eben';
  if (s < 3600) return `vor ${Math.round(s / 60)} Min.`;
  if (s < 86400) return `vor ${Math.round(s / 3600)} Std.`;
  return `vor ${Math.round(s / 86400)} Tagen`;
}
export function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return `${sec} s`;
  if (sec < 3600) return `${Math.floor(sec / 60)} min ${sec % 60 ? (sec % 60) + ' s' : ''}`.trim();
  return `${Math.floor(sec / 3600)} h ${Math.round((sec % 3600) / 60)} min`;
}
export const statusText = { queued: 'geplant', running: 'läuft', waiting: 'wartet', success: 'erfolgreich', failed: 'fehlgeschlagen', canceled: 'abgebrochen', done: 'fertig' };
const statusClass = { success: 'ok', done: 'ok', failed: 'bad', canceled: 'warn', running: 'info', waiting: 'info', queued: '' };
export function badge(st, text) { return h('span', { class: 'badge ' + (statusClass[st] ?? st ?? '') }, text || statusText[st] || st || '–'); }
export const jobText = { backup: 'Backup', restore_files: 'Datei-Wiederherstellung', restore_image: 'Image-Wiederherstellung', update: 'Agent-Update', uninstall: 'Deinstallation', storage_move: 'Backups verschieben', verify: 'Restore-Prüfung' };
export const osText = (os) => os === 'windows' ? 'Windows' : os === 'linux' ? 'Linux' : (os || '?');
const ranks = { viewer: 1, operator: 2, admin: 3 };
export const can = (role) => (ranks[state.me?.role] || 0) >= ranks[role];

// ---------- components ----------
export function toast(msg, bad) {
  const t = $('#toast');
  t.replaceChildren(icon(bad ? 'alert' : 'check'), h('span', {}, msg));
  t.className = 'toast show' + (bad ? ' bad' : '');
  clearTimeout(t._t);
  t._t = setTimeout(() => t.classList.remove('show'), bad ? 7000 : 3500);
}

export async function act(fn, okMsg, after) {
  try { const r = await fn(); if (okMsg) toast(okMsg); if (after) after(r); else window.dispatchEvent(new Event('bastion-refresh')); return r; }
  catch (e) { toast(e.message, true); }
}

export function progress(p, cls) {
  return h('div', { class: 'progress ' + (cls || ''), title: Math.round(p || 0) + ' %' }, h('div', { style: `width:${Math.max(1.5, Math.min(100, p || 0))}%` }));
}

export function meter(used, total, label) {
  const p = total ? used / total * 100 : 0;
  return h('div', { class: 'meter' }, progress(p, p > 90 ? 'bad' : p > 75 ? 'warn' : ''), label ? h('div', { class: 'small muted' }, label) : null);
}

// table: cols = [[title, render, cls?]], rows; onclick(row); key(row) for live updates
export function table(cols, rows, opts = {}) {
  if (!rows.length) return h('div', { class: 'empty' }, opts.empty || 'Keine Einträge');
  const tr = (r) => h('tr', { class: opts.onclick ? 'click' : '', 'data-key': opts.key ? opts.key(r) : null, onclick: opts.onclick ? () => opts.onclick(r) : null },
    cols.map((c) => h('td', { class: c[2] || '' }, c[1](r))));
  const tbody = h('tbody', {}, rows.map(tr));
  const el = h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, cols.map((c) => h('th', { class: c[2] || '' }, c[0])))), tbody));
  el.update = (r) => {
    const old = tbody.querySelector(`tr[data-key="${CSS.escape(String(opts.key(r)))}"]`);
    if (old) old.replaceWith(tr(r)); else if (opts.prepend) tbody.prepend(tr(r));
  };
  return el;
}

export function copyBox(text) {
  return h('div', { class: 'copy' }, h('code', {}, text),
    h('button', { class: 'btn small', onclick: () => { navigator.clipboard.writeText(text); toast('Kopiert'); } }, 'Kopieren'));
}

export function modal(title, body, buttons, wide) {
  const box = $('#modal-box');
  box.className = 'modal-box' + (wide ? ' wide' : '');
  box.replaceChildren(h('div', { class: 'modal-head' }, h('h2', {}, title), h('button', { class: 'icon-btn', onclick: closeModal, title: 'Schließen' }, icon('x'))),
    h('div', { class: 'modal-body' }, body), h('div', { class: 'modal-actions' }, h('button', { class: 'btn', onclick: closeModal }, 'Abbrechen'), buttons || []));
  $('#modal').classList.add('open');
  setTimeout(() => box.querySelector('input,select,textarea')?.focus(), 50);
}
export function closeModal() { $('#modal').classList.remove('open'); }
export const modalOpen = () => $('#modal').classList.contains('open');

export function confirmBox(title, text, okText, fn, danger) {
  modal(title, h('p', {}, text), h('button', { class: 'btn ' + (danger ? 'danger' : 'primary'), onclick: async () => { closeModal(); await act(fn); } }, okText));
}

export function formData(form) {
  const o = {};
  for (const el of form.querySelectorAll('[name]')) {
    if (el.type === 'checkbox' || el.type === 'radio') o[el.name] = el.checked;
    else if (el.type === 'number') o[el.name] = el.value === '' ? 0 : Number(el.value);
    else o[el.name] = el.value;
  }
  return o;
}

export function field(label, input, hint) { return h('label', { class: 'field' }, h('span', {}, label), input, hint ? h('small', {}, hint) : null); }

export function pageHead(title, sub, ...actions) {
  return h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, title), sub ? h('div', { class: 'muted' }, sub) : null), h('div', { class: 'actions' }, actions));
}

export function card(title, ...body) {
  return h('section', { class: 'card' }, title ? h('div', { class: 'card-head' }, typeof title === 'string' ? h('h2', {}, title) : title) : null, body);
}

export function kpi(ic, label, value, sub, tone) {
  return h('div', { class: 'kpi ' + (tone || '') }, h('div', { class: 'kpi-ic' }, icon(ic)),
    h('div', {}, h('div', { class: 'kpi-l' }, label), h('div', { class: 'kpi-v' }, value), sub ? h('div', { class: 'kpi-s' }, sub) : null));
}

// ---------- charts (inline SVG) ----------
function svg(tag, attrs, ...kids) {
  const e = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
  for (const k of kids.flat()) if (k) e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  return e;
}

// stacked bar chart: series = [{key, cls, label}], data = [{label, [key]: n}]
export function barChart(data, series, height = 160) {
  const W = 640, H = height, pad = 22, bw = (W - pad) / data.length;
  const max = Math.max(1, ...data.map((d) => series.reduce((s, x) => s + (d[x.key] || 0), 0)));
  const g = svg('svg', { viewBox: `0 0 ${W} ${H + 22}`, class: 'chart', preserveAspectRatio: 'none' });
  for (let i = 0; i <= 2; i++) {
    const y = H - (H - 10) * i / 2;
    g.append(svg('line', { x1: pad, x2: W, y1: y, y2: y, class: 'gl' }), svg('text', { x: 0, y: y + 4, class: 'axis' }, Math.round(max * i / 2)));
  }
  data.forEach((d, i) => {
    let y = H;
    const x = pad + i * bw + bw * 0.18;
    for (const s of series) {
      const v = d[s.key] || 0;
      if (!v) continue;
      const hgt = (H - 10) * v / max;
      y -= hgt;
      const r = svg('rect', { x, y, width: bw * 0.64, height: hgt, rx: 3, class: 'bar ' + s.cls });
      r.append(svg('title', {}, `${d.label}: ${v} ${s.label}`));
      g.append(r);
    }
    if (data.length <= 16) g.append(svg('text', { x: x + bw * 0.32, y: H + 16, class: 'axis', 'text-anchor': 'middle' }, d.short || d.label));
  });
  return h('div', { class: 'chart-wrap' }, g,
    h('div', { class: 'legend' }, series.map((s) => h('span', {}, h('i', { class: 'dot-l ' + s.cls }), s.label))));
}

export function donut(parts, center) {
  const total = parts.reduce((s, p) => s + p.value, 0) || 1;
  const g = svg('svg', { viewBox: '0 0 42 42', class: 'donut' });
  g.append(svg('circle', { cx: 21, cy: 21, r: 15.9, class: 'donut-bg' }));
  let off = 25;
  for (const p of parts) {
    const pct = p.value / total * 100;
    g.append(svg('circle', { cx: 21, cy: 21, r: 15.9, class: 'donut-seg ' + p.cls, 'stroke-dasharray': `${pct} ${100 - pct}`, 'stroke-dashoffset': off }));
    off -= pct;
  }
  if (center) g.append(svg('text', { x: 21, y: 23, 'text-anchor': 'middle', class: 'donut-t' }, center));
  return g;
}

// ---------- live events (SSE) ----------
const listeners = new Map();
let es = null;
export function on(type, fn) {
  if (!listeners.has(type)) listeners.set(type, new Set());
  listeners.get(type).add(fn);
  return () => listeners.get(type)?.delete(fn);
}
export function clearPageListeners() { for (const s of listeners.values()) for (const f of [...s]) if (f.page) s.delete(f); }
export function onPage(type, fn) { fn.page = true; return on(type, fn); }
export function connectEvents() {
  if (es) return;
  es = new EventSource('/api/events');
  es.onopen = () => document.body.classList.add('live');
  es.onerror = () => document.body.classList.remove('live');
  es.onmessage = (m) => {
    try {
      const ev = JSON.parse(m.data);
      for (const fn of listeners.get(ev.type) || []) fn(ev.data);
      for (const fn of listeners.get('*') || []) fn(ev);
    } catch { /* ignore */ }
  };
}
export function disconnectEvents() { if (es) { es.close(); es = null; } }
