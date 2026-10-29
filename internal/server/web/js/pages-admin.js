// Agent-Verteilung, Rettungssysteme und Einstellungen.
import { h, api, icon, bytes, dt, badge, can, state, toast, act, table, copyBox, formData, field, pageHead, card, onPage, modal, closeModal } from './lib.js';

// ---------- Agent-Verteilung ----------
const rollout = { hosts: [], sel: new Set() };

export async function pageRollout() {
  const [creds, rolls, plans, info, tokens] = await Promise.all([
    can('admin') ? api('GET', '/api/credentials') : [], api('GET', '/api/rollouts'), api('GET', '/api/plans'),
    api('GET', '/api/install-info'), can('admin') ? api('GET', '/api/tokens') : []]);
  const credOpts = () => creds.length ? creds.map((c) => h('option', { value: c.id }, `${c.name} (${c.username})`)) : h('option', { value: 0 }, '– zuerst Zugangsdaten anlegen –');
  const results = h('div');
  const renderHosts = () => {
    results.replaceChildren(rollout.hosts.length ? h('div', {},
      h('div', { class: 'toolbar' },
        h('button', { class: 'btn small', onclick: () => { rollout.hosts.filter((x) => !x.agent_id).forEach((x) => rollout.sel.add(x.name || x.ip)); renderHosts(); } }, 'Alle ohne Agent auswählen'),
        h('button', { class: 'btn small', onclick: () => { rollout.sel.clear(); renderHosts(); } }, 'Auswahl leeren'),
        h('span', { class: 'muted small' }, `${rollout.sel.size} von ${rollout.hosts.length} ausgewählt`)),
      table([
        ['', (x) => h('input', { type: 'checkbox', checked: rollout.sel.has(x.name || x.ip), onchange: (e) => { const k = x.name || x.ip; e.target.checked ? rollout.sel.add(k) : rollout.sel.delete(k); renderHosts(); } }), 'w-check'],
        ['Rechner', (x) => h('div', {}, x.name || x.ip, x.name && x.ip ? h('div', { class: 'muted small' }, x.ip) : null)],
        ['System', (x) => h('span', { class: 'small' }, x.os || '?')],
        ['Dienste', (x) => h('span', { class: 'small' }, [x.smb && 'SMB', x.winrm && 'WinRM', x.ssh && 'SSH'].filter(Boolean).join(', ') || (x.last_logon ? 'zuletzt ' + dt(x.last_logon) : ''))],
        ['Agent', (x) => x.agent_id ? h('a', { href: '#/agents/' + x.agent_id }, badge('success', 'vorhanden')) : badge('', 'fehlt')],
      ], rollout.hosts)) : h('div', { class: 'empty' }, icon('search'), ' Rechner über Active Directory oder einen IP-Bereich suchen.'));
  };
  renderHosts();

  const deployForm = h('form', { onsubmit: (e) => e.preventDefault() },
    h('div', { class: 'form-grid' },
      field('Zugangsdaten', h('select', { name: 'cred_id' }, credOpts())),
      field('Methode', h('select', { name: 'method' }, h('option', { value: 'auto' }, 'Automatisch'), h('option', { value: 'smb' }, 'SMB + Dienststeuerung (wie PsExec)'), h('option', { value: 'winrm' }, 'WinRM'), h('option', { value: 'ssh' }, 'SSH (Linux)'))),
      field('Backup-Plan zuweisen', h('select', { name: 'plan_id' }, h('option', { value: 0 }, '– keiner –'), plans.map((p) => h('option', { value: p.plan.id }, p.plan.name)))),
      field('Gruppe', h('input', { name: 'group', placeholder: 'optional' }))),
    field('Zusätzliche Rechner (Name oder IP, einer pro Zeile)', h('textarea', { name: 'extra', rows: 2 })),
    h('div', { class: 'actions' },
      h('button', { class: 'btn', onclick: (e) => { const f = deployInput(); if (f) checkDialog(f, e.currentTarget); } }, icon('shield'), 'Vorab prüfen'),
      h('button', { class: 'btn primary', onclick: () => { const f = deployInput(); if (f) deploy(f); } }, icon('send'), 'Agent installieren')),
    h('p', { class: 'small muted' }, 'Ausgeschaltete oder nicht erreichbare Rechner werden automatisch später erneut versucht (Einstellungen).'));
  const deployInput = () => {
    const f = formData(deployForm);
    f.targets = [...new Set([...rollout.sel, ...f.extra.split('\n').map((x) => x.trim()).filter(Boolean)])];
    if (!f.targets.length) { toast('Keine Rechner ausgewählt', true); return null; }
    if (!Number(f.cred_id)) { toast('Zuerst Zugangsdaten anlegen', true); return null; }
    return f;
  };
  const deploy = (f, targets = f.targets) =>
    act(() => api('POST', '/api/rollouts', { targets, method: f.method, cred_id: Number(f.cred_id), plan_id: Number(f.plan_id), group: f.group }), `Verteilung auf ${targets.length} Rechner gestartet`)
      .then(() => rollout.sel.clear());
  const checkDialog = async (f, btn) => {
    const label = btn.innerHTML; btn.disabled = true; btn.textContent = `Prüfe ${f.targets.length} Rechner …`;
    let res;
    try { res = await api('POST', '/api/rollouts/check', { targets: f.targets, method: f.method, cred_id: Number(f.cred_id) }); }
    catch (err) { toast(err.message, true); return; }
    finally { btn.disabled = false; btn.innerHTML = label; }
    const good = res.filter((r) => r.ok).map((r) => r.target);
    const body = h('div', { class: 'check-list' },
      h('p', {}, `${good.length} von ${res.length} Rechnern bereit.`),
      res.map((r) => h('details', { class: 'check-host' + (r.ok ? '' : ' bad'), open: !r.ok },
        h('summary', {}, badge(r.ok ? 'success' : 'failed', r.ok ? 'bereit' : 'Problem'), ' ', h('b', {}, r.target),
          r.method ? h('span', { class: 'muted small' }, ` · ${r.method.toUpperCase()}`) : null,
          r.agent_id ? h('span', { class: 'badge info sp' }, 'Agent schon vorhanden') : null),
        h('ul', { class: 'check-steps' }, r.steps.map((s) => h('li', { class: s.ok ? 'ok' : 'bad' },
          h('span', { class: 'mark' }, s.ok ? '✓' : '✗'), h('b', {}, s.name), ' ', h('span', { class: 'small' }, s.detail)))))));
    modal('Vorab-Prüfung', body, good.length ? h('button', { class: 'btn primary', onclick: () => { closeModal(); deploy(f, good); } }, icon('send'),
      good.length === res.length ? 'Alle installieren' : `${good.length} bereite installieren`) : null, true);
  };

  const searchBtn = (label, run) => h('button', { class: 'btn', onclick: async (e) => {
    const b = e.currentTarget; b.disabled = true; const t = b.textContent; b.textContent = 'Suche läuft …';
    try { rollout.hosts = await run(); renderHosts(); } catch (err) { toast(err.message, true); }
    b.disabled = false; b.textContent = t;
  } }, label);
  const adForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Domänencontroller', h('input', { name: 'server', placeholder: 'dc01.firma.local' })),
    field('Basis-DN (optional)', h('input', { name: 'base', placeholder: 'OU=Clients,DC=firma,DC=local' })),
    field('Zugangsdaten', h('select', { name: 'cred_id' }, credOpts()))),
    searchBtn('Im AD suchen', () => { const f = formData(adForm); return api('POST', '/api/discover/ldap', { server: f.server, base: f.base, cred_id: Number(f.cred_id) }); }));
  const scanForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Netzwerk', h('input', { name: 'cidr', placeholder: '172.16.50.0/24' }))),
    searchBtn('Netzwerk durchsuchen', () => api('POST', '/api/discover/scan', { cidr: formData(scanForm).cidr })));

  const credForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Bezeichnung', h('input', { name: 'name', placeholder: 'Domänen-Admin' })),
    field('Art', h('select', { name: 'kind' }, h('option', { value: 'windows' }, 'Windows (SMB/WinRM/AD)'), h('option', { value: 'ssh' }, 'Linux (SSH)'))),
    field('Benutzer', h('input', { name: 'username', placeholder: 'FIRMA\\admin oder root' })),
    field('Passwort oder privater SSH-Schlüssel', h('input', { name: 'secret', type: 'password' }))),
    h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/credentials', formData(credForm)), 'Gespeichert') }, icon('plus'), 'Zugangsdaten speichern'));

  const tokenOut = h('div');
  const tokenForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid four' },
    field('Bezeichnung', h('input', { name: 'label', placeholder: 'GPO Standort Nord' })),
    field('Gruppe', h('input', { name: 'group' })),
    field('Plan', h('select', { name: 'plan_id' }, h('option', { value: 0 }, '– keiner –'), plans.map((p) => h('option', { value: p.plan.id }, p.plan.name)))),
    field('Gültig (Tage, 0 = unbegrenzt)', h('input', { type: 'number', name: 'days', value: 30 }))),
    h('button', { class: 'btn', onclick: async () => {
      try {
        const r = await api('POST', '/api/tokens', { ...formData(tokenForm), uses: -1 });
        tokenOut.replaceChildren(h('div', { class: 'callout' }, icon('shield'), h('span', {}, 'Token: ', h('b', { class: 'mono' }, r.token), ' – wird nur jetzt angezeigt.')),
          h('h3', {}, 'Windows (PowerShell als Administrator)'), copyBox(r.commands.windows_download),
          h('h3', {}, 'Windows (Programm liegt vor, z. B. GPO-Startskript)'), copyBox(r.commands.gpo),
          h('h3', {}, 'Linux'), copyBox(r.commands.linux));
      } catch (e) { toast(e.message, true); }
    } }, 'Token erzeugen'), tokenOut);

  const history = table([
    ['Rechner', (r) => r.agent_id ? h('a', { href: '#/agents/' + r.agent_id }, r.target) : r.target],
    ['Methode', (r) => r.method],
    ['Status', (r) => badge(r.status)],
    ['Meldung', (r) => h('span', { class: 'small' }, r.message)],
    ['Zeit', (r) => dt(r.updated)],
    ['', (r) => can('admin') ? h('span', { class: 'actions' }, r.status === 'failed' || r.status === 'waiting' ? h('button', { class: 'btn small', onclick: () => act(() => api('POST', `/api/rollouts/${r.id}/retry`)) }, r.status === 'waiting' ? 'Jetzt versuchen' : 'Wiederholen') : null,
      h('button', { class: 'btn small', onclick: () => act(() => api('DELETE', '/api/rollouts/' + r.id)) }, icon('x'))) : '', 'right'],
  ], rolls, { empty: 'Noch keine Verteilung' });
  let reload;
  onPage('rollout', () => { clearTimeout(reload); reload = setTimeout(() => window.dispatchEvent(new Event('bastion-refresh')), 400); });

  return h('div', {},
    pageHead('Agent-Verteilung', 'Agents zentral auf Windows- und Linux-Rechnern installieren'),
    !info.binaries?.length ? h('div', { class: 'callout warn' }, icon('alert'), 'Auf dem Server liegen noch keine Agent-Programme (Ordner data/agents/<os>-<arch>/).') : null,
    can('admin') ? h('div', { class: 'grid two' },
      card('1. Rechner finden', h('h3', {}, 'Active Directory'), adForm, h('h3', {}, 'IP-Bereich'), scanForm),
      card('Zugangsdaten',
        creds.length ? table([['Name', (c) => c.name], ['Art', (c) => c.kind], ['Benutzer', (c) => c.username],
          ['', (c) => h('button', { class: 'btn small danger-o', onclick: () => act(() => api('DELETE', '/api/credentials/' + c.id)) }, icon('trash')), 'right']], creds) : h('div', { class: 'muted small' }, 'Noch keine Zugangsdaten.'),
        h('h3', {}, 'Neu'), credForm)) : null,
    can('admin') ? card('2. Auswählen und installieren', results, h('h3', {}, 'Installation'), deployForm) : null,
    card('Verlauf', history),
    can('admin') ? card('Manuell, per GPO oder Intune',
      h('p', { class: 'small muted' }, `Server: ${info.server} · Zertifikat SHA-256: `, h('span', { class: 'mono' }, info.fingerprint)),
      tokenForm,
      tokens.length ? h('div', {}, h('h3', {}, 'Vorhandene Tokens'), table([['Bezeichnung', (t) => t.label || '–'], ['Gruppe', (t) => t.group || '–'], ['Gültig bis', (t) => t.expires ? dt(t.expires) : 'unbegrenzt'],
        ['', (t) => h('button', { class: 'btn small danger-o', onclick: () => act(() => api('DELETE', '/api/tokens/' + t.hash)) }, 'Widerrufen'), 'right']], tokens)) : null,
      h('h3', {}, 'Agent-Programme auf dem Server'),
      table([['Plattform', (b) => b.os_arch], ['Version', (b) => b.version || '?'], ['Größe', (b) => bytes(b.size)], ['SHA-256', (b) => h('span', { class: 'mono small' }, b.sha256.slice(0, 16) + '…')]], info.binaries || []),
      h('button', { class: 'btn small', onclick: () => act(() => api('POST', '/api/agent-binaries/rescan'), 'Neu eingelesen') }, icon('refresh'), 'Neu einlesen')) : null);
}

