# Verbesserungen: Sicherheit, Stabilität, Performance

Ergebnis einer Code-Durchsicht von Bastion Backup 0.3.0 (November 2026). Die Punkte stammen aus dem Lesen des Codes; reproduziert wurden nur die behobenen, und zwar mit den unten genannten Tests.

Funktions- und Dateiangaben beziehen sich auf den Stand nach Commit `1971b32`.

## Behoben (Commit `1971b32`)

| # | Problem | Lösung | Test |
|---|---|---|---|
| 1 | **Agent-Übernahme per Installations-Token.** Wer ein gültiges Token hatte, konnte sich mit Hostname und MAC eines vorhandenen Rechners anmelden und bekam dessen ID samt Zugriff auf dessen Backups. Der MAC-Vergleich war ein Teilstring-Vergleich, das Herunterzählen der Token-Nutzungen nicht atomar. | Jede Anmeldung bekommt eine neue ID. Ein Vorgänger mit gleichem Namen und exakt gleicher MAC wird nur in `agents.replaces` vermerkt; ein Administrator übernimmt den Verlauf per „Verlauf übernehmen“ (`handleAgentMerge`). Token-Nutzungen werden atomar gezählt. | `TestEnrollNoTakeover`, `TestEnrollTokenUses` |
| 2 | **Wiederherstellung schreibt außerhalb des Zielordners.** Pfade aus dem Verzeichnisbaum wurden ungeprüft an das Ziel gehängt (`../`, absolute Pfade, Einträge unterhalb eines zuvor wiederhergestellten Symlinks). Ausgeführt als root bzw. SYSTEM. | `internal/fsx/guard.go` prüft jeden Pfad und Hardlink-Ziel mit `filepath.IsLocal` und lehnt Einträge unterhalb wiederhergestellter Symlinks/Junctions ab. Gilt für Agent und Rettungsumgebung. ZIP-Downloads überspringen solche Namen. | `TestPathGuard` |
| 3 | **Ein kompromittierter Client konnte seine Backup-Historie löschen.** Zeitstempel und Plan-ID eines Snapshots kamen vom Agent, ein laufender Auftrag war nicht nötig; gefälschte Snapshots verdrängten über die Aufbewahrung die echten. | Snapshots werden nur für den laufenden Backup-Auftrag angenommen (eins pro Auftrag, `snapshots.job_id`). Plan, Art und Zeit kommen vom Server. | `TestSnapshotBoundToJob` |
| 4 | **Chunks ohne `fsync`.** Nach einem Stromausfall konnte eine leere oder halbe Datei unter dem endgültigen Namen liegen; `Has()` meldete sie als vorhanden, sie wurde nie neu hochgeladen. | `writeDurable` in `internal/repo/store.go` schreibt mit `fsync` vor dem Umbenennen. Zu kurze Chunk-Dateien gelten als fehlend. | `TestTornChunkIsMissing` |
| 5 | **Defekte Chunks heilten nie.** Die Integritätsprüfung meldete sie nur; inkrementelle Backups übernahmen die Chunks unveränderter Dateien ungeprüft. | Defekte Chunks kommen nach `quarantine/`. Betroffene Backups werden als beschädigt markiert (`snapshots.damaged`) und nicht mehr als Basis für inkrementelle Backups verwendet, damit die Daten neu hochgeladen werden. Backups, die während der Prüfung laufen, werden beim Eintreffen nachgeprüft (`checkComplete`). Die Bereinigung bricht an bekannt beschädigten Backups nicht mehr ab. | `TestScrubQuarantine` |

Verhaltensänderung: Ein neu installierter Rechner startet ohne alten Verlauf, bis ein Administrator auf „Verlauf übernehmen“ klickt. Bis dahin ist sein erstes Backup vollständig.

## Behoben (nach 0.3.1)

