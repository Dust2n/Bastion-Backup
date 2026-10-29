// Bastion web UI – bootstrap, login, navigation and routing.
import { $, h, api, icon, state, formData, closeModal, modalOpen, connectEvents, disconnectEvents, clearPageListeners } from './lib.js';
import { pageDashboard, pageAgents, pageAgent, pageSnapshot } from './pages-machines.js';
import { pagePlans, pagePlanEdit, pageStorages, pageJobs, pageJob } from './pages-backup.js';
import { pageRollout, pageRescue, pageSettings } from './pages-admin.js';

const nav = [
  ['', 'home', 'Übersicht'], ['agents', 'monitor', 'Rechner'], ['plans', 'calendar', 'Backup-Pläne'],
  ['storages', 'db', 'Speicherorte'], ['jobs', 'list', 'Aufträge'], ['rollout', 'send', 'Agent-Verteilung'],
  ['rescue', 'life', 'Rettungssysteme'], ['settings', 'gear', 'Einstellungen'],
];

const routes = [
  [/^#?\/?$/, pageDashboard], [/^#\/agents$/, pageAgents], [/^#\/agents\/(\w+)$/, pageAgent],
  [/^#\/snapshots\/([\w-]+)$/, pageSnapshot], [/^#\/plans$/, pagePlans], [/^#\/plans\/(new|\d+)$/, pagePlanEdit],
  [/^#\/storages$/, pageStorages], [/^#\/jobs$/, pageJobs], [/^#\/jobs\/(\d+)$/, pageJob],
  [/^#\/rollout$/, pageRollout], [/^#\/rescue$/, pageRescue], [/^#\/settings$/, pageSettings],
];

// ---------- theme ----------
function applyTheme(t) {
  if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme;
  const dark = t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  $('#theme-btn').replaceChildren(icon(dark ? 'sun' : 'moon'));
}
$('#theme-btn').addEventListener('click', () => {
  const cur = document.documentElement.dataset.theme || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
  const next = cur === 'dark' ? 'light' : 'dark';
  try { localStorage.setItem('bastion-theme', next); } catch { /* private mode */ }
  applyTheme(next);
});
try { applyTheme(localStorage.getItem('bastion-theme')); } catch { applyTheme(null); }

$('#logout').append(icon('logout'));
$('#menu-btn').addEventListener('click', () => $('#sidebar').classList.toggle('open'));
$('#modal').addEventListener('click', (e) => { if (e.target.id === 'modal') closeModal(); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && modalOpen()) closeModal(); });

// ---------- login ----------
function showLogin() {
  disconnectEvents();
  $('#app').classList.add('hidden');
  $('#login').classList.remove('hidden');
  setTimeout(() => $('#login-form [name=name]').focus(), 50);
}
window.addEventListener('bastion-logout', showLogin);

$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const d = formData(e.target);
  try {
    await api('POST', '/api/login', { name: d.name, password: d.password });
    $('#login-error').textContent = '';
    e.target.reset();
    start();
  } catch (err) { $('#login-error').textContent = err.message; }
});
$('#logout').addEventListener('click', async () => { await api('POST', '/api/logout'); showLogin(); });

async function start() {
  try { state.me = await api('GET', '/api/me'); } catch { return; }
  $('#login').classList.add('hidden');
  $('#app').classList.remove('hidden');
  $('#nav').replaceChildren(...nav.map(([k, ic, t]) => h('a', { href: '#/' + k, 'data-nav': k }, icon(ic), h('span', {}, t))));
  $('#me').replaceChildren(h('b', {}, state.me.name), h('span', {}, ` · ${state.me.role} · v${state.me.version}`));
  connectEvents();
  route();
}

// ---------- routing ----------
let seq = 0;
async function route(keepScroll) {
  const hash = location.hash || '#/';
  const my = ++seq;
  clearPageListeners();
  $('#sidebar').classList.remove('open');
  let active = '';
  // a backup belongs to the "Rechner" section
  const section = hash.startsWith('#/snapshots') ? 'agents' : (hash.match(/^#\/(\w+)/) || [, ''])[1];
  for (const a of document.querySelectorAll('[data-nav]')) {
    const isActive = a.dataset.nav === section;
    a.classList.toggle('active', isActive);
    if (isActive) active = a.textContent;
  }
  $('#crumb').textContent = active || 'Übersicht';
  for (const [re, fn] of routes) {
    const m = hash.match(re);
    if (!m) continue;
    try {
      const el = await fn(...m.slice(1));
      if (my !== seq || !el) return;
      const y = window.scrollY;
      $('#main').replaceChildren(el);
      if (keepScroll) window.scrollTo(0, y); else window.scrollTo(0, 0);
    } catch (e) {
      if (my === seq) $('#main').replaceChildren(h('div', { class: 'callout bad' }, icon('alert'), h('span', {}, e.message)));
    }
    return;
  }
  $('#main').replaceChildren(h('div', { class: 'empty' }, 'Seite nicht gefunden'));
}
window.addEventListener('hashchange', () => route());
window.addEventListener('bastion-refresh', () => { if (!modalOpen()) route(true); });

start().catch(showLogin);
