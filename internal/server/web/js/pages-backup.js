// Backup-Pläne, Speicherorte und Aufträge.
import {
  h, api, icon, bytes, num, dt, dur, badge, jobText, osText, can, toast, act, progress, meter, table, modal, closeModal,
  confirmBox, formData, field, pageHead, card, onPage,
} from './lib.js';
import { jobTable, jobStatus } from './pages-machines.js';

// ---------- Pläne ----------
const presets = [
  ['0 22 * * *', 'Täglich 22:00'], ['30 12 * * 1-5', 'Werktags 12:30'], ['0 */4 * * *', 'Alle 4 Stunden'],
  ['0 * * * *', 'Stündlich'], ['0 2 * * 0', 'Wöchentlich So 02:00'], ['0 3 1 * *', 'Monatlich am 1. um 03:00'], ['', 'Nur manuell'],
];
const scheduleText = (c) => { const p = presets.find((x) => x[0] === c); return p ? p[1] : h('code', {}, c); };
const retention = (p) => [p.keep_last && `${p.keep_last} letzte`, p.keep_daily && `${p.keep_daily} tägl.`, p.keep_weekly && `${p.keep_weekly} wöch.`, p.keep_monthly && `${p.keep_monthly} monatl.`].filter(Boolean).join(', ') || 'alle behalten';

export async function pagePlans() {
  const [plans, storages] = await Promise.all([api('GET', '/api/plans'), api('GET', '/api/storages')]);
  const storeName = (id) => (storages.find((s) => s.id === id) || storages.find((s) => s.is_default) || {}).name || '–';
  return h('div', {},
    pageHead('Backup-Pläne', 'Was, wann, wohin und wie lange gesichert wird',
      can('admin') ? h('a', { href: '#/plans/new', class: 'btn primary' }, icon('plus'), 'Neuer Plan') : null),
    plans.length ? h('div', { class: 'grid cards' }, plans.map((x) => {
      const p = x.plan;
      return h('a', { class: 'plan-card' + (p.enabled ? '' : ' off'), href: '#/plans/' + p.id },
        h('div', { class: 'row-between' }, h('h3', {}, p.name), p.kind === 'image' ? badge('info', 'Image') : badge('', 'Dateien')),
        h('dl', { class: 'kv small' },
          h('dt', {}, 'Zeitplan'), h('dd', {}, scheduleText(p.schedule), x.next ? h('div', { class: 'muted' }, 'nächster Lauf ' + x.next) : null),
          h('dt', {}, 'Speicherort'), h('dd', {}, icon('db'), ' ', storeName(p.storage_id)),
          h('dt', {}, 'Aufbewahrung'), h('dd', {}, retention(p)),
          h('dt', {}, 'Rechner'), h('dd', {}, x.agents)),
        h('div', { class: 'row-between' }, p.enabled ? badge('success', 'aktiv') : badge('', 'pausiert'),
          can('operator') ? h('button', { class: 'btn small', onclick: (e) => { e.preventDefault(); act(() => api('POST', `/api/plans/${p.id}/run`), 'Backups eingeplant'); } }, icon('play'), 'Jetzt ausführen') : null));
    })) : card(null, h('div', { class: 'empty' }, 'Noch keine Pläne. ', can('admin') ? h('a', { href: '#/plans/new' }, 'Ersten Plan anlegen') : null)));
}

