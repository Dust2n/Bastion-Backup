# Bastion Backup

Selbst gehostete, zentral verwaltete Backup-Lösung für Windows- und Linux-Rechner.

Ausführliches Handbuch (Betrieb, Notfall, Referenz): [wiki.md](wiki.md)

- **Zentrale Weboberfläche:** Rechner, Backup-Pläne, Zeitpläne, Aufträge, Wiederherstellung, Agent-Verteilung
- **Kleiner Agent:** ein Programm (ca. 10 MB), läuft als Windows-Dienst bzw. systemd-Dienst
- **Backups im laufenden Betrieb:** über Snapshots (Windows: VSS mit Writern, anwendungskonsistent; Linux: LVM oder btrfs)
- **Restore-Prüfung:** Backups werden regelmäßig vollständig gelesen und auf Startfähigkeit geprüft
- **Server-Sicherung:** Datenbank und Schlüssel täglich verschlüsselt in jeden Speicherort; neuer Server per `bastion-server restore`
- **Vollwertige Image-Backups:** bootfähig; das Wiederherstellen auf **kleinere Festplatten** klappt, sobald die belegten Daten passen
- **Image-Wiederherstellung per Klick:**
  1. Der Rechner startet selbst in eine schlanke Rettungsumgebung (WinPE / Linux).
  2. Dort spielt er das Image ein.
  3. Danach startet er neu.
  - Alternativ per USB-Stick, ISO oder PXE mit einem Code aus der Weboberfläche.
- **Datei-Wiederherstellung:**
  - im laufenden Betrieb; geöffnete Dateien werden beim Neustart ersetzt
  - Download einzelner Dateien oder ganzer Ordner als ZIP
- **Zentrale Agent-Verteilung:**
  - Active-Directory-Suche oder IP-Scan
  - Push per SMB (wie PsExec), WinRM oder SSH
  - Installations-Tokens für GPO und Intune
  - signierte Agent-Updates aus der Weboberfläche
- **Speicher:**
  - Deduplizierung über alle Rechner hinweg (gleiche Windows-Dateien werden nur einmal gespeichert)
  - zstd-Kompression
  - AES-256-GCM-Verschlüsselung
  - Aufbewahrungsregeln, Bereinigung, Integritätsprüfung
- **Benachrichtigungen per E-Mail:** bei Fehlern und bei Rechnern ohne aktuelles Backup

## Aufbau

```
 Rechner (Agent) ──HTTPS (Zertifikat gepinnt)──►  Bastion-Server ──► Repository (Chunks, dedupliziert, verschlüsselt)
   Windows: VSS + BackupRead                        Web-UI, API, Zeitplanung      SQLite-Datenbank
   Linux:   LVM/btrfs-Snapshot                      Rollout, PXE (optional)
```

Die Agents bauen die Verbindung selbst auf (Long-Polling). Am Client müssen keine Ports geöffnet werden.

### Warum ein dateibasiertes Image?
Ein Sektor-Image lässt sich nicht auf eine kleinere Festplatte schreiben. Bastion sichert stattdessen jedes Volume aus dem Snapshot **mit allen Metadaten**:
- Windows: Sicherheitsbeschreibungen/ACLs, alternative Datenströme, Reparse-Points, Hardlinks, 8.3-Namen, Attribute, Zeitstempel
- Linux: Besitzer, Rechte, xattrs, ACLs, Gerätedateien, Hardlinks

Dazu kommt das Partitionslayout. Bei der Wiederherstellung partitioniert Bastion die Zielplatte neu und skaliert dabei auf die neue Größe. Anschließend spielt es die Dateien ein und macht das System bootfähig:
- Windows: `bcdboot` und Laufwerksbuchstaben in `MountedDevices`
- Linux: `grub-install`, initramfs, gleiche Dateisystem-UUIDs

## Installation

### 1. Bauen
Benötigt Go ≥ 1.23.

```powershell
.\build.ps1 -Version 0.1.0
```

