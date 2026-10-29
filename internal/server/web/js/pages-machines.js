// Übersicht, Rechner, Rechner-Detail, Backup-Browser und Wiederherstellung.
import {
  h, api, icon, bytes, num, dt, ago, dur, badge, jobText, osText, can, toast, act, progress, meter, table, modal, closeModal,
  confirmBox, formData, field, pageHead, card, kpi, barChart, donut, onPage,
} from './lib.js';

// ---------- jobs (shared) ----------
export function jobStatus(j) {
  if (j.status !== 'running') return badge(j.status);
  const extra = [Math.round(j.percent) + ' %'];
  if (j.speed) extra.push(bytes(j.speed) + '/s');
  if (j.eta) extra.push('noch ' + dur(j.eta));
  return h('div', { class: 'job-live' }, h('div', { class: 'row-between' }, badge('running'), h('span', { class: 'small muted' }, extra.join(' · '))), progress(j.percent));
}

export function jobTable(jobs, opts = {}) {
  const cols = [
    ['Auftrag', (j) => h('div', {}, h('a', { href: '#/jobs/' + j.id }, jobText[j.type] || j.type), j.plan_name ? h('div', { class: 'small muted' }, j.plan_name) : null)],
    ['Rechner', (j) => j.agent_id ? h('a', { href: '#/agents/' + j.agent_id, onclick: (e) => e.stopPropagation() }, j.hostname) : h('span', { class: 'muted' }, j.hostname)],
    ['Status', jobStatus, 'w-status'],
    ['Zeit', (j) => h('span', { class: 'small', title: 'erstellt ' + dt(j.created) }, j.finished ? dt(j.finished) : j.started ? 'seit ' + dur(Date.now() / 1000 - j.started) : dt(j.created))],
  ];
  if (!opts.compact) cols.push(['Meldung', (j) => h('span', { class: 'small msg' }, j.message)]);
  const t = table(cols, jobs || [], { onclick: (j) => location.hash = '#/jobs/' + j.id, key: (j) => j.id, prepend: opts.live, empty: opts.empty });
  if (opts.live) onPage('job', (j) => { if (!opts.filter || opts.filter(j)) t.update?.(j); });
  return t;
}

// ---------- Übersicht ----------
export async function pageDashboard() {
  const [d, stats, storages, active] = await Promise.all([api('GET', '/api/dashboard'), api('GET', '/api/stats'),
    api('GET', '/api/storages'), api('GET', '/api/jobs?status=active&limit=20')]);
  const ratio = d.repo && d.repo.bytes ? d.logical_bytes / d.repo.bytes : 0;
  const days = stats.days.map((x) => ({ label: new Date(x.day).toLocaleDateString('de-DE', { weekday: 'short', day: '2-digit', month: '2-digit' }), short: x.day.slice(8) + '.', ok: x.ok, failed: x.failed }));
  const activeBox = h('div');
  const renderActive = (list) => activeBox.replaceChildren(list.length ? jobTable(list, { compact: true }) : h('div', { class: 'empty' }, icon('check'), ' Gerade läuft nichts.'));
  let current = active;
  renderActive(current);
  onPage('job', (j) => {
    const i = current.findIndex((x) => x.id === j.id);
    const isActive = ['queued', 'running', 'waiting'].includes(j.status);
    if (i >= 0) { if (isActive) current[i] = j; else current.splice(i, 1); } else if (isActive) current.unshift(j);
    renderActive(current);
  });

  return h('div', {},
    pageHead('Übersicht', 'Zustand aller gesicherten Rechner'),
    h('div', { class: 'grid kpis' },
      kpi('monitor', 'Rechner online', `${d.online} / ${d.agents}`, d.agents - d.online ? `${d.agents - d.online} offline` : 'alle erreichbar', d.online < d.agents ? 'warn' : 'ok'),
      kpi('check', 'Backups (24 h)', h('span', {}, h('span', { class: 'ok-t' }, d.ok24), ' / ', h('span', { class: d.failed24 ? 'bad-t' : '' }, d.failed24)), 'erfolgreich / fehlgeschlagen', d.failed24 ? 'bad' : 'ok'),
      kpi('play', 'Aktive Aufträge', d.running, d.queued ? `${d.queued} geplant` : 'keine geplant'),
      kpi('db', 'Speicher belegt', d.repo && d.repo.bytes ? bytes(d.repo.bytes) : '–', ratio ? `Deduplizierung ${ratio.toFixed(1).replace('.', ',')} : 1 · ${d.snapshots} Backups` : `${d.snapshots} Backups`)),
    d.scrub && d.scrub.bad > 0 ? h('div', { class: 'callout bad' }, icon('alert'), `Integritätsprüfung: ${d.scrub.message}`) : null,
    h('div', { class: 'grid two-one' },
      card('Backups der letzten 14 Tage', barChart(days, [{ key: 'ok', cls: 'ok', label: 'erfolgreich' }, { key: 'failed', cls: 'bad', label: 'fehlgeschlagen' }])),
      card(h('div', { class: 'row-between' }, h('h2', {}, 'Speicherorte'), h('a', { href: '#/storages', class: 'small' }, 'verwalten')),
        storages.map((s) => h('div', { class: 'store-mini' },
          h('div', { class: 'row-between' }, h('b', {}, s.name), h('span', { class: 'small muted' }, s.online ? `${bytes(s.free)} frei` : 'nicht erreichbar')),
          meter(s.total - s.free, s.total, `${bytes(s.used)} Bastion · ${s.snapshots} Backups · ${s.path}`))))),
    h('div', { class: 'grid two' },
      card('Aktive Aufträge', activeBox),
      card('Handlungsbedarf', (d.problems || []).length ? table([
        ['Rechner', (r) => h('a', { href: '#/agents/' + r.id }, r.hostname)],
        ['Letztes Backup', (r) => r.last_backup ? ago(r.last_backup) : badge('failed', 'noch nie')],
        ['Online', (r) => ago(r.last_seen)],
      ], d.problems) : h('div', { class: 'empty' }, icon('check'), ` Alle Rechner wurden in den letzten ${d.stale_days} Tagen gesichert.`))),
    card('Letzte Aufträge', jobTable(d.recent, { compact: false, live: true })));
}