export async function pagePlanEdit(id) {
  const [agents, plans, storages] = await Promise.all([api('GET', '/api/agents'), id === 'new' ? [] : api('GET', '/api/plans'), api('GET', '/api/storages')]);
  const p = id === 'new'
    ? { name: '', kind: 'image', schedule: '0 22 * * *', paths: [], excludes: [], keep_last: 0, keep_daily: 14, keep_weekly: 8, keep_monthly: 12, bandwidth_kbps: 0, use_snapshot: true, enabled: true, targets: [], storage_id: 0 }
    : plans.find((x) => x.plan.id === Number(id))?.plan;
  if (!p) throw new Error('Plan nicht gefunden');
  const def = storages.find((s) => s.is_default);
  const curStorage = p.storage_id || def?.id;
  const groups = [...new Set(agents.map((a) => a.group).filter(Boolean))].sort();
  const targets = new Set(p.targets || []);
  const custom = !presets.some((x) => x[0] === p.schedule);
  const cronInput = h('input', { name: 'schedule', value: p.schedule, class: 'mono' });
  const cronWrap = field('Cron-Ausdruck (Minute Stunde Tag Monat Wochentag)', cronInput);
  if (!custom) cronWrap.classList.add('hidden');
  const pathsWrap = field('Zu sichernde Ordner (einer pro Zeile)', h('textarea', { name: 'paths', placeholder: 'C:\\Users\nD:\\Daten\n/home' }, (p.paths || []).join('\n')));
  if (p.kind !== 'files') pathsWrap.classList.add('hidden');
  const storeSel = h('select', { name: 'storage_id' }, storages.map((s) => h('option', { value: s.id, selected: s.id === curStorage, disabled: !s.online },
    `${s.name} – ${s.path}${s.online ? ` (${bytes(s.free)} frei)` : ' (nicht erreichbar)'}`)));

  const form = h('form', { onsubmit: (e) => e.preventDefault() },
    card('Allgemein',
      h('div', { class: 'form-grid' },
        field('Name', h('input', { name: 'name', value: p.name, required: true, placeholder: 'z. B. Arbeitsplätze täglich' })),
        field('Art', h('select', { name: 'kind', onchange: (e) => pathsWrap.classList.toggle('hidden', e.target.value !== 'files') },
          h('option', { value: 'image', selected: p.kind === 'image' }, 'Image – ganzer Rechner (bootfähig)'),
          h('option', { value: 'files', selected: p.kind === 'files' }, 'Dateien und Ordner'))),
        field('Zeitplan', h('select', { onchange: (e) => { if (e.target.value !== 'custom') cronInput.value = e.target.value; cronWrap.classList.toggle('hidden', e.target.value !== 'custom'); } },
          presets.map(([c, t]) => h('option', { value: c, selected: !custom && c === p.schedule }, t)), h('option', { value: 'custom', selected: custom }, 'Eigener Cron-Ausdruck'))),
        field('Speicherort', storeSel, h('span', {}, 'Wo die Backups liegen. ', h('a', { href: '#/storages' }, 'Speicherorte verwalten')))),
      cronWrap, pathsWrap,
      field('Ausschlüsse (einer pro Zeile, relativ, * erlaubt)', h('textarea', { name: 'excludes', placeholder: 'Users/*/AppData/Local/Microsoft/Windows/INetCache/*\n*.tmp' }, (p.excludes || []).join('\n')),
        'Auslagerungs-, Ruhezustands- und Temp-Dateien werden bei Image-Backups automatisch ausgelassen.')),
    card('Aufbewahrung',
      h('div', { class: 'form-grid four' },
        field('Letzte Backups', h('input', { type: 'number', min: 0, name: 'keep_last', value: p.keep_last })),
        field('Tägliche', h('input', { type: 'number', min: 0, name: 'keep_daily', value: p.keep_daily })),
        field('Wöchentliche', h('input', { type: 'number', min: 0, name: 'keep_weekly', value: p.keep_weekly })),
        field('Monatliche', h('input', { type: 'number', min: 0, name: 'keep_monthly', value: p.keep_monthly }))),
      h('small', { class: 'muted' }, 'Alle Werte 0 = nichts wird automatisch gelöscht.')),
    card('Optionen',
      h('div', { class: 'form-grid' }, field('Bandbreite pro Rechner (KB/s, 0 = unbegrenzt)', h('input', { type: 'number', min: 0, name: 'bandwidth_kbps', value: p.bandwidth_kbps }))),
      h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'use_snapshot', checked: p.use_snapshot }), 'Snapshot verwenden (VSS / LVM / btrfs) – Sicherung im laufenden Betrieb'),
      h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'enabled', checked: p.enabled }), 'Plan aktiv')),
    card('Rechner',
      h('div', { class: 'checklist' },
        h('label', { class: 'check' }, h('input', { type: 'checkbox', 'data-target': 'all', checked: targets.has('all') }), h('b', {}, 'Alle Rechner')),
        groups.map((g) => h('label', { class: 'check' }, h('input', { type: 'checkbox', 'data-target': 'group:' + g, checked: targets.has('group:' + g) }), 'Gruppe ', h('b', {}, g))),
        agents.map((a) => h('label', { class: 'check' }, h('input', { type: 'checkbox', 'data-target': 'agent:' + a.id, checked: targets.has('agent:' + a.id) }), a.hostname,
          h('span', { class: 'muted small' }, ' ' + osText(a.os) + (a.group ? ' · ' + a.group : '')))))),
    h('div', { class: 'actions sticky-actions' },
      can('admin') ? h('button', { class: 'btn primary', onclick: save }, icon('check'), 'Speichern') : null,
      h('a', { href: '#/plans', class: 'btn' }, 'Zurück'),
      can('admin') && id !== 'new' ? h('button', { type: 'button', class: 'btn danger-o', onclick: () => confirmBox('Plan löschen', 'Der Plan wird gelöscht. Vorhandene Backups bleiben erhalten.', 'Löschen', async () => { await api('DELETE', '/api/plans/' + id); location.hash = '#/plans'; }, true) }, icon('trash'), 'Löschen') : null));

  async function doSave(move) {
    const f = formData(form);
    const body = { ...f, storage_id: Number(f.storage_id) === def?.id && !p.storage_id ? 0 : Number(f.storage_id), schedule: cronInput.value.trim(), paths: f.paths.split('\n'), excludes: f.excludes.split('\n'), targets: [...form.querySelectorAll('[data-target]')].filter((c) => c.checked).map((c) => c.dataset.target) };
    try {
      const r = await api(id === 'new' ? 'POST' : 'PUT', id === 'new' ? '/api/plans' : `/api/plans/${id}${move ? '?move=1' : ''}`, body);
      if (r.move_error) toast('Verschieben nicht gestartet: ' + r.move_error, true);
      else if (r.move_job) { toast('Plan gespeichert, Backups werden verschoben'); location.hash = '#/jobs/' + r.move_job; return; }
      else toast('Plan gespeichert');
      location.hash = '#/plans';
    } catch (e) { toast(e.message, true); }
  }
  function save() {
    const changed = id !== 'new' && Number(storeSel.value) !== curStorage;
    if (!changed) return doSave(false);
    const to = storages.find((s) => s.id === Number(storeSel.value));
    modal('Speicherort ändern', h('div', {},
      h('p', {}, `Neue Backups dieses Plans werden ab jetzt in „${to?.name}“ gespeichert.`),
      h('p', {}, 'Sollen die vorhandenen Backups dorthin verschoben werden? Die Daten werden kopiert, im Ziel geprüft und erst dann umgestellt. Der alte Speicherplatz wird bei der nächsten Bereinigung frei.')),
    [h('button', { class: 'btn', onclick: () => { closeModal(); doSave(false); } }, 'Nur neue Backups'),
      h('button', { class: 'btn primary', onclick: () => { closeModal(); doSave(true); } }, icon('upload'), 'Vorhandene verschieben')]);
  }
  return h('div', {}, pageHead(id === 'new' ? 'Neuer Backup-Plan' : p.name, id === 'new' ? null : 'Plan bearbeiten'), form);
}