| Problem | Lösung | Test |
|---|---|---|
| **Hängende Aufträge.** Ein Auftrag blieb `running`, bis der Agent wieder pollte. Fiel der Rechner mitten im Backup weg, wartete die Bereinigung jede Nacht zwei Stunden und brach ab; der Auftrag zählte dauerhaft gegen `max_parallel_backups`. | `jobs.updated` hält die letzte Meldung fest. `failStalledJobs` schließt Aufträge ohne Meldung nach `job_stall_minutes` (Standard 30). Meldet sich der Agent doch noch, bekommt er „Abbruch“ zurück. | `TestStalledJobFails` |
| **Verpasste Zeitpläne entfielen.** Der Zeitplaner begann bei jedem Start mit der aktuellen Uhrzeit. War der Server zur geplanten Zeit aus, fielen Backup, Bereinigung, Restore-Prüfung und Server-Sicherung ersatzlos aus. | Der Zeitplaner speichert seinen Stand (`sched_last`) und holt nach dem Start alles einmal nach, was in der Pause fällig war (höchstens 7 Tage zurück). | `TestScheduleCatchUp`, `TestCrossed` |
| **Server sicherte sein eigenes Repository.** Ein Image-Backup des Backup-Servers erfasst alle Laufwerke, auch die mit einem Speicherort. Die Chunks sind verschlüsselt, lassen sich also nicht deduplizieren; das Backup wuchs mit jedem Lauf um das Repository. | Der Server erkennt den Agent auf dem eigenen Rechner (Hostname und IP-Adresse) und gibt ihm die Pfade der lokalen Speicherorte und des Index-Caches als `exclude_paths` mit. Datenbank, Schlüssel und Zertifikat bleiben im Backup. Braucht einen Agent ab dieser Version; ältere ignorieren das Feld. | `TestOwnRepositoryExcluded`, `TestSameMachine`, `TestExcludeBelow` |
| **Ein unvollständiges Backup blockierte die Bereinigung.** Beim Annehmen eines Backups wurden nur die Chunks des Verzeichnisbaums geprüft. Fehlte später ein Chunk, brach die Bereinigung für den ganzen Speicherort mit „nichts gelöscht“ ab, jede Nacht. Auslösbar durch einen fehlerhaften oder kompromittierten Agent. | `handlePostSnapshot` prüft alle referenzierten Chunks und lehnt unvollständige Backups ab; das Vorgänger-Backup gilt dann als beschädigt, das nächste Backup liest alles neu. Die Bereinigung markiert ein Backup mit fehlendem Baum als beschädigt und läuft weiter, Volume für Volume. Bei jedem anderen Fehler (Speicherort nicht erreichbar, Lesefehler, falscher Schlüssel) löscht sie weiterhin nichts. | `TestIncompleteSnapshotRefused`, `TestCleanupSurvivesIncompleteBackup` |
| **Online-Status während langer Aufträge.** | `handleProgress` setzt `last_seen`. | `TestStalledJobFails` |

| **Anmeldebremse.** Zählte auch erfolgreiche Anmeldungen und nur pro Adresse; zehn Anmeldungen in fünf Minuten sperrten alle hinter derselben Adresse aus. Die Tabelle wuchs mit jeder Adresse. | Es zählen nur Fehlversuche: 10 pro Adresse, 30 pro Benutzername in fünf Minuten. Abgelaufene Einträge werden entfernt. | `TestLoginLimit` |
| **Sessions nach Passwortwechsel.** | Der Wechsel beendet alle anderen Sessions des Benutzers. Das Startpasswort steht nicht mehr im Server-Log, nur noch in der Datei. | `TestPasswordChangeEndsSessions` |
| **Speicherverbrauch ohne Anmeldung, doppeltes Puffern.** Jede Anfrage wurde vor der Anmeldung bis 17 MB gepuffert, Chunk-Uploads danach ein zweites Mal gelesen. | Chunk-Uploads liest nur noch der Handler, nach der Anmeldung. Alle anderen Anfragen sind auf 4 MB begrenzt. | `TestRequestLimits` |
| **Uploads ohne Auftrag.** Ein Agent konnte jederzeit Chunks hochladen und den Speicherort füllen. | Chunks werden nur angenommen, solange für den Rechner ein Backup läuft. | `TestRequestLimits` |
| **Linux-Installationsbefehl ohne Prüfung.** | `curl --pinnedpubkey` mit dem Schlüssel des Server-Zertifikats. | `TestLinuxInstallPinsKey` |
| **Absturz durch Hintergrundaufgaben.** Eine Panic in Bereinigung, Prüflauf, Server-Sicherung oder Zeitplaner beendete den Server. | Hintergrundaufgaben laufen über `Server.bg` mit `recover`; der Zeitplaner fängt Fehler je Durchlauf ab. `getPlan`, `planAgents` und `runGC` prüfen ihre Abfragen. | `TestBackgroundPanic` |
| **Datenbankwachstum.** | Abgeschlossene Aufträge werden nach `keep_jobs_days` (180), Audit-Einträge nach `keep_audit_days` (730) gelöscht; der Auftrag eines vorhandenen Backups bleibt. | `TestPruneHistory` |
| **Kompression auf einem Kern, kleine Abfragepakete, einsträngiger Prüflauf.** | zstd nutzt alle Kerne; fehlende Chunks werden in Paketen bis 256 Stück bzw. 32 MB abgefragt; die Integritätsprüfung arbeitet mit bis zu acht Workern. | `TestBackupRestoreEndToEnd`, `TestScrubQuarantine` |
| **Keine Überwachung von außen.** | `GET /api/monitor` mit eigenem Token (`monitor_token`): Stand je Rechner, Speicherorte, beschädigte Backups, Server-Sicherung, Liste der Probleme. | `TestMonitor` |
| **Keine Tests für Agent und Übertragung.** | `TestBackupRestoreEndToEnd` startet Server und Agent über TLS: Backup, inkrementelles Backup, Wiederherstellung, Vergleich aller Dateien. | |
| **Abhängigkeiten.** | `go mod tidy`; `golang.org/x/crypto` 0.59, `x/net` 0.62, `x/sys` 0.50. **Braucht jetzt Go 1.26.** CI unter `.github/workflows/ci.yml` (vet, Tests, Race-Erkennung, govulncheck). | |