// ---------- Rechner ----------
const agentFilter = { q: '', group: '', status: '' };
export async function pageAgents() {
  const [agents, plans] = await Promise.all([api('GET', '/api/agents'), api('GET', '/api/plans')]);
  const groups = [...new Set(agents.map((a) => a.group).filter(Boolean))].sort();
  const list = h('div');
  const render = () => {
    const q = agentFilter.q.toLowerCase();
    const rows = agents.filter((a) => (!q || a.hostname.toLowerCase().includes(q) || (a.ips || []).join(' ').includes(q))
      && (!agentFilter.group || a.group === agentFilter.group)
      && (agentFilter.status === 'removed' ? a.removed : !a.removed)
      && (!agentFilter.status || (agentFilter.status === 'offline' ? !a.online : agentFilter.status === 'problem' ? a.last_status === 'failed' || !a.last_backup : true)));
    list.replaceChildren(table([
      ['Rechner', (a) => h('div', { class: 'cell-main' }, h('span', { class: 'dot' + (a.online ? ' on' : a.removed ? ' removed' : ''), title: a.removed ? 'Agent deinstalliert ' + ago(a.removed) : a.online ? 'online' : 'offline, zuletzt ' + ago(a.last_seen) }),
        h('div', {}, h('a', { href: '#/agents/' + a.id }, a.hostname), a.removed ? h('span', { class: 'badge warn sp' }, 'deinstalliert') : null, h('div', { class: 'small muted' }, (a.ips || [])[0] || '')))],
      ['System', (a) => h('span', { class: 'small' }, a.os_version || osText(a.os))],
      ['Gruppe', (a) => a.group ? h('span', { class: 'tag' }, a.group) : h('span', { class: 'muted' }, '–')],
      ['Letztes Backup', (a) => h('div', {}, a.last_backup ? ago(a.last_backup) : 'noch nie', a.last_status ? h('div', {}, badge(a.last_status)) : null)],
      ['Pläne', (a) => a.removed ? h('span', { class: 'muted' }, '–') : (a.plans || []).length ? h('span', { class: 'small' }, a.plans.join(', ')) : badge('canceled', 'kein Plan')],
      ['Agent', (a) => h('span', { class: 'small' }, a.version, a.update_available && !a.removed ? h('span', { class: 'badge info sp' }, 'Update') : null)],
      ['', (a) => can('operator') && !a.removed ? h('button', { class: 'btn small', onclick: (e) => { e.stopPropagation(); backupNow(a, plans); } }, icon('play'), 'Sichern') : '', 'right'],
    ], rows, { onclick: (a) => location.hash = '#/agents/' + a.id, empty: 'Keine Rechner gefunden' }));
  };
  render();
  onPage('agent', (ev) => { const a = agents.find((x) => x.id === ev.id); if (a) { a.online = ev.online; if (ev.removed) a.removed = Date.now() / 1000; render(); } });
  const active = agents.filter((a) => !a.removed);
  const chip = (v, t) => h('button', { class: 'chip' + (agentFilter.status === v ? ' active' : ''), onclick: (e) => { agentFilter.status = v; e.target.parentNode.querySelectorAll('.chip').forEach((c) => c.classList.remove('active')); e.target.classList.add('active'); render(); } }, t);
  return h('div', {},
    pageHead('Rechner', `${active.length} Rechner mit Agent · ${active.filter((a) => a.online).length} online`,
      can('admin') && active.some((a) => a.update_available) ? h('button', { class: 'btn', onclick: () => confirmBox('Agents aktualisieren', 'Alle Agents mit älterer Version erhalten ein Update und starten neu.', 'Aktualisieren', () => api('POST', '/api/agents/all/update' + (agentFilter.group ? '?group=' + encodeURIComponent(agentFilter.group) : ''))) }, icon('refresh'), 'Alle aktualisieren') : null,
      h('a', { href: '#/rollout', class: 'btn primary' }, icon('plus'), 'Agent verteilen')),
    card(null,
      h('div', { class: 'toolbar' },
        h('div', { class: 'search' }, icon('search'), h('input', { placeholder: 'Name oder IP suchen …', value: agentFilter.q, oninput: (e) => { agentFilter.q = e.target.value; render(); } })),
        h('div', { class: 'chips' }, chip('', 'Alle'), chip('offline', 'Offline'), chip('problem', 'Ohne/fehlgeschlagenes Backup'),
          agents.some((a) => a.removed) ? chip('removed', 'Deinstalliert') : null),
        groups.length ? h('select', { onchange: (e) => { agentFilter.group = e.target.value; render(); } },
          h('option', { value: '' }, 'Alle Gruppen'), groups.map((g) => h('option', { value: g, selected: g === agentFilter.group }, g))) : null),
      list));
}