// ---------- Speicherorte ----------
export async function pageStorages() {
  const list = await api('GET', '/api/storages');
  return h('div', {},
    pageHead('Speicherorte', 'Wo die Backups liegen – je Backup-Plan wählbar',
      can('admin') ? h('button', { class: 'btn primary', onclick: storageDialog }, icon('plus'), 'Speicherort hinzufügen') : null),
    h('div', { class: 'grid cards' }, list.map((s) => h('section', { class: 'card store-card' + (s.online ? '' : ' offline') },
      h('div', { class: 'row-between' }, h('div', { class: 'cell-main' }, h('div', { class: 'kpi-ic' }, icon(s.kind === 'smb' ? 'link' : 'hdd')), h('div', {}, h('h3', {}, s.name), h('div', { class: 'small muted mono' }, s.path))),
        s.is_default ? badge('info', 'Standard') : null),
      s.online ? h('div', {},
        meter(s.total - s.free, s.total, `${bytes(s.free)} von ${bytes(s.total)} frei`),
        h('dl', { class: 'kv small' },
          h('dt', {}, 'Belegt durch Bastion'), h('dd', {}, bytes(s.used), s.logical ? h('span', { class: 'muted' }, ` (Inhalt ${bytes(s.logical)}${s.used ? ', Faktor ' + (s.logical / s.used).toFixed(1).replace('.', ',') : ''})`) : null),
          h('dt', {}, 'Backups'), h('dd', {}, num(s.snapshots)),
          h('dt', {}, 'Pläne'), h('dd', {}, num(s.plans))))
        : h('div', { class: 'callout bad' }, icon('alert'), s.error || 'nicht erreichbar'),
      can('admin') ? h('div', { class: 'actions' },
        !s.is_default && s.online ? h('button', { class: 'btn small', onclick: () => act(() => api('PUT', '/api/storages/' + s.id, { is_default: true }), 'Neuer Standard-Speicherort') }, 'Als Standard') : null,
        h('button', { class: 'btn small', onclick: () => renameDialog(s) }, 'Umbenennen'),
        !s.is_default ? h('button', { class: 'btn small danger-o', disabled: s.snapshots > 0 || s.plans > 0, title: s.snapshots || s.plans ? 'Erst Pläne umstellen und Backups verschieben' : '', onclick: () => confirmBox('Speicherort entfernen', `„${s.name}“ wird aus Bastion entfernt. Die Dateien im Ordner bleiben unangetastet.`, 'Entfernen', () => api('DELETE', '/api/storages/' + s.id), true) }, icon('trash')) : null) : null))),
    h('div', { class: 'callout' }, icon('shield'), h('div', {}, 'Alle Speicherorte sind verschlüsselt (AES-256). Gleiche Daten werden innerhalb eines Speicherorts nur einmal abgelegt. Um vorhandene Backups umzuziehen, im Backup-Plan einen anderen Speicherort wählen.')));
}