## Offen: Sicherheit

- **Anmeldebremse hinter Reverse-Proxy.** Mit `-http` sehen alle Anfragen wie von der Proxy-Adresse aus. Vorschlag: `X-Forwarded-For` von einer konfigurierten Proxy-Adresse auswerten.
- **AD-Suche ohne Zertifikatsprüfung.** `tlsInsecure` in `internal/server/rollout.go` wird für StartTLS zum Domänencontroller benutzt; das Passwort des Domänen-Admins ist für einen Angreifer im Netz abfangbar. Vorschlag: Zertifikat prüfen, optional eigene CA oder Fingerabdruck hinterlegen.
- **Chunk-IDs sind reines SHA-256.** Jeder Agent darf jeden Chunk seines Speicherorts per ID laden. Wer den Hash einer Datei kennt, kann ihr Vorhandensein prüfen und sie herunterladen. Ein schlüsselabhängiger Hash (HMAC) wäre sauberer, ist aber ein Formatwechsel mit Migration.

## Offen: Stabilität

- **Ungeprüfte Abfragen in Handlern.** An mehreren Stellen steht `rows, _ := s.db.Query(...)`, danach wird `rows` ohne Prüfung benutzt (z. B. `getPlan`, `planAgents`, `handleTokens`, `handleUsers`, `handleAudit`). Liefert die Datenbank einen Fehler, ist `rows` nil und der Aufruf panict; im Zeitplaner-Goroutine beendet das den ganzen Server.
- **Ignorierte Fehler.** Viele `s.db.Exec`-Aufrufe und das Schreiben der Snapshot-JSON-Dateien in `handlePostSnapshot` prüfen den Fehler nicht. Bei gesperrter Datenbank gehen Zustandsänderungen still verloren.

## Offen: Performance

- **Einsträngige Restore-Prüfung.** `runVerify` liest mit einem Worker.
- **Eine Datei pro Chunk.** Skaliert schlecht bei Millionen Chunks und auf Netzwerkfreigaben; Packdateien nennt auch das README als offenen Punkt.

## Offen: größere Bauabschnitte

- **Packdateien** statt einer Datei pro Chunk (Formatwechsel mit Migration).
- **Replikation** in einen zweiten Speicherort als geplanter Auftrag.
- **Zwei-Faktor-Anmeldung**, **Sperrfrist beim Löschen** von Backups, **Schutz von `repo.key`** (Passwort, DPAPI/TPM).
- **Server-Sicherung als Stream**, Index der vorhandenen Chunks im Speicher, Vorgänger-Baum im Agent im Gleichschritt lesen statt komplett laden.
- **Code-Signatur** für Agent und Server.