// ---------- Rettungssysteme ----------
export async function pageRescue() {
  const r = await api('GET', '/api/rescue-status');
  const ok = (b) => b ? badge('success', 'vorhanden') : badge('canceled', 'fehlt');
  const data = r.data_dir;
  return h('div', {},
    pageHead('Rettungssysteme', 'Für Image-Wiederherstellung – im laufenden Betrieb, per USB/ISO oder per Netzwerk (PXE)'),
    h('div', { class: 'grid two' },
      card('Windows (WinPE)',
        h('dl', { class: 'kv' }, h('dt', {}, 'WinPE-Image'), h('dd', {}, ok(r.winpe)), h('dt', {}, 'Boot-ISO'), h('dd', {}, ok(r.winpe_iso))),
        r.winpe_iso ? h('a', { class: 'btn primary', href: '/api/rescue/file/bastion-rescue-winpe.iso' }, icon('download'), 'ISO herunterladen') : null,
        h('p', { class: 'small' }, 'Neu erstellen auf einem Windows-PC mit Windows ADK und WinPE-Add-on (PowerShell als Administrator):'),
        copyBox(`.\\rescue\\winpe\\build.ps1 -Server ${r.server} -Fingerprint ${r.fingerprint} -Agent .\\dist\\windows-amd64\\bastion-agent.exe -Out "${data}\\rescue"`),
        h('p', { class: 'small muted' }, 'USB-Stick: ISO z. B. mit Rufus schreiben.')),
      card('Linux',
        h('dl', { class: 'kv' }, h('dt', {}, 'Kernel + Initramfs'), h('dd', {}, ok(r.linux))),
        h('p', { class: 'small' }, 'Erstellen auf einem Debian/Ubuntu-System als root:'),
        copyBox(`sudo ./rescue/linux/build.sh --server ${r.server} --fingerprint ${r.fingerprint} --agent ./dist/linux-amd64/bastion-agent --out ${data}/rescue`))),
    card('Netzwerkstart (PXE)',
      h('dl', { class: 'kv' }, h('dt', {}, 'Status'), h('dd', {}, r.pxe_enabled ? badge('success', 'aktiv') : badge('', 'aus')),
        h('dt', {}, 'iPXE UEFI / BIOS'), h('dd', {}, ok(r.ipxe_efi), ' ', ok(r.ipxe_bios)), h('dt', {}, 'wimboot'), h('dd', {}, ok(r.wimboot))),
      h('p', { class: 'small' }, `iPXE und wimboot mit rescue\\pxe\\fetch-ipxe.ps1 nach ${data}${data.includes('\\') ? '\\' : '/'}pxe laden und PXE in den Einstellungen aktivieren. Der Server antwortet als proxyDHCP – der vorhandene DHCP-Server bleibt unverändert.`)),
    card('So läuft eine Image-Wiederherstellung',
      h('ol', { class: 'steps-list' },
        h('li', {}, 'Rechner öffnen → Backup wählen → „Image wiederherstellen“.'),
        h('li', {}, h('b', {}, 'Im laufenden Betrieb: '), 'Der Rechner startet von selbst in die Wiederherstellung, die Festplatte wird passend partitioniert (auch kleiner als vorher), das Image zurückgespielt und das System startfähig gemacht.'),
        h('li', {}, h('b', {}, 'Rechner startet nicht mehr: '), 'Vom USB-Stick/ISO oder per PXE starten. Im Menü mit einem Bastion-Administrator anmelden und das Backup auswählen – oder den Code aus der Weboberfläche eingeben.'),
        h('li', {}, 'Fortschritt mit Tempo und Restzeit erscheint live im Auftrag.'))));
}