function renameDialog(s) {
  const inp = h('input', { value: s.name });
  modal('Speicherort umbenennen', field('Name', inp), h('button', { class: 'btn primary', onclick: () => { closeModal(); act(() => api('PUT', '/api/storages/' + s.id, { name: inp.value }), 'Gespeichert'); } }, 'Speichern'));
}

function storageDialog() {
  let kind = 'local';
  const pathIn = h('input', { name: 'path', placeholder: 'D:\\BastionBackups', class: 'mono' });
  const browser = h('div', { class: 'folder-browser' });
  const smbFields = h('div', { class: 'hidden' },
    h('div', { class: 'form-grid' }, field('Benutzer', h('input', { name: 'username', placeholder: 'NAS\\backup' })), field('Passwort', h('input', { name: 'password', type: 'password' }))));
  const browse = async (p) => {
    try {
      const r = await api('GET', '/api/storages/browse?path=' + encodeURIComponent(p || ''));
      browser.replaceChildren(
        h('div', { class: 'fb-head' }, r.path ? h('button', { class: 'btn small', onclick: () => browse(r.parent || '') }, '‹ zurück') : null, h('span', { class: 'mono small' }, r.path || 'Laufwerke'),
          r.path ? h('span', { class: 'small muted' }, ` · ${bytes(r.free)} frei`) : null),
        h('div', { class: 'fb-list' }, r.entries.length ? r.entries.map((e) => h('button', { type: 'button', class: 'fb-item', onclick: () => { pathIn.value = e.path; browse(e.path); } },
          icon(r.path ? 'folder' : 'hdd'), h('span', {}, e.name, e.label ? ` (${e.label})` : ''), e.total ? h('span', { class: 'small muted' }, `${bytes(e.free)} frei von ${bytes(e.total)}`) : null))
          : h('div', { class: 'muted small pad' }, 'Keine Unterordner. Ein neuer Ordnername kann im Pfad ergänzt werden – er wird angelegt.')));
    } catch (e) { browser.replaceChildren(h('div', { class: 'callout bad' }, e.message)); }
  };
  browse('');
  const kindBtn = (k, ic, t) => h('button', { type: 'button', class: 'choice' + (k === kind ? ' active' : ''), onclick: (e) => {
    kind = k; e.currentTarget.parentNode.querySelectorAll('.choice').forEach((c) => c.classList.remove('active')); e.currentTarget.classList.add('active');
    smbFields.classList.toggle('hidden', k !== 'smb'); browser.classList.toggle('hidden', k !== 'local'); pathIn.placeholder = k === 'smb' ? '\\\\nas\\backup\\bastion' : 'D:\\BastionBackups';
  } }, icon(ic), h('b', {}, t));
  const form = h('form', { onsubmit: (e) => e.preventDefault() },
    h('div', { class: 'choices' }, kindBtn('local', 'hdd', 'Laufwerk / Ordner'), kindBtn('smb', 'link', 'Netzwerkfreigabe (SMB/NAS)')),
    field('Name', h('input', { name: 'name', placeholder: 'z. B. Backup-Platte D:' })),
    field('Pfad', pathIn), browser, smbFields);
  modal('Speicherort hinzufügen', form, h('button', { class: 'btn primary', onclick: async () => {
    const f = formData(form);
    if (!f.name) f.name = f.path;
    try { await api('POST', '/api/storages', { ...f, kind }); closeModal(); toast('Speicherort angelegt'); window.dispatchEvent(new Event('bastion-refresh')); } catch (e) { toast(e.message, true); }
  } }, icon('check'), 'Anlegen'), true);
}