export function backupNow(a, plans) {
  const mine = plans.filter((p) => (a.plans || []).includes(p.plan.name));
  const sel = h('select', { name: 'plan' }, mine.map((p) => h('option', { value: p.plan.id }, p.plan.name)), h('option', { value: 0 }, 'Einmaliges Image-Backup (Standard-Speicherort)'));
  modal(`Backup von ${a.hostname}`, h('div', {}, field('Plan', sel),
    !a.online ? h('div', { class: 'callout warn' }, icon('alert'), 'Der Rechner ist offline. Das Backup startet, sobald er erreichbar ist (Wake-on-LAN wird versucht).') : null),
    h('button', { class: 'btn primary', onclick: () => { closeModal(); act(() => api('POST', `/api/agents/${a.id}/backup`, { plan_id: Number(sel.value) }), 'Backup eingeplant'); } }, icon('play'), 'Starten'));
}

function diskBars(lay) {
  if (!lay || !lay.disks) return h('div', { class: 'muted' }, 'Noch kein Datenträger-Layout gemeldet.');
  return h('div', {}, lay.disks.map((d) => h('div', { class: 'disk-row' },
    h('div', { class: 'row-between' }, h('div', {}, icon('hdd'), h('b', {}, ` Datenträger ${d.number}`), h('span', { class: 'muted' }, ` ${d.model || ''}`)),
      h('span', { class: 'small muted' }, `${bytes(d.size)} · ${(d.style || '').toUpperCase()}`, d.system ? h('span', { class: 'badge info sp' }, 'System') : null)),
    h('div', { class: 'disk' }, (d.partitions || []).map((p) => h('div', {
      class: 'p-' + p.role, style: `flex:${Math.max(p.size / d.size * 100, 1.4)}`,
      title: `${p.mount || p.label || p.role} · ${p.fs || ''} · ${bytes(p.size)}${p.used ? ' · belegt ' + bytes(p.used) : ''}${p.volume ? ' · wird gesichert' : ''}`,
    }, h('span', {}, p.mount || (p.role === 'lvm' ? 'LVM' : p.role)), h('span', { class: 'o' }, bytes(p.size)),
      p.used ? h('div', { class: 'used', style: `width:${Math.min(100, p.used / p.size * 100)}%` }) : null))))),
  h('div', { class: 'small muted' }, `Firmware: ${(lay.firmware || '').toUpperCase()} · helle Linie = belegter Anteil`));
}