// ---------- Einstellungen ----------
export async function pageSettings() {
  if (!can('admin')) return h('div', {}, pageHead('Einstellungen'), card('Eigenes Passwort', passwordForm()));
  const [s, users, audit, d, dr] = await Promise.all([api('GET', '/api/settings'), api('GET', '/api/users'), api('GET', '/api/audit'), api('GET', '/api/dashboard'), api('GET', '/api/server-backup')]);
  const f = (name, label, attrs, hint) => field(label, h('input', { name, value: s[name] || '', ...attrs }), hint);
  const sw = (name, checked, label) => h('label', { class: 'check' }, h('input', { type: 'checkbox', name, checked }), label);
  const general = h('form', { onsubmit: (e) => e.preventDefault() },
    card('Allgemein',
      h('div', { class: 'form-grid' },
        f('public_url', 'Server-Adresse für Agents', { placeholder: s.effective_public_url }),
        f('max_parallel_backups', 'Gleichzeitige Backups', { type: 'number', placeholder: '10' }),
        f('bandwidth_kbps', 'Bandbreite pro Rechner (KB/s)', { type: 'number', placeholder: '0' }, '0 = unbegrenzt'),
        f('gc_time', 'Tägliche Bereinigung um', { placeholder: '03:30' }),
        f('scrub_time', 'Tägliche Integritätsprüfung um', { placeholder: '' }, 'leer = aus'),
        f('rollout_retry_minutes', 'Agent-Verteilung: neuer Versuch alle (Min.)', { type: 'number', placeholder: '15' }, 'für ausgeschaltete/nicht erreichbare Rechner'),
        f('rollout_retry_hours', 'Agent-Verteilung: Versuche über (Stunden)', { type: 'number', placeholder: '24' }, '0 = nicht wiederholen'),
        f('verify_time', 'Restore-Prüfung um', { placeholder: '04:30' }, 'jeweils das neueste Backup jedes Rechners'),
        f('verify_days', 'Restore-Prüfung alle (Tage)', { type: 'number', placeholder: '7' }, '0 = aus')),
      h('p', { class: 'small muted' }, 'Zertifikat SHA-256: ', h('span', { class: 'mono' }, s.fingerprint), ` · Daten: ${s.data_dir}`)),
    card('E-Mail-Benachrichtigung',
      h('div', { class: 'form-grid' },
        f('smtp_host', 'SMTP-Server'), f('smtp_port', 'Port', { placeholder: '587' }), f('smtp_user', 'Benutzer'),
        field('Passwort', h('input', { name: 'smtp_pass', type: 'password', placeholder: s.smtp_pass_set ? '(gespeichert)' : '' })),
        f('smtp_from', 'Absender'), f('notify_to', 'Empfänger (Komma-getrennt)'),
        f('notify_stale_days', 'Warnen nach Tagen ohne Backup', { type: 'number', placeholder: '3' })),
      sw('notify_failure_cb', s.notify_failure !== '0', 'Bei fehlgeschlagenen Aufträgen benachrichtigen')),
    card('Netzwerkstart (PXE)',
      h('div', { class: 'form-grid' }, f('pxe_ip', 'IP-Adresse für PXE', {}, 'leer = automatisch')),
      sw('pxe_enabled_cb', s.pxe_enabled === '1', 'PXE-Dienste aktivieren (Neustart des Servers nötig)'),
      sw('pxe_dhcp_cb', s.pxe_dhcp !== '0', 'proxyDHCP beantworten')),
    h('div', { class: 'actions' },
      h('button', { class: 'btn primary', onclick: () => {
        const x = formData(general);
        x.notify_failure = x.notify_failure_cb ? '1' : '0'; x.pxe_enabled = x.pxe_enabled_cb ? '1' : '0'; x.pxe_dhcp = x.pxe_dhcp_cb ? '1' : '0';
        for (const k of Object.keys(x)) { if (k.endsWith('_cb')) delete x[k]; else x[k] = String(x[k]); }
        if (!x.smtp_pass) delete x.smtp_pass;
        act(() => api('PUT', '/api/settings', x), 'Einstellungen gespeichert');
      } }, icon('check'), 'Speichern'),
      h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/settings/test-mail'), 'Test-E-Mail gesendet', () => {}) }, 'Test-E-Mail senden')));
  const userForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Benutzer', h('input', { name: 'name' })),
    field('Passwort (min. 10 Zeichen)', h('input', { name: 'password', type: 'password' })),
    field('Rolle', h('select', { name: 'role' }, h('option', { value: 'viewer' }, 'Nur lesen'), h('option', { value: 'operator' }, 'Operator (sichern, Dateien wiederherstellen)'), h('option', { value: 'admin' }, 'Administrator')))),
    h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/users', formData(userForm)), 'Benutzer angelegt') }, icon('plus'), 'Anlegen'));
  const drForm = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Passwort der Server-Sicherung', h('input', { name: 'passphrase', type: 'password', autocomplete: 'new-password', placeholder: dr.passphrase_set ? '(gespeichert – nur zum Ändern ausfüllen)' : 'mindestens 12 Zeichen' }),
      'Wird zum Wiederherstellen gebraucht – getrennt vom Server aufbewahren!'),
    field('Täglich um', h('input', { name: 'time', value: dr.time, placeholder: '02:15' }))));
  const last = dr.last;
  const drCard = card('Server-Sicherung (Notfall-Wiederherstellung)',
    h('p', { class: 'small' }, 'Sichert Datenbank, Schlüssel und Zertifikat dieses Servers verschlüsselt in jeden Speicherort. Ohne diese Daten sind die Backups bei einem Ausfall des Servers nicht lesbar.'),
    !dr.passphrase_set ? h('div', { class: 'callout bad' }, icon('alert'), 'Noch nicht eingerichtet: Passwort festlegen, damit die Server-Sicherung täglich läuft.')
      : last ? h('div', { class: 'callout ' + (last.ok && last.offsite ? 'ok' : last.ok ? 'warn' : 'bad') }, icon(last.ok ? 'check' : 'alert'), `${dt(last.time)}: ${last.message}`)
        : h('div', { class: 'callout warn' }, icon('alert'), 'Noch keine Server-Sicherung gelaufen.'),
    drForm,
    h('div', { class: 'actions' },
      h('button', { class: 'btn primary', onclick: () => { const x = formData(drForm); act(() => api('PUT', '/api/server-backup', x), 'Gespeichert'); } }, icon('check'), 'Speichern'),
      dr.passphrase_set ? h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/server-backup/run'), 'Server-Sicherung geschrieben') }, icon('play'), 'Jetzt sichern') : null,
      dr.passphrase_set ? h('a', { class: 'btn', href: '/api/server-backup/download' }, icon('download'), 'Herunterladen') : null),
    h('h3', {}, 'Wiederherstellen auf einem neuen Server'),
    h('p', { class: 'small' }, 'bastion-server.exe auf den neuen Server kopieren, dann (als Administrator):'),
    copyBox(`bastion-server.exe restore -from "\\\\NAS\\Backups\\Bastion" -data C:\\BastionData`),
    copyBox('bastion-server.exe -install-service -data C:\\BastionData'),
    h('p', { class: 'small muted' }, '-from: Ordner eines Speicherorts (die neueste Sicherung wird gewählt) oder eine .bsb-Datei. Neuere Backups aus den Speicherorten werden beim ersten Start automatisch übernommen. Den neuen Server unter derselben Adresse (IP/Name) betreiben, dann verbinden sich alle Agents von selbst.'));
  return h('div', {},
    pageHead('Einstellungen'),
    drCard,
    general,
    h('div', { class: 'grid two' },
      card('Benutzer',
        table([['Name', (u) => u.name], ['Rolle', (u) => u.role], ['', (u) => u.name !== state.me.name ? h('button', { class: 'btn small danger-o', onclick: () => act(() => api('DELETE', '/api/users/' + u.id)) }, icon('trash')) : h('span', { class: 'muted small' }, 'Sie'), 'right']], users),
        h('h3', {}, 'Neuer Benutzer'), userForm, h('h3', {}, 'Eigenes Passwort'), passwordForm()),
      card('Wartung',
        h('p', {}, d.repo?.message || 'Noch keine Bereinigung gelaufen.'),
        d.repo?.time ? h('p', { class: 'small muted' }, 'Stand ' + dt(d.repo.time)) : null,
        d.scrub ? h('p', {}, 'Integritätsprüfung: ', d.scrub.message, ' (' + dt(d.scrub.time) + ')') : null,
        h('div', { class: 'actions' },
          h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/maintenance/gc'), 'Bereinigung gestartet', () => {}) }, icon('trash'), 'Jetzt bereinigen'),
          h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/maintenance/scrub'), 'Prüfung gestartet', () => {}) }, icon('shield'), 'Integrität prüfen')),
        h('p', { class: 'small muted' }, 'Die Bereinigung löscht Daten, die zu keinem Backup mehr gehören – je Speicherort. Die Integritätsprüfung entschlüsselt und prüft alle gespeicherten Blöcke.'))),
    card('Protokoll der Verwaltung', table([
      ['Zeit', (a) => dt(a.time)], ['Benutzer', (a) => a.user], ['Aktion', (a) => h('code', {}, a.action)], ['Details', (a) => h('span', { class: 'small' }, a.detail)]], audit.slice(0, 100))));
}

function passwordForm() {
  const form = h('form', { onsubmit: (e) => e.preventDefault() }, h('div', { class: 'form-grid' },
    field('Altes Passwort', h('input', { name: 'old', type: 'password' })),
    field('Neues Passwort', h('input', { name: 'new', type: 'password' }))),
    h('button', { class: 'btn', onclick: () => act(() => api('POST', '/api/password', formData(form)), 'Passwort geändert', () => form.reset()) }, 'Ändern'));
  return form;
}