// ---------- Aufträge ----------
let jobFilter = 'active';
export async function pageJobs() {
  const jobs = await api('GET', '/api/jobs?limit=300' + (jobFilter ? '&status=' + jobFilter : ''));
  const tab = (v, t) => h('button', { class: v === jobFilter ? 'active' : '', onclick: () => { jobFilter = v; window.dispatchEvent(new Event('bastion-refresh')); } }, t);
  const matches = (j) => !jobFilter || (jobFilter === 'active' ? ['queued', 'running', 'waiting'].includes(j.status) || jobs.some((x) => x.id === j.id) : j.status === jobFilter);
  return h('div', {},
    pageHead('Aufträge', 'Backups, Wiederherstellungen und Wartung – live aktualisiert'),
    card(null, h('div', { class: 'tabs' }, tab('active', 'Aktiv'), tab('', 'Alle'), tab('failed', 'Fehlgeschlagen'), tab('success', 'Erfolgreich')),
      jobTable(jobs, { live: true, filter: matches, empty: jobFilter === 'active' ? 'Gerade läuft nichts' : 'Keine Aufträge' })));
}

export async function pageJob(id) {
  let j = await api('GET', '/api/jobs/' + id);
  const head = h('div');
  const logBox = h('pre', { class: 'log' });
  const render = () => {
    const active = ['queued', 'running', 'waiting'].includes(j.status);
    head.replaceChildren(
      pageHead(`${jobText[j.type] || j.type} #${j.id}`, h('span', {}, j.agent_id ? h('a', { href: '#/agents/' + j.agent_id }, j.hostname) : 'Server', j.plan_name ? ` · Plan ${j.plan_name}` : '', ` · gestartet von ${j.created_by || '–'}`),
        can('operator') && active ? h('button', { class: 'btn danger-o', onclick: () => act(() => api('POST', `/api/jobs/${id}/cancel`), 'Abbruch angefordert', () => {}) }, icon('x'), 'Abbrechen') : null),
      card(null,
        h('div', { class: 'stats-row' },
          h('div', {}, h('div', { class: 'kpi-l' }, 'Status'), badge(j.status)),
          h('div', {}, h('div', { class: 'kpi-l' }, 'Erstellt'), dt(j.created)),
          h('div', {}, h('div', { class: 'kpi-l' }, 'Dauer'), j.started ? dur((j.finished || Date.now() / 1000) - j.started) : '–'),
          h('div', {}, h('div', { class: 'kpi-l' }, 'Daten'), `${num(j.files)} / ${bytes(j.bytes)}`),
          j.speed ? h('div', {}, h('div', { class: 'kpi-l' }, 'Tempo'), bytes(j.speed) + '/s') : null,
          j.eta ? h('div', {}, h('div', { class: 'kpi-l' }, 'Restzeit'), dur(j.eta)) : null,
          j.snapshot_id ? h('div', {}, h('div', { class: 'kpi-l' }, 'Backup'), h('a', { href: '#/snapshots/' + j.snapshot_id }, 'öffnen')) : null),
        active ? h('div', { class: 'big-progress' }, progress(j.percent), h('div', { class: 'row-between small muted' }, h('span', {}, j.message), h('b', {}, Math.round(j.percent) + ' %'))) : h('p', { class: 'msg' }, j.message),
        j.type === 'restore_image' && j.status === 'waiting' && (j.message || '').includes('Code') ? h('div', { class: 'callout' }, icon('life'), 'Rechner vom Rettungsmedium starten und den Code eingeben – oder im Rettungssystem anmelden und das Backup auswählen.') : null));
    logBox.textContent = j.log || 'Noch keine Einträge';
  };
  render();
  let pending = false;
  onPage('job', async (x) => {
    if (x.id !== j.id || pending) return;
    pending = true;
    try { j = await api('GET', '/api/jobs/' + id); render(); logBox.scrollTop = logBox.scrollHeight; } finally { pending = false; }
  });
  return h('div', {}, head, card('Protokoll', logBox));
}