function timeline(snaps) {
  if (!snaps.length) return h('div', { class: 'empty' }, 'Noch keine Backups');
  const now = Date.now() / 1000, span = 30 * 86400;
  return h('div', { class: 'timeline' },
    h('div', { class: 'tl-bar' }, snaps.filter((s) => now - s.time < span).map((s) => h('a', {
      href: '#/snapshots/' + s.id, class: 'tl-dot ' + (s.kind === 'image' ? 'img' : 'files'),
      style: `left:${100 - (now - s.time) / span * 100}%`, title: `${dt(s.time)} · ${s.kind === 'image' ? 'Image' : 'Dateien'} · ${bytes(s.bytes)}`,
    }))),
    h('div', { class: 'tl-axis small muted' }, h('span', {}, 'vor 30 Tagen'), h('span', {}, 'vor 15 Tagen'), h('span', {}, 'heute')));
}

export async function pageAgent(id) {
  const [d, plans] = await Promise.all([api('GET', '/api/agents/' + id), api('GET', '/api/plans')]);
  const a = d.agent;
  const images = d.snapshots.filter((s) => s.kind === 'image');
  return h('div', {},
    pageHead(h('span', { class: 'cell-main' }, h('span', { class: 'dot big' + (a.online ? ' on' : a.removed ? ' removed' : '') }), a.hostname),
      `${a.os_version || osText(a.os)} · ${(a.ips || []).join(', ')} · Agent ${a.version} · ${a.removed ? 'deinstalliert ' + ago(a.removed) : a.online ? 'online' : 'offline, zuletzt ' + ago(a.last_seen)}`,
      can('operator') && !a.removed ? h('button', { class: 'btn primary', onclick: () => backupNow(a, plans) }, icon('play'), 'Jetzt sichern') : null,
      can('admin') && images.length ? h('button', { class: 'btn', onclick: () => restoreImageDialog(images[0], a) }, icon('restore'), 'Image wiederherstellen') : null,
      h('div', { class: 'menu' }, h('button', { class: 'btn', onclick: (e) => e.currentTarget.parentNode.classList.toggle('open') }, '⋯'),
        h('div', { class: 'menu-list' },
          can('operator') ? h('button', { onclick: () => groupDialog(a) }, 'Gruppe festlegen') : null,
          can('admin') && !a.removed ? h('button', { onclick: () => act(() => api('POST', `/api/agents/${id}/update`), 'Update eingeplant') }, 'Agent aktualisieren') : null,
          can('admin') && !a.removed ? h('button', { class: 'danger-t', onclick: () => confirmBox('Agent deinstallieren', `Der Agent wird von ${a.hostname} entfernt. Backups bleiben erhalten.`, 'Deinstallieren', () => api('POST', `/api/agents/${id}/uninstall`), true) }, 'Agent deinstallieren') : null,
          can('admin') ? h('button', { class: 'danger-t', onclick: () => confirmBox('Rechner entfernen', `${a.hostname} wird aus der Verwaltung entfernt. Backups bleiben bis zur Aufbewahrungsgrenze.`, 'Entfernen', async () => { await api('DELETE', '/api/agents/' + id); location.hash = '#/agents'; }, true) }, 'Aus Verwaltung entfernen') : null))),
    a.removed ? h('div', { class: 'callout warn' }, icon('alert'), `Der Agent wurde ${dt(a.removed)} auf dem Rechner deinstalliert. Die Backups bleiben erhalten und können weiter durchsucht und wiederhergestellt werden. Für neue Backups den Agent erneut verteilen.`) : null,
    h('div', { class: 'grid two-one' },
      card('Datenträger', diskBars(d.layout)),
      card('Zuordnung',
        h('dl', { class: 'kv' },
          h('dt', {}, 'Gruppe'), h('dd', {}, a.group || '–'),
          h('dt', {}, 'Backup-Pläne'), h('dd', {}, (d.plans || []).length ? d.plans.join(', ') : badge('canceled', 'kein Plan')),
          h('dt', {}, 'Angemeldet'), h('dd', {}, dt(d.enrolled)),
          h('dt', {}, 'MAC'), h('dd', { class: 'small' }, (d.macs || []).join(', ') || '–'),
          h('dt', {}, 'Agent-ID'), h('dd', { class: 'small mono' }, a.id)))),
    card(`Backups (${d.snapshots.length})`, timeline(d.snapshots), snapshotTable(d.snapshots, a)),
    card('Aufträge', jobTable(d.jobs, { live: true, filter: (j) => j.agent_id === id })));
}