Ergebnis in `dist\<os>-<arch>\`: `bastion-server(.exe)`, `bastion-agent(.exe)` und `VERSION`.

### 2. Server einrichten (Windows oder Linux)

```powershell
# Windows (als Administrator)
.\bastion-server.exe -install-service -data C:\BastionData -listen :8443
# Agent-Programme für Verteilung und Updates bereitstellen:
.\build.ps1 -Version 0.1.0 -Publish C:\BastionData
```

```bash
# Linux
sudo ./bastion-server -install-service -data /var/lib/bastion-server -listen :8443
```

- Beim ersten Start legt der Server den Benutzer `admin` an. Das Passwort steht in `<data>\initial-admin-password.txt`.
- Weboberfläche: `https://<server>:8443`. Das selbstsignierte Zertifikat wird angezeigt; Agents prüfen es über den Fingerabdruck.
- Unter **Einstellungen** die Server-Adresse eintragen, unter der die Rechner den Server erreichen (z. B. `https://backup01.firma.local:8443`).
- **Wichtig:** Den Ordner `<data>\keys` sicher aufbewahren. Ohne `repo.key` sind die Backups nicht lesbar. Alternativ den Schlüssel per Umgebungsvariable `BASTION_REPO_KEY` (Base64) übergeben und nicht auf dem Server ablegen.
- Firewall: TCP 8443 eingehend. Für PXE zusätzlich UDP 67, 69, 4011 und TCP 8080.
- Eigenes Zertifikat: `<data>\tls\cert.pem` und `key.pem` ersetzen und den Server neu starten. Neue Agents erhalten den neuen Fingerabdruck über den Installationsbefehl; bereits verteilte Agents pinnen den alten Fingerabdruck, deshalb das Zertifikat am besten vor der Verteilung tauschen.
- Hinter einem Reverse-Proxy (z. B. für ein öffentliches Zertifikat der Weboberfläche): zusätzlich `-http 127.0.0.1:8080` starten und den Proxy darauf zeigen lassen.

### 3. Agents verteilen
Unter **Agent-Verteilung** gibt es drei Wege:

**Automatisch:**
1. Zugangsdaten hinterlegen (Domänen-Admin bzw. SSH).
2. Rechner per AD-Suche oder IP-Scan finden.
3. Rechner auswählen und „Agent installieren“ klicken.
   - Windows: SMB + Dienststeuerung (nur wenn der Server unter Windows läuft) oder WinRM
   - Linux: SSH (Passwort mit sudo oder Schlüssel)

**Token:** Für GPO-Startskripte, Intune oder manuelle Installation wird ein Befehl erzeugt:

```
bastion-agent.exe install --server https://backup01:8443 --token XXXX-XXXX-XXXX --fingerprint <sha256>
```

**Updates:** Neue Agent-Version mit `build.ps1 -Publish` bereitstellen und unter „Rechner“ auf „Alle aktualisieren“ klicken. Die Updates sind mit dem Server-Schlüssel signiert.

Für SMB-Push mit **lokalen** Administratorkonten muss auf den Clients `LocalAccountTokenFilterPolicy=1` gesetzt sein (Remote-UAC). Domänenkonten benötigen das nicht.

### 4. Backup-Pläne
**Backup-Pläne → Neuer Plan:**
- Art: **Image** (ganzer Rechner) oder **Dateien/Ordner**
- Zeitplan: Voreinstellung oder Cron-Ausdruck
- Aufbewahrung: letzte, tägliche, wöchentliche und monatliche Backups
- Bandbreite, Ausschlüsse
- Zielrechner: einzelne Rechner, Gruppen oder alle

Weitere Einstellungen:
- Bei Image-Backups lässt Bastion `pagefile.sys`, `hiberfil.sys`, `swapfile.sys`, `System Volume Information`, Temp-Ordner und den Windows-Update-Cache automatisch aus.
- Ist ein Rechner zum Backup-Zeitpunkt aus, versucht der Server **Wake-on-LAN**. Andernfalls läuft das Backup, sobald der Rechner online ist.
- Wie viele Backups gleichzeitig laufen, regelt die Einstellung „Gleichzeitige Backups“ (Standard 10).

### 5. Speicherorte
Unter **Speicherorte** werden Ziele angelegt:
- lokales Laufwerk bzw. Ordner, mit Ordnerauswahl und Anzeige des freien Platzes
- Netzwerkfreigabe (SMB/NAS) mit Benutzer und Passwort

Jeder Backup-Plan wählt seinen Speicherort, z. B. Server-Images auf D:, Arbeitsplätze auf das NAS. Wird der Speicherort eines Plans geändert, bietet Bastion an, die **vorhandenen Backups zu verschieben**:
1. Die Daten werden kopiert und im Ziel geprüft.
2. Erst dann stellt Bastion je Backup um.
3. Den alten Platz gibt die nächste Bereinigung frei.

