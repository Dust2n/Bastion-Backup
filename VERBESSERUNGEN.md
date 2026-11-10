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

## Offen: Sicherheit

- **Speicherverbrauch ohne Anmeldung.** `logRequests` in `internal/server/server.go` liest jede Anfrage vor der Authentifizierung bis 17 MB in den RAM. Außer `ReadHeaderTimeout` gibt es keine Zeitlimits (`ReadTimeout`, Body-Timeout). Viele parallele Verbindungen erschöpfen den Speicher. Vorschlag: Puffern nur für authentifizierte Anfragen, kleineres Limit für Nicht-Chunk-Routen, Timeouts für den Body.
- **Anmeldebremse.** `rateLimiter.allow` zählt nur pro IP und zählt auch erfolgreiche Anmeldungen. Hinter einem Reverse-Proxy (`-http`) teilen sich alle Nutzer die Proxy-IP; zehn Versuche sperren alle aus, und Brute-Force über viele IPs wird nicht gebremst. Vorschlag: zusätzlich pro Benutzername zählen, nur Fehlversuche zählen, bei Proxy-Betrieb `X-Forwarded-For` von einer konfigurierten Proxy-Adresse auswerten.
- **Sessions nach Passwortwechsel.** `handlePassword` beendet bestehende Sessions nicht. Das Startpasswort steht außerdem im Server-Log (`ensureAdmin`).
- **Linux-Installationsbefehl ohne Prüfung.** `installCommands` erzeugt `curl -fsSk …` und startet die geladene Datei als root. Die Windows-Variante prüft den Fingerabdruck. Vorschlag: `curl --pinnedpubkey` oder SHA-256 der Datei prüfen (`sha256sum -c`).
- **AD-Suche ohne Zertifikatsprüfung.** `tlsInsecure` in `internal/server/rollout.go` wird für StartTLS zum Domänencontroller benutzt; das Passwort des Domänen-Admins ist für einen Angreifer im Netz abfangbar. Vorschlag: Zertifikat prüfen, optional eigene CA oder Fingerabdruck hinterlegen.
- **Chunk-IDs sind reines SHA-256.** Jeder Agent darf jeden Chunk seines Speicherorts per ID laden. Wer den Hash einer Datei kennt, kann ihr Vorhandensein prüfen und sie herunterladen. Ein schlüsselabhängiger Hash (HMAC) wäre sauberer, ist aber ein Formatwechsel mit Migration.
- **Abhängigkeiten.** `golang.org/x/crypto` v0.31.0 und `golang.org/x/net` v0.33.0 sind alt. In `go.mod` ist alles als `// indirect` markiert. Vorschlag: `govulncheck ./...`, Abhängigkeiten aktualisieren, `go mod tidy`.

## Offen: Stabilität

- **Absturzgefahr bei Datenbankfehlern.** An mehreren Stellen steht `rows, _ := s.db.Query(...)`, danach wird `rows` ohne Prüfung benutzt (z. B. `getPlan`, `planAgents`, `handleTokens`, `handleUsers`, `handleAudit`). Liefert die Datenbank einen Fehler, ist `rows` nil und der Aufruf panict; im Zeitplaner-Goroutine beendet das den ganzen Server.
- **Ignorierte Fehler.** Viele `s.db.Exec`-Aufrufe und das Schreiben der Snapshot-JSON-Dateien in `handlePostSnapshot` prüfen den Fehler nicht. Bei gesperrter Datenbank gehen Zustandsänderungen still verloren.
- **Online-Status während langer Aufträge.** `last_seen` wird nur beim Polling gesetzt. Während eines Backups pollt der Agent nicht; nach zwei Minuten dürfte der Rechner in der Oberfläche als offline erscheinen. Vorschlag: `last_seen` auch in `handleProgress` setzen.
- **Datenbankwachstum.** Aufträge mit bis zu 200 KB Protokoll und das Audit-Log werden nie gelöscht. Die Server-Sicherung (`buildServerBackup`) baut die ganze Datenbank im RAM zusammen. Vorschlag: Aufbewahrung für Aufträge und Audit, Server-Sicherung als Stream.

## Offen: Performance

- **Kompression auf einem Kern.** Der globale zstd-Encoder in `internal/repo/store.go` hat `WithEncoderConcurrency(1)`; die vier Upload-Worker teilen sich ihn. Vorschlag: Encoder pro Worker oder `sync.Pool`.
- **Kleine Abfragepakete.** Der Uploader fragt fehlende Chunks in Paketen von 16 ab (`internal/transfer/upload.go`), der Server erlaubt 1000. Jede Prüfung ist ein `os.Stat`, auf SMB-Speicherorten teuer. Vorschlag: größere Pakete, serverseitiger Index der vorhandenen Chunks im Speicher.
- **Doppeltes Puffern.** Jeder Chunk-Upload wird erst in `logRequests` und dann im Handler vollständig gelesen.
- **Einsträngige Prüfläufe.** Integritätsprüfung (`runScrub`) und Restore-Prüfung laufen mit einem Worker.
- **Eine Datei pro Chunk.** Skaliert schlecht bei Millionen Chunks und auf Netzwerkfreigaben; Packdateien nennt auch das README als offenen Punkt.