function groupDialog(a) {
  const inp = h('input', { value: a.group || '', placeholder: 'z. B. Buchhaltung' });
  modal('Gruppe festlegen', field('Gruppe', inp),
    h('button', { class: 'btn primary', onclick: () => { closeModal(); act(() => api('PATCH', '/api/agents/' + a.id, { group: inp.value }), 'Gespeichert'); } }, 'Speichern'));
}

function snapshotTable(snaps, agent) {
  return table([
    ['Zeitpunkt', (s) => h('a', { href: '#/snapshots/' + s.id }, dt(s.time))],
    ['Art', (s) => s.kind === 'image' ? badge('info', 'Image') : badge('', 'Dateien')],
    ['Plan', (s) => s.plan_name || h('span', { class: 'muted' }, 'manuell')],
    ['Dateien', (s) => num(s.files), 'right'],
    ['Größe', (s) => bytes(s.bytes), 'right'],
    ['Neu', (s) => bytes(s.new_bytes), 'right'],
    ['', (s) => h('span', { class: 'actions' },
      s.keep ? h('span', { class: 'badge ok', title: 'Wird von der Aufbewahrung nicht gelöscht' }, icon('pin')) : null,
      s.verified ? h('span', { class: 'badge ' + (s.verify_ok ? 'ok' : 'bad'), title: `Restore-Prüfung ${dt(s.verified)}: ${s.verify_msg || ''}` }, s.verify_ok ? 'geprüft' : 'Prüfung fehlgeschlagen') : null,
      h('button', { class: 'btn small', onclick: (e) => { e.stopPropagation(); location.hash = '#/snapshots/' + s.id; } }, icon('search'), 'Durchsuchen'),
      can('admin') && s.kind === 'image' ? h('button', { class: 'btn small', onclick: (e) => { e.stopPropagation(); restoreImageDialog(s, agent); } }, icon('restore'), 'Image') : null), 'right'],
  ], snaps, { onclick: (s) => location.hash = '#/snapshots/' + s.id, empty: 'Noch keine Backups' });
}

// ---------- Backup-Browser ----------
const browse = { snap: null, vol: '', path: '', sel: new Set(), q: '' };