Alle Speicherorte sind verschlüsselt. Dedupliziert wird je Speicherort. Jeder Speicherort enthält auch die Beschreibungen seiner Backups (`snapshots/*.json`) und ist damit eigenständig.

## Weboberfläche
- Live-Aktualisierung ohne Neuladen (Server-Sent Events): Fortschritt mit Tempo und Restzeit, Rechner online/offline, Verteilung
- Übersicht mit Diagramm der letzten 14 Tage und Belegung je Speicherort
- Hell/dunkel umschaltbar
- Der Backup-Browser nutzt einen Index, der nach jedem Backup erzeugt wird:
  - Ordner öffnen sich in Millisekunden, auch bei Hunderttausenden Dateien.
  - Große Ordner werden seitenweise geladen.
  - Die Suche (`rechnung`, `*.pdf`) läuft über das ganze Backup.

## Wiederherstellung

### Dateien und Ordner
Ein Backup öffnen, Dateien auswählen und „Auswahl wiederherstellen“ klicken. Ziel ist der ursprüngliche Ort oder ein anderer Ordner, auch auf einem anderen Rechner.
- Dateien, die gerade geöffnet sind, ersetzt Bastion beim nächsten Neustart.
- Einzelne Dateien und ganze Ordner (als ZIP) lassen sich direkt im Browser herunterladen.

### Ganzes Daten-Volume
„Ganzes Volume wiederherstellen“ mit der Option „vorher formatieren“: Das Volume wird im laufenden Betrieb ausgehängt, neu formatiert und zurückgespielt.

### Ganzes System (Image)
Rechner öffnen → Backup → **„Image zurückspielen“**.

**Im laufenden Betrieb:**
1. Der Agent lädt die WinPE-Umgebung herunter und bindet die Netzwerk- und Speichertreiber des Rechners ein.
2. Er legt die Umgebung lokal ab und richtet einen **einmaligen** Start daraus ein: unter Windows per BCD-Ramdisk und `bootsequence`, unter Linux per `grub-reboot`.
3. Nach der Vorwarnzeit startet der Rechner neu.
4. Die Rettungsumgebung läuft vollständig im RAM. Sie partitioniert die Festplatte passend, spielt das Image ein, macht das System bootfähig und startet neu.
5. Fortschritt und Protokoll sind live in der Weboberfläche sichtbar.

Bricht etwas ab, **bevor** die Festplatte verändert wurde (z. B. Server nicht erreichbar), startet der Rechner wieder ins alte System, und der Auftrag wird als fehlgeschlagen markiert.

**Mit USB-Stick, ISO oder PXE** (Rechner startet nicht mehr oder hat eine neue Festplatte):
1. Den Modus „USB / ISO / PXE“ wählen. Die Weboberfläche zeigt einen 8-stelligen Code.
2. Den Rechner vom Rettungsmedium starten und den Code eingeben.

**Kleinere Festplatte:**
- Die Seite eines Backups zeigt, wie groß die Zielplatte mindestens sein muss: belegte Daten + 10 % (mind. 1 GB) Reserve + Boot-Partitionen.
- Ist die Platte größer als das Original, wächst die Systempartition entsprechend mit.
- Firmware-Wechsel werden automatisch berücksichtigt: Ein altes BIOS/MBR-System wird auf UEFI-Hardware als GPT mit EFI-Partition angelegt und umgekehrt.

### Rettungssystem (ISO/USB/PXE)
Farbige Vollbild-Oberfläche, Bedienung mit Pfeiltasten, Enter und Esc:
- **Backup auswählen:** als Bastion-Administrator anmelden, dann Rechner und Image-Backup wählen; alternativ den Code aus der Weboberfläche eingeben.
- **Netzwerk:** DHCP oder statische Adresse mit Gateway und DNS. Ohne DHCP öffnet sich das Netzwerkmenü automatisch.
- **Statusleiste:** IP-Adresse, Erreichbarkeit des Servers, Tastaturlayout, Uhrzeit.
- **Tastatur:** Deutsch ist voreingestellt, Umschalten auf US im Menü.
- **Vor dem Start:** Zusammenfassung mit dem neuen Partitionsplan und roter Sicherheitsabfrage.
- **Während der Wiederherstellung:** Fortschrittsbalken mit Tempo und Restzeit, dazu ein Protokoll.
- Ist die Eingabe umgeleitet (Skripte, serielle Konsole), arbeitet das Programm zeilenweise.

