# Bastion Backup – Wiki

Handbuch für Betrieb, Administration und Notfall. Stand: Version 0.3.0.

**Inhalt**
1. [Überblick](#1-überblick)
2. [Architektur](#2-architektur)
3. [Installation des Servers](#3-installation-des-servers)
4. [Agents verteilen](#4-agents-verteilen)
5. [Speicherorte](#5-speicherorte)
6. [Backup-Pläne](#6-backup-pläne)
7. [Wie ein Backup abläuft](#7-wie-ein-backup-abläuft)
8. [Wiederherstellung](#8-wiederherstellung)
9. [Rettungssystem (WinPE / Linux / PXE)](#9-rettungssystem-winpe--linux--pxe)
10. [Restore-Prüfung](#10-restore-prüfung)
11. [Server-Sicherung und Notfall-Wiederherstellung des Servers](#11-server-sicherung-und-notfall-wiederherstellung-des-servers)
12. [Weboberfläche](#12-weboberfläche)
13. [Einstellungen (Referenz)](#13-einstellungen-referenz)
14. [Kommandozeile (Referenz)](#14-kommandozeile-referenz)
15. [Netzwerk und Firewall](#15-netzwerk-und-firewall)
16. [Sicherheit](#16-sicherheit)
17. [Wartung und Updates](#17-wartung-und-updates)
18. [Fehlerbehebung](#18-fehlerbehebung)
19. [Grenzen](#19-grenzen)
20. [Entwicklung](#20-entwicklung)

---

## 1. Überblick

Bastion ist eine selbst gehostete, zentral verwaltete Backup-Lösung für Windows- und Linux-Rechner.

| Funktion | Kurz |
|---|---|
| Zentrale Verwaltung | Weboberfläche für Rechner, Pläne, Speicherorte, Aufträge, Verteilung |
| Agent | ein Programm (~10 MB), Windows-Dienst bzw. systemd-Dienst, erscheint unter „Installierte Apps“ |
| Backups im laufenden Betrieb | Windows: VSS **mit Writern** (anwendungskonsistent), Linux: LVM/btrfs |
| Image-Backups | bootfähig, Wiederherstellung auch auf **kleinere** oder größere Festplatten |
| Image-Restore per Klick | Rechner startet selbst in WinPE/Linux im RAM, spielt das Image ein, startet neu |
| Datei-Restore | an den Originalort, anderswohin, oder Download im Browser |
| Agent-Verteilung | AD-Suche, IP-Scan, Push per SMB/WinRM/SSH, Vorab-Prüfung, automatische Wiederholung |
| Speicher | Deduplizierung, zstd, AES-256-GCM, mehrere Speicherorte (lokal, SMB) |
| Restore-Prüfung | liest Backups vollständig und prüft Startfähigkeit, auch automatisch |
| Server-Sicherung | Datenbank + Schlüssel verschlüsselt in jeden Speicherort, Wiederherstellung per Befehl |

---

## 2. Architektur

```
 Rechner (Agent) ──HTTPS, Zertifikat gepinnt──► Bastion-Server ──► Speicherort(e)
   Windows: VSS + BackupRead                     Weboberfläche, API      Chunks (dedupliziert,
   Linux:   LVM/btrfs-Snapshot                   Zeitplanung, Rollout    komprimiert, verschlüsselt)
                                                 SQLite-Datenbank        snapshots/*.json
                                                 PXE (optional)          server-backup/*.bsb
```

- **Verbindungsrichtung:** Die Agents bauen die Verbindung selbst auf (Long-Polling auf `/api/agent/poll`). Am Client muss **kein Port** geöffnet werden.
- **Datenformat:**
  - Dateien werden mit FastCDC in inhaltsabhängige Blöcke (Chunks) zerlegt.
  - Ein Chunk wird über seinen SHA-256 identifiziert, mit zstd komprimiert und mit AES-256-GCM verschlüsselt.
  - Gleiche Blöcke speichert ein Speicherort nur einmal, auch über alle Rechner hinweg.
- **Verzeichnisbaum:** Jedes Volume wird als JSONL-Baum mit allen Metadaten gespeichert, selbst wieder in Chunks.
- **Snapshot-Beschreibung:** Ein Backup ist eine JSON-Beschreibung (Rechner, Zeit, Partitionslayout, Volumes, Baum-Chunks). Sie liegt in der Datenbank **und** als Datei im Speicherort (`snapshots/<id>.json`).

### Warum dateibasierte Images?
Ein Sektor-Image passt nicht auf eine kleinere Festplatte. Bastion sichert deshalb jedes Volume aus dem Snapshot mit allen Metadaten, dazu das Partitionslayout:
- Windows: ACLs, alternative Datenströme, Reparse-Points, Hardlinks, 8.3-Namen, Attribute, Zeitstempel
- Linux: Besitzer, Rechte, xattrs, ACLs, Gerätedateien, Hardlinks

Beim Restore wird die Zielplatte neu partitioniert (auf die neue Größe skaliert), die Dateien werden eingespielt und das System wird bootfähig gemacht:
- Windows: `bcdboot`, Laufwerksbuchstaben in `MountedDevices`
- Linux: `grub-install`, initramfs, gleiche UUIDs

### Datenverzeichnis des Servers

| Pfad | Inhalt |
|---|---|
| `bastion.db` | SQLite-Datenbank (Rechner, Pläne, Aufträge, Backups, Benutzer, Einstellungen) |
| `keys\repo.key` | **Schlüssel aller Backups** |
| `keys\signing.key` | Signaturschlüssel für Agent-Updates |
| `keys\vault.key` | Schlüssel für gespeicherte Zugangsdaten |
| `tls\cert.pem`, `tls\key.pem` | Server-Zertifikat (Agents pinnen den Fingerabdruck) |
| `repo\` | Standard-Speicherort |
| `agents\<os>-<arch>\` | Agent-Programme für Verteilung und Updates |
| `rescue\` | `winpe.wim`, ISO, Linux-Kernel/initrd |
| `pxe\` | iPXE und wimboot |
| `snapshots\`, `cache\` | Kopien der Backup-Beschreibungen, Index für den Backup-Browser |
| `server.log` | Server-Protokoll |
| `initial-admin-password.txt` | Startpasswort des Benutzers `admin` |

---

## 3. Installation des Servers

### Bauen
Benötigt Go ≥ 1.23.
```powershell
.\build.ps1 -Version 0.3.0
.\build.ps1 -Version 0.3.0 -Publish C:\BastionData   # zusätzlich Agents für Verteilung/Updates bereitstellen
```
Ergebnis in `dist\<os>-<arch>\`: `bastion-server(.exe)`, `bastion-agent(.exe)`, `VERSION`.

### Einrichten
```powershell
# Windows (als Administrator)
.\bastion-server.exe -install-service -data C:\BastionData -listen :8443
```
```bash
# Linux
sudo ./bastion-server -install-service -data /var/lib/bastion-server -listen :8443
```

Danach:
1. Weboberfläche öffnen: `https://<server>:8443`. Benutzer `admin`, Passwort aus `initial-admin-password.txt`, danach ändern.
2. **Einstellungen → Server-Adresse für Agents** eintragen (z. B. `https://backup01.firma.local:8443`).
3. **Einstellungen → Server-Sicherung:** Passwort festlegen (siehe [Kapitel 11](#11-server-sicherung-und-notfall-wiederherstellung-des-servers)). **Nicht überspringen.**
4. **Speicherorte** anlegen, mindestens einen auf einem anderen Gerät (NAS).
5. Rettungssysteme bauen ([Kapitel 9](#9-rettungssystem-winpe--linux--pxe)).
6. Agents verteilen und Pläne anlegen.

**Eigenes Zertifikat:** `tls\cert.pem` und `key.pem` ersetzen und neu starten – am besten **vor** der Agent-Verteilung, weil verteilte Agents den alten Fingerabdruck pinnen.

**Reverse-Proxy:** zusätzlich `-http 127.0.0.1:8080` starten und den Proxy darauf zeigen lassen.

---

## 4. Agents verteilen

Seite **Agent-Verteilung**.

### Automatisch (Push)
1. **Zugangsdaten** anlegen: Windows (Domänen-Admin oder lokaler Admin) bzw. Linux (SSH mit Passwort oder Schlüssel).
2. **Rechner finden:** Active Directory (Domänencontroller, optional OU als Basis-DN) oder IP-Bereich (bis /20).
3. Rechner auswählen.
4. **„Vorab prüfen“** – zeigt je Rechner ✓/✗ für:
   - Namensauflösung, offene Ports (SMB 445, WinRM 5985, SSH 22)
   - SMB: Anmeldung, ADMIN$-Zugriff, Dienststeuerung
   - WinRM: Anmeldung, Adminrechte, **Verbindung vom Rechner zum Server**
   - SSH: Anmeldung, Agent-Programm für die Architektur, sudo, Verbindung zum Server
   
   Danach lassen sich direkt nur die bereiten Rechner installieren.
5. **„Agent installieren“.**

| Methode | Voraussetzung |
|---|---|
| SMB + Dienststeuerung | Server läuft unter Windows; Datei- und Druckerfreigabe auf dem Client |
| WinRM | `winrm quickconfig` auf dem Client |
| SSH | root oder sudo; bei Schlüsseln `NOPASSWD` |

**Remote-UAC:** Lokale Administratorkonten (außer dem eingebauten „Administrator“) haben über das Netz keine Adminrechte. Abhilfe: Domänenkonto verwenden oder auf dem Client einmalig:
```
reg add HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System /v LocalAccountTokenFilterPolicy /t REG_DWORD /d 1 /f
```

### Status im Verlauf

| Status | Bedeutung |
|---|---|
| geplant / läuft | in Arbeit |
| **wartet** | Rechner war nicht erreichbar (aus, nicht im Netz). Neuer Versuch alle 15 Min. für 24 Std. (einstellbar). „Jetzt versuchen“ startet sofort. |
| fertig | Agent installiert und am Server angemeldet |
| fehlgeschlagen | Fehler, der einen Eingriff braucht (Passwort, Rechte, Firewall zum Server). Die Meldung nennt die Ursache. |

Schlägt die Anmeldung des Agents fehl, diagnostiziert der Installer selbst: DNS, TCP-Verbindung zum Server, TLS/Proxy, Zertifikats-Fingerabdruck.

### Manuell, GPO, Intune
Unter „Manuell, per GPO oder Intune“ ein Token erzeugen. Die Seite zeigt fertige Befehle, z. B.:
```
bastion-agent.exe install --server https://backup01:8443 --token XXXX-XXXX-XXXX --fingerprint <sha256>
```

### Deinstallation
- Weboberfläche: Rechner → ⋯ → „Agent deinstallieren“
- Am Rechner: **Einstellungen → Apps → Installierte Apps → Bastion Backup Agent → Deinstallieren** oder `bastion-agent uninstall`

Der Agent meldet sich dabei beim Server ab. Der Rechner erscheint dann als **„deinstalliert“** (Filter in der Rechnerliste), nimmt an keinen Plänen mehr teil, seine Backups bleiben erhalten. Wird der Agent erneut verteilt, ist der Rechner mit seiner Historie wieder aktiv.

---

## 5. Speicherorte

Seite **Speicherorte**.

- **Lokal:** Laufwerk/Ordner des Servers (Ordnerauswahl, freier Platz wird angezeigt)
- **SMB:** Netzwerkfreigabe (NAS) mit Benutzer und Passwort

Jeder Plan wählt seinen Speicherort. Deduplizierung gilt **je Speicherort**.

Inhalt eines Speicherorts:
```
<Speicherort>\
  xx\xxxxxxxx…          Chunks (verschlüsselt)
  snapshots\*.json      Beschreibungen der Backups
  server-backup\*.bsb   Server-Sicherungen (siehe Kapitel 11)
```

**Verschieben:** Wird der Speicherort eines Plans geändert, bietet Bastion an, vorhandene Backups zu verschieben. Daten werden kopiert, im Ziel geprüft und erst dann umgestellt; den alten Platz gibt die nächste Bereinigung frei.

**Empfehlung:** Mindestens einen Speicherort auf einem **anderen Gerät** als dem Server betreiben.

---

## 6. Backup-Pläne

Seite **Backup-Pläne → Neuer Plan**.

| Feld | Hinweis |
|---|---|
| Art | **Image** (ganzer Rechner, bootfähig) oder **Dateien/Ordner** |
| Zeitplan | Voreinstellung oder Cron (`0 22 * * *` = täglich 22:00) |
| Aufbewahrung | letzte / tägliche / wöchentliche / monatliche |
| Speicherort | Ziel der Backups |
| Bandbreite | KB/s je Rechner, 0 = unbegrenzt |
| Ausschlüsse | Muster; bei Images werden Auslagerungsdatei, Ruhezustand, Temp, Update-Cache automatisch ausgelassen |
| Snapshot | für Datei-Backups abschaltbar; Images immer mit Snapshot |
| Ziele | einzelne Rechner, Gruppen oder alle |

- Ist ein Rechner zur Backup-Zeit aus, versucht der Server **Wake-on-LAN**; sonst läuft das Backup, sobald er online ist.
- „Gleichzeitige Backups“ (Einstellungen) begrenzt die Last auf dem Server.
- Ein einzelnes Backup lässt sich jederzeit über „Jetzt sichern“ starten; mit „Behalten“ wird ein Backup von der Aufbewahrung ausgenommen.

---

## 7. Wie ein Backup abläuft

1. Der Server teilt dem Agent den Auftrag beim nächsten Poll mit (inkl. vorherigem Backup als Basis).
2. **Snapshot**
   - **Windows:** VSS-Requester mit Writern, Backup-Typ *COPY*.
     - SQL Server, Exchange, Hyper-V, Active Directory usw. schreiben ihre Daten vor dem Snapshot konsistent weg → **anwendungskonsistent**.
     - *COPY* kürzt keine Transaktionsprotokolle; andere Backup-Programme (z. B. SQL-Log-Sicherungen) bleiben unbeeinflusst.
     - Das Auftragsprotokoll zeigt „VSS-Snapshot mit Writern (anwendungskonsistent)“.
     - Sind die Writer nicht nutzbar, fällt Bastion auf einen Snapshot ohne Writer zurück und schreibt eine **Warnung** (Datenbanken dann nur absturzkonsistent).
   - **Linux:** LVM- oder btrfs-Snapshot; ohne beides läuft das Backup ohne Snapshot mit Warnung.
3. **Lesen:** Unveränderte Dateien (Größe und Zeitstempel gleich) werden aus dem vorherigen Backup übernommen, ohne sie zu lesen. Nur geänderte Dateien werden gelesen und in Chunks zerlegt.
4. **Übertragen:** Der Agent fragt, welche Chunks dem Speicherort fehlen, und lädt nur diese hoch.
5. **Abschluss:** Snapshot-Beschreibung an den Server, Index für den Backup-Browser wird erzeugt, Snapshot wird freigegeben.

---

## 8. Wiederherstellung

### Dateien und Ordner
Backup öffnen → Dateien auswählen → **„Auswahl wiederherstellen“**.
- Ziel: Originalort oder anderer Ordner, auch auf einem anderen Rechner.
- Geöffnete Dateien ersetzt Bastion beim nächsten Neustart.
- Einzelne Dateien und Ordner (als ZIP) lassen sich direkt herunterladen.
- Suche über das ganze Backup: `rechnung`, `*.pdf`.

### Ganzes Daten-Volume
„Ganzes Volume wiederherstellen“ mit Option „vorher formatieren“: Das Volume wird im laufenden Betrieb ausgehängt, formatiert und zurückgespielt.

### Ganzes System (Image)
Rechner → Backup → **„Image wiederherstellen“**.

**Im laufenden Betrieb:**
1. Der Agent lädt WinPE, bindet Netzwerk- und Speichertreiber des Rechners ein und richtet einen **einmaligen** Start ein (Windows: BCD-Ramdisk + `bootsequence`, Linux: `grub-reboot`).
2. Der Agent speichert dabei die **aktuellen Netzwerkeinstellungen** (IP, Präfix, Gateway, DNS je Netzwerkkarte) im Auftrag.
3. Nach der Vorwarnzeit startet der Rechner neu in die Rettungsumgebung (läuft im RAM).
4. **Netzwerk in der Rettungsumgebung:**
   - Karten mit vorher fester IP bekommen diese sofort.
   - Sonst DHCP (bis 25 s), danach Rückfall auf die zuletzt bekannte Adresse.
   - Ist der Server trotzdem nicht erreichbar: Menü mit „Netzwerk einstellen“, „Erneut verbinden“, „Neustart in das bisherige System“. Die Festplatte ist bis dahin unverändert.
5. Partitionieren, Image einspielen, bootfähig machen, Neustart. Fortschritt live in der Weboberfläche.

**Mit USB-Stick, ISO oder PXE** (Rechner startet nicht mehr, neue Festplatte): siehe [Kapitel 9](#9-rettungssystem-winpe--linux--pxe).

**Andere Plattengröße:** Die Backup-Seite zeigt die Mindestgröße (belegte Daten + 10 % Reserve, mind. 1 GB, + Boot-Partitionen). Bei größeren Platten wächst die Systempartition mit. BIOS/MBR ↔ UEFI/GPT wird automatisch angepasst.

**Tipp:** Ist der Zielrechner ein Klon eines noch laufenden Systems, vor dem ersten Start das Netzwerk trennen (gleicher Name/IP).

---

## 9. Rettungssystem (WinPE / Linux / PXE)

### Erstellen

| | Befehl | Ergebnis |
|---|---|---|
| Windows | `rescue\winpe\build.ps1 -Server … -Fingerprint … -Agent dist\windows-amd64\bastion-agent.exe -Out <data>\rescue` | `winpe.wim`, `bastion-rescue-winpe.iso` |
| Linux | `sudo rescue/linux/build.sh --server … --fingerprint … --agent dist/linux-amd64/bastion-agent --out <data>/rescue` | Kernel, initrd, ISO |
| PXE | `rescue\pxe\fetch-ipxe.ps1 -Out <data>\pxe`, dann in den Einstellungen aktivieren | iPXE-Menü per proxyDHCP |

- WinPE braucht Windows ADK + WinPE-Add-on. Die Befehle mit eingesetzter Adresse und Fingerabdruck zeigt die Seite **Rettungssysteme**.
- **Nach jedem Agent-Update das WinPE neu bauen**, damit Rettungsprogramm und Agent zusammenpassen.
- Warnungen „Optionale WinPE-Pakete nicht hinzugefügt“ sind unkritisch.

### Bedienung
Vollbild-Oberfläche, Pfeiltasten, Enter, Esc. Tastatur Deutsch (umschaltbar auf US).

| Menüpunkt | Zweck |
|---|---|
| Backup auswählen | als Bastion-Administrator anmelden, Rechner und Image wählen |
| Wiederherstellungs-Code | 8-stelligen Code aus der Weboberfläche (Image wiederherstellen → USB/ISO/PXE) eingeben |
| Netzwerk einstellen | DHCP oder statisch (IP/Präfix, Gateway, DNS) |
| Server-Adresse ändern | falls der Server umgezogen ist |
| Tastatur umschalten | DE ↔ US |
| Beenden | Eingabeaufforderung zur Diagnose (`wpeutil reboot` zum Neustart) |

Vor dem Start: Zusammenfassung mit neuem Partitionsplan und rote Sicherheitsabfrage („JA“).

---

## 10. Restore-Prüfung

Beantwortet die Frage „Lässt sich dieses Backup im Ernstfall wirklich zurückspielen?“, ohne etwas zu schreiben.

Ablauf (auf dem Server, als Auftrag „Restore-Prüfung“):
1. Alle Verzeichnisbäume werden gelesen.
2. **Jeder** Datenblock wird gelesen, entschlüsselt und gegen seine Prüfsumme geprüft.
3. Bei Image-Backups zusätzlich:
   - Datenträger-Layout vorhanden (nötig für leere Festplatten)
   - Windows: `Windows\System32\config\SYSTEM`, `ntoskrnl.exe`, `winload.efi`/`winload.exe`
   - Linux: `/etc/fstab`, Kernel `/boot/vmlinuz*`

Ergebnis:
- Badge **„geprüft“** bzw. **„Prüfung fehlgeschlagen“** in der Backup-Liste, Details auf der Backup-Seite
- bei Fehlern E-Mail-Benachrichtigung (wie bei fehlgeschlagenen Backups)

Starten:
- manuell: Backup öffnen → **„Wiederherstellbarkeit prüfen“**
- automatisch: täglich um `verify_time` (Standard 04:30) wird das **neueste Backup jedes Rechners** geprüft, wenn dessen letzte Prüfung älter als `verify_days` (Standard 7) ist. `verify_days = 0` schaltet das ab.

Dauer: etwa 1,5 Minuten für ein 13-GB-Image (abhängig von Speicherort und CPU). Es läuft immer nur eine Prüfung gleichzeitig.

Die tägliche **Integritätsprüfung** (Einstellung `scrub_time`) prüft dagegen alle Chunks eines Speicherorts, unabhängig von einzelnen Backups.

---

## 11. Server-Sicherung und Notfall-Wiederherstellung des Servers

### Warum
Die Backups sind mit `keys\repo.key` verschlüsselt. Geht der Server verloren (Hardwaredefekt, Ransomware) und mit ihm Schlüssel und Datenbank, sind **alle Backups wertlos** – auch die auf dem NAS.

### Was gesichert wird
Eine Datei `bastion-server-JJJJMMTT-hhmmss.bsb` mit:
- Datenbank (konsistente Kopie im laufenden Betrieb)
- `repo.key`, `signing.key`, `vault.key`
- TLS-Zertifikat und -Schlüssel (damit die Agents dem neuen Server vertrauen)

Die Datei ist mit einem **eigenen Passwort** verschlüsselt (scrypt + AES-256-GCM).

### Einrichten
**Einstellungen → Server-Sicherung**:
1. Passwort festlegen (mind. 12 Zeichen). **Getrennt vom Server aufbewahren** (Passwort-Safe, Tresor). Ohne dieses Passwort ist die Sicherung nutzlos.
2. Uhrzeit wählen (Standard 02:15).
3. „Jetzt sichern“ für eine erste Sicherung.

Danach wird täglich in **jeden** Speicherort unter `server-backup\` geschrieben; die letzten 14 bleiben erhalten. Die Statuszeile warnt, wenn alle Kopien nur auf dem Server selbst liegen. „Herunterladen“ liefert eine aktuelle Sicherung zum Ablegen an einem weiteren Ort.

### Notfall: Server neu aufbauen
1. Neuen Server installieren, möglichst mit **derselben IP-Adresse bzw. demselben DNS-Namen**.
2. Speicherorte erreichbar machen (NAS-Freigabe, Laufwerke unter denselben Pfaden/Laufwerksbuchstaben).
3. `bastion-server.exe` kopieren und als Administrator ausführen:
   ```powershell
   bastion-server.exe restore -from "\\NAS\Backups\Bastion" -data C:\BastionData
   ```
   - `-from`: Ordner eines Speicherorts (die neueste Sicherung wird gewählt) oder eine `.bsb`-Datei
   - Passwort wird abgefragt (oder Umgebungsvariable `BASTION_RESTORE_PASSPHRASE`)
   - `-force`: vorhandene Daten überschreiben (alte Dateien werden in `*.before-restore-*` umbenannt)
   - Bei SMB-Freigaben vorher ggf. `net use \\NAS\Backups /user:…` ausführen
4. Dienst einrichten:
   ```powershell
   bastion-server.exe -install-service -data C:\BastionData
   ```
5. Beim ersten Start übernimmt der Server automatisch alle Backups, die **nach** der Server-Sicherung entstanden sind (aus `snapshots\*.json` der Speicherorte, nur wenn alle Datenblöcke vorhanden sind).
6. Agent-Programme bereitstellen (`build.ps1 -Publish`) und WinPE neu bauen.

Die Agents verbinden sich von selbst wieder, sobald der Server unter der alten Adresse erreichbar ist (gleicher Zertifikats-Fingerabdruck).

### Testen
Den Ablauf regelmäßig auf einer Test-VM durchspielen: `restore` in ein leeres Datenverzeichnis, Server starten (ggf. mit anderem `-listen`-Port), Backups im Browser öffnen.

---

## 12. Weboberfläche

| Seite | Inhalt |
|---|---|
| Übersicht | Kennzahlen, Backups der letzten 14 Tage, Belegung je Speicherort, Rechner ohne aktuelles Backup |
| Rechner | Liste mit Status, Filter (offline, ohne Backup, deinstalliert), Gruppen; Detail mit Datenträger, Backups, Aufträgen |
| Backup-Pläne | Pläne anlegen, ändern, sofort ausführen |
| Speicherorte | Ziele, Belegung, Verschieben |
| Aufträge | alle Aufträge mit Live-Fortschritt, Tempo, Restzeit, Protokoll, Abbrechen |
| Agent-Verteilung | Suche, Vorab-Prüfung, Installation, Verlauf, Tokens, Agent-Programme |
| Rettungssysteme | Status und Befehle für WinPE/Linux/PXE, ISO-Download |
| Einstellungen | Server-Sicherung, Allgemein, E-Mail, PXE, Benutzer, Wartung, Audit-Protokoll |

- Live-Aktualisierung über Server-Sent Events, kein Neuladen nötig.
- Hell/dunkel umschaltbar.
- Rollen: **Nur lesen**, **Operator** (sichern, Dateien wiederherstellen, prüfen), **Administrator** (alles).

---

## 13. Einstellungen (Referenz)

| Schlüssel | Standard | Bedeutung |
|---|---|---|
| `public_url` | automatisch | Adresse, unter der Agents den Server erreichen |
| `max_parallel_backups` | 10 | gleichzeitige Backups |
| `bandwidth_kbps` | 0 | Bandbreite je Rechner |
| `gc_time` | 03:30 | tägliche Bereinigung (nicht mehr benötigte Chunks löschen) |
| `scrub_time` | aus | tägliche Integritätsprüfung aller Chunks |
| `verify_time` | 04:30 | Uhrzeit der automatischen Restore-Prüfung |
| `verify_days` | 7 | Prüfabstand je Rechner, 0 = aus |
| `rollout_retry_minutes` | 15 | Abstand der Wiederholungen bei der Verteilung |
| `rollout_retry_hours` | 24 | Zeitraum der Wiederholungen, 0 = keine |
| `notify_*`, `smtp_*` | – | E-Mail-Benachrichtigung |
| `notify_stale_days` | 3 | Warnung bei Rechnern ohne Backup |
| `pxe_*` | aus | PXE-Dienste |
| Server-Sicherung | 02:15 | Passwort und Uhrzeit, eigene Karte in den Einstellungen |

---

## 14. Kommandozeile (Referenz)

### bastion-server
```
bastion-server [-data DIR] [-listen :8443] [-http 127.0.0.1:8080] [-pxe]
bastion-server -install-service -data DIR [-listen :8443]
bastion-server restore -from <Datei|Ordner> [-data DIR] [-force]
bastion-server -version
```

### bastion-agent
```
bastion-agent install --server URL --token TOKEN [--fingerprint SHA256] [--force]
bastion-agent enroll  --server URL --token TOKEN [--fingerprint SHA256]   nur anmelden, kein Dienst
bastion-agent uninstall
bastion-agent run          im Vordergrund (Diagnose)
bastion-agent status
bastion-agent rescue [--auto]
bastion-agent version
```

Agent-Dateien:

| | Windows | Linux |
|---|---|---|
| Programm | `C:\Program Files\Bastion\bastion-agent.exe` | `/opt/bastion/bastion-agent` |
| Konfiguration/Protokoll | `C:\ProgramData\Bastion\` (`agent.log`) | `/var/lib/bastion/` |
| Dienst | `BastionAgent` | `bastion-agent` (systemd) |

---

## 15. Netzwerk und Firewall

| Richtung | Port | Zweck |
|---|---|---|
| Clients → Server | TCP 8443 | Agents, Weboberfläche, Rettungssystem |
| Server → Clients | TCP 445 | Verteilung per SMB (nur bei Push) |
| Server → Clients | TCP 5985 | Verteilung per WinRM (nur bei Push) |
| Server → Clients | TCP 22 | Verteilung per SSH (nur bei Push) |
| Server → Clients | UDP 9 (Broadcast) | Wake-on-LAN |
| Clients → Server | UDP 67, 69, 4011, TCP 8080 | PXE (optional) |
| Server → DC | TCP 389/636 | AD-Suche (optional) |
| Server → NAS | TCP 445 | SMB-Speicherorte |

---

## 16. Sicherheit

- **Agents:** melden sich einmalig mit einem Token an, erhalten ein eigenes Geheimnis, prüfen das Server-Zertifikat per Fingerabdruck. Konfiguration nur für SYSTEM/Administratoren lesbar.
- **Updates:** mit Ed25519 signiert; der Agent prüft Signatur und Prüfsumme.
- **Rettungsumgebung:** einmaliges Token, 48 Stunden gültig, nur für ein Backup.
- **Repository:** AES-256-GCM je Chunk (Chunk-ID als Zusatzdaten), Manipulationen fallen beim Lesen auf.
- **Server-Sicherung:** scrypt-abgeleiteter Schlüssel + AES-256-GCM; Passwort verschlüsselt in der Datenbank (für die tägliche Sicherung).
- **Weboberfläche:** Argon2id-Passwörter, Brute-Force-Schutz, CSRF-Schutz, Rollen, Audit-Protokoll.
- **Zugangsdaten für die Verteilung:** verschlüsselt mit `vault.key`.
- Empfehlung: Server nicht in der Domäne betreiben bzw. mit eigenem Admin-Konto, NAS-Freigabe nur für das Bastion-Konto beschreibbar.

---

## 17. Wartung und Updates

**Server aktualisieren**
```powershell
.\build.ps1 -Version x.y.z -Publish C:\BastionData
Stop-Service BastionServer
Copy-Item .\dist\windows-amd64\bastion-server.exe 'C:\Program Files\BastionServer\' -Force
Start-Service BastionServer
```
Datenbank-Änderungen werden beim Start automatisch nachgezogen.

**Agents aktualisieren:** Rechner → „Alle aktualisieren“ (oder je Rechner im ⋯-Menü).

**WinPE neu bauen** nach Agent-Updates (Seite Rettungssysteme zeigt den Befehl).

**Regelmäßig:**
- Status der Server-Sicherung in den Einstellungen ansehen
- Badges der Restore-Prüfung in den Backup-Listen
- Belegung der Speicherorte (Übersicht)
- Notfall-Wiederherstellung des Servers einmal im Jahr testen

---

## 18. Fehlerbehebung

| Symptom | Ursache / Lösung |
|---|---|
| Verteilung: „kein Zugriff auf ADMIN$“ | Remote-UAC, siehe [Kapitel 4](#4-agents-verteilen) |
| Verteilung: „erreicht 172.16.50.10:8443 nicht“ | Firewall auf dem Server (eingehend TCP 8443) oder im Netz |
| Verteilung bleibt auf „wartet“ | Rechner aus/nicht erreichbar; läuft automatisch weiter, „Jetzt versuchen“ für sofort |
| Backup-Warnung „VSS-Writer nicht nutzbar“ | `vssadmin list writers` prüfen (Writer in Fehlerzustand → Dienst neu starten); Schattenkopie-Speicher prüfen (`vssadmin list shadowstorage`) |
| „es läuft bereits ein anderer Snapshot“ | anderes Backup-Programm gleichzeitig; Zeitplan verschieben |
| WinPE: keine IP-Adresse | Rettungssystem übernimmt die alte IP; sonst Menü „Netzwerk einstellen“. Keine Netzwerkkarte → Treiber fehlt (VM: e1000 statt virtio) |
| Image-Restore startet nicht in WinPE | Secure Boot / BitLocker-Abfrage am Rechner prüfen; Auftrag zeigt nach dem Neustart „Wiederherstellungsumgebung wurde nicht ausgeführt“ |
| Restore-Prüfung fehlgeschlagen: Datenblöcke fehlen | Speicherort beschädigt oder Dateien gelöscht; Integritätsprüfung laufen lassen, neues Backup erstellen |
| Agent dauerhaft offline | Dienst `BastionAgent` läuft? `C:\ProgramData\Bastion\agent.log` lesen |
| Server startet nicht | `<data>\server.log` lesen |

Protokolle:
- Server: `<data>\server.log`
- Agent: `C:\ProgramData\Bastion\agent.log`
- Rettungssystem: Protokoll des Auftrags in der Weboberfläche
- Verteilung per SMB: `C:\Windows\Temp\bastion-install.log` auf dem Client (wird nach Auswertung gelöscht)

---

## 19. Grenzen

- **Abweichende Hardware:** Treiber werden nicht in das wiederhergestellte Windows injiziert (kein „Universal Restore“).
- **VSS:** ein Snapshot je Volume; Datenbanken mit Daten und Logs auf verschiedenen Volumes sind je Volume konsistent, nicht volumeübergreifend.
- **WinRE-Partition** wird nicht mitgesichert (`reagentc` nach dem Restore).
- **EFS-Dateien** werden übersprungen; BitLocker-Volumes werden unverschlüsselt wiederhergestellt.
- **Linux:** keine erweiterten MBR-Partitionen, kein mdadm-RAID.
- **Speicherorte:** lokal und SMB; kein S3 / unveränderlicher Speicher.
- **Restore-Prüfung** prüft Daten und Startdateien, bootet das System aber nicht tatsächlich.
- Sehr große Umgebungen (> 200 Rechner, zweistellige TB) brauchen Packdateien und eine externe Datenbank.

---

## 20. Entwicklung

```powershell
go test ./...
go vet ./...
```

| Verzeichnis | Inhalt |
|---|---|
| `cmd/bastion-server`, `cmd/bastion-agent` | Programme |
| `internal/chunker` | FastCDC |
| `internal/repo` | Chunk-Speicher, Snapshot- und Baumformat |
| `internal/layout` | Partitionslayout und Skalierung |
| `internal/fsx` | Dateisystem mit allen Metadaten |
| `internal/platform` | VSS (COM mit Writern), LVM, btrfs, Layout-Erfassung |
| `internal/transfer` | Upload mit Deduplizierung, paralleler Download |
| `internal/agent` | Agent-Dienst, Backup, Restore, Image-Staging, Netzwerkdaten |
| `internal/rescue` | Rettungsprogramm (TUI, Netzwerk, Partitionierung) |
| `internal/tui` | Vollbild-Textoberfläche |
| `internal/server` | API, Weboberfläche, Zeitplanung, Rollout, Restore-Prüfung, Server-Sicherung, PXE |
| `rescue/` | Build-Skripte für WinPE, Linux, PXE |