export async function pageSnapshot(id) {
  const d = await api('GET', '/api/snapshots/' + id);
  const s = d.snapshot;
  if (browse.snap !== id) { browse.snap = id; browse.vol = d.volumes[0]?.key || ''; browse.path = ''; browse.sel.clear(); browse.q = ''; }
  const vol = d.volumes.find((v) => v.key === browse.vol) || {};
  const listBox = h('div', {}, h('div', { class: 'empty' }, h('span', { class: 'spinner' }), ' Lade …'));
  const selInfo = h('span', { class: 'muted small' });
  const restoreBtn = h('button', { class: 'btn primary', onclick: () => restoreFilesDialog(s, vol, [...browse.sel]) }, icon('restore'), 'Auswahl wiederherstellen');
  const updateSel = () => { selInfo.textContent = browse.sel.size ? `${browse.sel.size} ausgewählt` : ''; restoreBtn.disabled = !browse.sel.size; };
  updateSel();

  const rowCols = (showPath) => [
    ['', (e) => h('input', { type: 'checkbox', checked: browse.sel.has(e.path), onclick: (ev) => ev.stopPropagation(), onchange: (ev) => { ev.target.checked ? browse.sel.add(e.path) : browse.sel.delete(e.path); updateSel(); } }), 'w-check'],
    ['Name', (e) => h('span', { class: 'cell-main' }, icon(e.type === 'd' ? 'folder' : e.type === 'l' ? 'link' : 'file', e.type === 'd' ? 'folder-ic' : 'file-ic'),
      h('span', {}, e.name, showPath ? h('div', { class: 'small muted' }, e.path) : null))],
    ['Größe', (e) => e.type === 'd' ? h('span', { class: 'muted small' }, num(e.items) + ' Einträge') : bytes(e.size), 'right'],
    ['Geändert', (e) => dt(e.mtime)],
    ['', (e) => e.type === 'l' ? '' : h('a', { class: 'btn small', href: `/api/snapshots/${id}/download?vol=${encodeURIComponent(browse.vol)}&path=${encodeURIComponent(e.path)}`, onclick: (ev) => ev.stopPropagation(), title: e.type === 'd' ? 'Als ZIP herunterladen' : 'Herunterladen' }, icon('download'), e.type === 'd' ? 'ZIP' : ''), 'right'],
  ];
  const open = (e) => { if (e.type === 'd') { browse.path = e.path; browse.q = ''; searchInput.value = ''; load(); } };

  const load = async (append) => {
    if (!can('operator')) { listBox.replaceChildren(h('div', { class: 'empty' }, 'Zum Durchsuchen ist die Rolle Operator nötig.')); return; }
    try {
      if (browse.q.length >= 2) {
        const r = await api('GET', `/api/snapshots/${id}/search?vol=${encodeURIComponent(browse.vol)}&q=${encodeURIComponent(browse.q)}`);
        listBox.replaceChildren(h('div', { class: 'small muted pad' }, r.total > r.entries.length ? `${num(r.total)} Treffer, die ersten ${r.entries.length} werden angezeigt` : `${num(r.total)} Treffer`),
          table(rowCols(true), r.entries, { onclick: (e) => { if (e.type === 'd') open(e); }, empty: 'Nichts gefunden' }));
        return;
      }
      const offset = append ? listBox._n || 0 : 0;
      const r = await api('GET', `/api/snapshots/${id}/ls?vol=${encodeURIComponent(browse.vol)}&path=${encodeURIComponent(browse.path)}&offset=${offset}&limit=500`);
      const parts = browse.path ? browse.path.split('/') : [];
      const crumbs = h('div', { class: 'crumbs' },
        h('button', { onclick: () => { browse.path = ''; load(); } }, icon('hdd'), browse.vol), parts.map((p, i) => [h('span', { class: 'sep' }, '›'), h('button', { onclick: () => { browse.path = parts.slice(0, i + 1).join('/'); load(); } }, p)]));
      if (!append) { listBox._rows = []; }
      listBox._rows.push(...r.entries);
      listBox._n = offset + r.entries.length;
      const more = listBox._n < r.total ? h('button', { class: 'btn block', onclick: () => load(true) }, `Weitere ${Math.min(500, r.total - listBox._n)} von ${num(r.total - listBox._n)} laden`) : null;
      listBox.replaceChildren(crumbs, table(rowCols(false), listBox._rows, { onclick: open, empty: 'Ordner ist leer' }), more);
    } catch (e) { listBox.replaceChildren(h('div', { class: 'callout bad' }, icon('alert'), e.message)); }
  };
  let timer;
  const searchInput = h('input', { placeholder: 'Im Backup suchen (z. B. rechnung oder *.pdf) …', value: browse.q, oninput: (e) => { clearTimeout(timer); timer = setTimeout(() => { browse.q = e.target.value.trim(); load(); }, 300); } });
  load();
  const isSystem = (s.layout?.disks || []).some((dk) => (dk.partitions || []).some((p) => p.volume === vol.key && p.role === 'system'));
  const time = Date.parse(s.time) / 1000;

  return h('div', {},
    pageHead(`Backup vom ${dt(time)}`, h('span', {}, h('a', { href: '#/agents/' + s.agent_id }, s.hostname), ` · ${s.kind === 'image' ? 'Image-Backup' : 'Datei-Backup'} · ${s.id}`),
      can('admin') && s.kind === 'image' ? h('button', { class: 'btn primary', onclick: () => restoreImageDialog({ id: s.id, kind: s.kind, time }, { id: s.agent_id, hostname: s.hostname }, d) }, icon('restore'), 'Ganzes System wiederherstellen') : null,
      can('operator') ? h('button', { class: 'btn', title: 'Liest das Backup vollständig wie bei einer Wiederherstellung und prüft jeden Datenblock', onclick: () => act(() => api('POST', `/api/snapshots/${s.id}/verify`), 'Prüfung gestartet – Fortschritt unter Aufträge') }, icon('shield'), 'Wiederherstellbarkeit prüfen') : null,
      can('operator') ? h('button', { class: 'btn', onclick: () => act(() => api('POST', `/api/snapshots/${s.id}/keep`, { keep: true }), 'Wird dauerhaft behalten') }, icon('pin'), 'Behalten') : null,
      can('admin') ? h('button', { class: 'btn danger-o', onclick: () => confirmBox('Backup löschen', 'Das Backup wird gelöscht. Der Speicher wird bei der nächsten Bereinigung frei.', 'Löschen', async () => { await api('DELETE', '/api/snapshots/' + s.id); location.hash = '#/agents/' + s.agent_id; }, true) }, icon('trash')) : null),
    (s.warnings || []).length ? h('div', { class: 'callout warn' }, icon('alert'), h('div', {}, s.warnings.map((w) => h('div', {}, w)))) : null,
    d.verified ? h('div', { class: 'callout ' + (d.verify_ok ? 'ok' : 'bad') }, icon(d.verify_ok ? 'check' : 'alert'), `Restore-Prüfung vom ${dt(d.verified)}: ${d.verify_msg}`) : null,
    card(null,
      h('div', { class: 'tabs' }, d.volumes.map((v) => h('button', { class: v.key === browse.vol ? 'active' : '', onclick: () => { browse.vol = v.key; browse.path = ''; browse.sel.clear(); browse.q = ''; window.dispatchEvent(new Event('bastion-refresh')); } },
        icon('hdd'), `${v.key}`, h('span', { class: 'muted small' }, ' ' + bytes(v.bytes))))),
      h('div', { class: 'vol-info small muted' }, `${num(vol.files)} Dateien · ${bytes(vol.bytes)} · ${vol.fs || '–'} · Snapshot: ${vol.snapshot || '–'}${vol.errors ? ` · ${vol.errors} Dateien nicht lesbar` : ''}`),
      h('div', { class: 'toolbar' }, h('div', { class: 'search grow' }, icon('search'), searchInput),
        can('operator') ? h('div', { class: 'actions' }, selInfo, restoreBtn, !isSystem ? h('button', { class: 'btn', onclick: () => restoreFilesDialog(s, vol, []) }, 'Ganzes Volume') : null) : null),
      listBox));
}