## Rettungssysteme erstellen

| | Befehl | Ergebnis |
|---|---|---|
| Windows | `rescue\winpe\build.ps1 -Server … -Fingerprint … -Agent dist\windows-amd64\bastion-agent.exe -Out <data>\rescue` | `winpe.wim`, `bastion-rescue-winpe.iso` |
| Linux | `sudo rescue/linux/build.sh --server … --fingerprint … --agent dist/linux-amd64/bastion-agent --out <data>/rescue` | `linux-vmlinuz`, `linux-initrd.img`, `bastion-rescue-linux.iso` |
| PXE | `rescue\pxe\fetch-ipxe.ps1 -Out <data>\pxe`, dann PXE in den Einstellungen aktivieren | iPXE-Menü über proxyDHCP |

Voraussetzungen:
- **WinPE:** [Windows ADK + WinPE-Add-on](https://learn.microsoft.com/windows-hardware/get-started/adk-install)
- **Linux:** ein Debian- oder Ubuntu-System mit `debootstrap`, `cpio` und `zstd`

Die genauen Befehle mit Ihrer Server-Adresse und Ihrem Fingerabdruck zeigt die Seite „Rettungssysteme“ in der Weboberfläche.

## Sicherheit
- **Agents:**
  - melden sich einmalig mit einem Token an und erhalten ein eigenes Geheimnis
  - prüfen das Server-Zertifikat per SHA-256-Fingerabdruck
  - schützen ihre Konfiguration so, dass nur SYSTEM und Administratoren sie lesen können
- **Rettungsumgebungen:** erhalten nur ein einmaliges, 48 Stunden gültiges Token für genau ein Backup.
- **Repository:** verschlüsselt (AES-256-GCM, Chunk-ID als Zusatzdaten). Eine Manipulation fällt beim Lesen und bei der Integritätsprüfung auf.
- **Weboberfläche:**
  - Rollen „Nur lesen“, „Operator“ und „Administrator“
  - Argon2id-Passwörter, Schutz vor Anmelde-Brute-Force
  - CSRF-Schutz und Audit-Protokoll
- **Gespeicherte Zugangsdaten:** für die Verteilung verschlüsselt abgelegt (`keys\vault.key`)

## Grenzen (aktueller Stand)
- Wiederherstellung auf **abweichende Hardware** mit fremden Speicher-Controllern: Treiber werden nicht in das wiederhergestellte Windows injiziert.
- Die **Windows-Recovery-Partition (WinRE)** wird nicht mitgesichert. Nach einer Image-Wiederherstellung kann WinRE bei Bedarf mit `reagentc` neu eingerichtet werden.
- **EFS-verschlüsselte Dateien** werden übersprungen und im Protokoll gemeldet. BitLocker-Volumes werden entschlüsselt gesichert und unverschlüsselt wiederhergestellt.
- **Linux:**
  - MBR-Datenträger mit mehr als 4 Partitionen (erweiterte Partitionen) werden bei der Image-Wiederherstellung nicht unterstützt.
  - Software-RAID (mdadm) wird nicht nachgebaut.
  - Ohne LVM- oder btrfs-Snapshot läuft die Sicherung ohne Snapshot; Bastion gibt dazu eine Warnung aus.
- Bei sehr vielen Rechnern (> 200) oder Repositorys im zweistelligen TB-Bereich sollten Packdateien und eine externe Datenbank ergänzt werden.

## Entwicklung

```powershell
go test ./...
go vet ./...
```

| Verzeichnis | Inhalt |
|---|---|
| `internal/chunker` | FastCDC (inhaltsabhängige Blöcke) |
| `internal/repo` | Chunk-Speicher, Snapshot- und Verzeichnisbaum-Format |
| `internal/layout` | Partitionslayout und Skalierung auf andere Plattengrößen |
| `internal/fsx` | Dateisystem lesen/schreiben mit allen Metadaten |
| `internal/platform` | VSS, LVM, btrfs, Layout-Erfassung |
| `internal/transfer` | Upload mit Deduplizierung, paralleler Download |
| `internal/agent` | Agent-Dienst, Backup, Wiederherstellung, Image-Staging |
| `internal/rescue` | Programm in der Rettungsumgebung |
| `internal/server` | API, Weboberfläche, Zeitplanung, Rollout, PXE |
