package server

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Rules for the values of the settings form. A value that the server
// would silently misread (0 parallel backups, "3.30" as a time, a URL
// without https://) is refused when it is saved, with the label the form
// shows. Empty always means "the default".

var settingLabels = map[string]string{
	"public_url":            "Server-Adresse für Agents",
	"max_parallel_backups":  "Gleichzeitige Backups",
	"bandwidth_kbps":        "Bandbreite pro Rechner",
	"job_stall_minutes":     "Auftrag abbrechen ohne Meldung nach",
	"gc_time":               "Tägliche Bereinigung um",
	"keep_jobs_days":        "Aufträge aufbewahren",
	"keep_audit_days":       "Audit-Protokoll aufbewahren",
	"scrub_time":            "Tägliche Integritätsprüfung um",
	"rollout_retry_minutes": "Agent-Verteilung: neuer Versuch alle",
	"rollout_retry_hours":   "Agent-Verteilung: Versuche über",
	"verify_time":           "Restore-Prüfung um",
	"verify_days":           "Restore-Prüfung alle",
	"skip_offline_days":     "Zeitplan: Rechner überspringen nach",
	"queue_expire_hours":    "Nicht abgeholtes Backup verwerfen nach",
	"auto_update_agents":    "Nach Server-Update Agents aktualisieren",
	"auto_rescue_build":     "Nach Server-Update Rettungssystem neu bauen",
	"monitor_token":         "Monitoring-Token",
	"smtp_port":             "SMTP-Port",
	"notify_stale_days":     "Warnen nach Tagen ohne Backup",
	"pxe_ip":                "IP-Adresse für PXE",
	"ldap_insecure":         "ldap_insecure",
}

// minValue of the whole-number settings; 0 switches most of them off.
var minValue = map[string]int{
	"max_parallel_backups":  1,
	"bandwidth_kbps":        0,
	"job_stall_minutes":     0,
	"keep_jobs_days":        0,
	"keep_audit_days":       0,
	"rollout_retry_minutes": 0,
	"rollout_retry_hours":   0,
	"verify_days":           0,
	"skip_offline_days":     0,
	"queue_expire_hours":    0,
	"notify_stale_days":     0,
	"smtp_port":             1,
}

// validSetting checks one value of the settings form.
func validSetting(key, v string) error {
	if v == "" {
		return nil
	}
	label := settingLabels[key]
	if label == "" {
		label = key
	}
	if lo, ok := minValue[key]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %q ist keine ganze Zahl", label, v)
		}
		if n < lo {
			if lo == 1 {
				return fmt.Errorf("%s: mindestens 1 (leer = Standard)", label)
			}
			return fmt.Errorf("%s: darf nicht negativ sein", label)
		}
		if key == "smtp_port" && n > 65535 {
			return fmt.Errorf("%s: höchstens 65535", label)
		}
		return nil
	}
	switch key {
	case "gc_time", "scrub_time", "verify_time":
		if _, _, err := parseClock(v); err != nil {
			return fmt.Errorf("%s: %v", label, err)
		}
	case "public_url":
		return validPublicURL(v)
	case "auto_update_agents", "auto_rescue_build", "ldap_insecure":
		if v != "0" && v != "1" {
			return fmt.Errorf("%s: 0 oder 1 (leer = Standard)", label)
		}
	case "monitor_token":
		if len(v) < 16 {
			return fmt.Errorf("%s: mindestens 16 Zeichen (leer = aus)", label)
		}
	case "pxe_ip":
		if net.ParseIP(v) == nil {
			return fmt.Errorf("%s: %q ist keine IP-Adresse", label, v)
		}
	}
	return nil
}

// parseClock reads a time of day as HH:MM.
func parseClock(v string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(v, ":")
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if !ok || err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(ms) != 2 {
		return 0, 0, fmt.Errorf("%q ist keine Uhrzeit (HH:MM, z. B. 03:30)", v)
	}
	return h, m, nil
}

// validPublicURL: the agents connect to this address and download from
// <address>/dl/..., so it must be a bare http(s)://host[:port].
func validPublicURL(v string) error {
	const label = "Server-Adresse für Agents"
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s: vollständig angeben, z. B. https://backup01.firma.local:8443", label)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: nur Protokoll, Name und Port, ohne Pfad (z. B. https://backup01.firma.local:8443)", label)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%s: Port %q ungültig", label, p)
		}
	}
	return nil
}

// maxParallelBackups is the limit for running backups. A value below 1
// (saved before the form checked it) would hold back every backup.
func (s *Server) maxParallelBackups() int {
	if n := s.settingInt("max_parallel_backups", 10); n >= 1 {
		return n
	}
	return 10
}

// setupWarnings are mistakes in the setup that do not show up as errors
// but bite later: agents that can not find the server, backups on the
// server's own disk, no server backup. The overview shows them.
func (s *Server) setupWarnings() []string {
	var out []string
	if s.setting("public_url", "") == "" {
		host, _ := os.Hostname()
		out = append(out, fmt.Sprintf("Die „Server-Adresse für Agents“ ist nicht eingetragen. Die Rechner verbinden sich deshalb mit dem Rechnernamen %q, den sie oft nicht auflösen können. Unter Einstellungen mit IP-Adresse oder DNS-Namen eintragen.", host))
	}
	rows, err := s.db.Query(`SELECT kind,path FROM storages`)
	if err == nil {
		n, local := 0, 0
		for rows.Next() {
			var kind, path string
			rows.Scan(&kind, &path)
			n++
			if kind != "smb" && sameDisk(path, s.opt.DataDir) {
				local++
			}
		}
		rows.Close()
		if n > 0 && local == n {
			out = append(out, "Alle Speicherorte liegen auf derselben Platte wie der Server. Fällt sie aus, sind Server und Backups zugleich verloren. Einen Speicherort auf einer zweiten Platte oder einem NAS anlegen.")
		}
	}
	// schedules run in the server's time zone; a server left on UTC starts
	// a 22:00 backup at 23:00 or 24:00 German time
	if name, off := time.Now().Zone(); name == "UTC" && off == 0 {
		hint := "die Zeitzone in Windows einstellen und den Dienst neu starten"
		if runtime.GOOS == "linux" {
			hint = "sudo timedatectl set-timezone Europe/Berlin, danach sudo systemctl restart bastion-server"
		}
		out = append(out, "Die Uhr des Servers steht auf UTC. Zeitpläne laufen in Serverzeit, ein Backup um 22:00 startet in Deutschland also erst um 23:00 bzw. 24:00 Uhr. Zeitzone einstellen: "+hint+".")
	}
	if s.setting("dr_passphrase", "") == "" {
		out = append(out, "Die Server-Sicherung ist nicht eingerichtet. Ohne sie lassen sich die Backups nach dem Verlust des Servers nicht wiederherstellen (Schlüssel, Datenbank). Unter Einstellungen → Server-Sicherung ein Passwort festlegen.")
	}
	return out
}