async function restoreFilesDialog(s, vol, paths) {
  const agents = await api('GET', '/api/agents');
  const target = h('input', { name: 'target', placeholder: s.os === 'windows' ? 'C:\\Wiederhergestellt' : '/root/wiederhergestellt' });
  const tWrap = field('Zielordner', target);
  tWrap.classList.add('hidden');
  const form = h('form', { onsubmit: (e) => e.preventDefault() },
    h('div', { class: 'callout' }, icon('restore'), paths.length ? `${paths.length} Einträge aus ${vol.key}` : `Komplettes Volume ${vol.key}`),
    field('Ziel-Rechner', h('select', { name: 'agent_id' }, agents.filter((a) => a.os === s.os).map((a) => h('option', { value: a.id, selected: a.id === s.agent_id }, a.hostname + (a.online ? '' : ' (offline)'))))),
    field('Ziel', h('select', { name: 'mode', onchange: (e) => tWrap.classList.toggle('hidden', e.target.value !== 'alt') }, h('option', { value: 'orig' }, 'Ursprünglicher Ort (' + vol.root + ')'), h('option', { value: 'alt' }, 'Anderer Ordner'))),
    tWrap,
    field('Vorhandene Dateien', h('select', { name: 'overwrite' },
      h('option', { value: 'always' }, 'überschreiben (geöffnete Dateien beim Neustart ersetzen)'),
      h('option', { value: 'rename' }, 'behalten, Wiederherstellung umbenennen'),
      h('option', { value: 'never' }, 'nicht überschreiben'))),
    !paths.length ? h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'format' }), 'Volume vorher formatieren (exakter Stand des Backups)') : null);
  modal('Dateien wiederherstellen', form, h('button', {
    class: 'btn primary', onclick: () => {
      const f = formData(form);
      if (f.mode === 'alt' && !f.target) return toast('Bitte Zielordner angeben', true);
      closeModal();
      act(async () => { const r = await api('POST', `/api/snapshots/${s.id}/restore`, { agent_id: f.agent_id, volume: vol.key, paths, overwrite: f.overwrite, target: f.mode === 'alt' ? f.target : '', format: !!f.format }); location.hash = '#/jobs/' + r.job_id; }, null, () => {});
    },
  }, icon('restore'), 'Wiederherstellen'));
}

export async function restoreImageDialog(snap, agent, detail) {
  detail = detail || await api('GET', '/api/snapshots/' + snap.id);
  let mode = 'live';
  const opt = (m, ic, title, text) => h('button', { type: 'button', class: 'choice' + (m === mode ? ' active' : ''), onclick: (e) => {
    mode = m; e.currentTarget.parentNode.querySelectorAll('.choice').forEach((c) => c.classList.remove('active')); e.currentTarget.classList.add('active'); liveOpts.classList.toggle('hidden', m !== 'live');
  } }, icon(ic), h('b', {}, title), h('span', { class: 'small muted' }, text));
  const liveOpts = h('div', {},
    field('Vorwarnzeit für den Benutzer (Sekunden)', h('input', { type: 'number', name: 'delay_sec', value: 120, min: 0 })),
    field('Meldung an den Benutzer', h('input', { name: 'message', value: 'Ihr Computer wird in Kürze für eine Systemwiederherstellung neu gestartet. Bitte speichern Sie Ihre Arbeit.' })));
  const form = h('form', { onsubmit: (e) => e.preventDefault() },
    h('div', { class: 'steps' },
      h('div', {}, h('b', {}, '1'), `Backup vom ${dt(snap.time)} von ${agent.hostname}`),
      detail.system_disk ? h('div', {}, h('b', {}, '2'), `Zielplatte mindestens ${bytes(detail.required_disk)} (Original ${bytes(detail.system_disk.size)}) – kleinere Platten sind möglich`) : null,
      h('div', {}, h('b', {}, '3'), 'Partitionen werden passend angelegt, das System wird startfähig gemacht')),
    h('div', { class: 'choices' },
      opt('live', 'play', 'Im laufenden Betrieb', 'Rechner startet von selbst in die Wiederherstellung – kein USB-Stick nötig'),
      opt('media', 'life', 'USB / ISO / PXE', 'Rechner startet nicht mehr oder hat eine neue Festplatte')),
    liveOpts,
    h('div', { class: 'callout bad' }, icon('alert'), 'Alle Daten auf dem Systemdatenträger werden durch den Stand des Backups ersetzt.'));
  modal('Image wiederherstellen', form, h('button', {
    class: 'btn danger', onclick: async () => {
      const f = formData(form);
      closeModal();
      try {
        const r = await api('POST', `/api/snapshots/${snap.id}/restore-image`, { agent_id: agent.id, mode, delay_sec: f.delay_sec, message: f.message });
        if (mode === 'media') {
          modal('Code für das Rettungsmedium', h('div', {},
            h('p', {}, 'Rechner vom Bastion-Rettungsmedium (USB/ISO) oder per Netzwerk (PXE) starten und im Menü „Wiederherstellungs-Code“ diesen Code eingeben:'),
            h('div', { class: 'code-big' }, r.code),
            h('p', { class: 'small muted' }, 'Der Code ist 48 Stunden gültig. Alternativ im Rettungssystem anmelden und das Backup direkt auswählen.')),
            h('button', { class: 'btn primary', onclick: () => { closeModal(); location.hash = '#/jobs/' + r.job_id; } }, 'Zum Auftrag'));
        } else location.hash = '#/jobs/' + r.job_id;
      } catch (e) { toast(e.message, true); }
    },
  }, icon('restore'), 'Wiederherstellung starten'), true);
}
