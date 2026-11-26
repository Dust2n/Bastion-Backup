package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"bastion/internal/proto"
)

func TestValidSetting(t *testing.T) {
	ok := map[string][]string{
		"max_parallel_backups": {"", "1", "10"},
		"bandwidth_kbps":       {"0", "500"},
		"gc_time":              {"03:30", "3:30", "00:00", "23:59"},
		"public_url":           {"https://192.168.137.120:8443", "https://backup01.firma.local:8443/", "http://127.0.0.1:8080"},
		"smtp_port":            {"25", "587"},
		"monitor_token":        {"", "0123456789abcdef"},
		"auto_update_agents":   {"0", "1"},
		"pxe_ip":               {"10.0.0.5"},
	}
	bad := map[string][]string{
		"max_parallel_backups": {"0", "-1", "zehn"},
		"bandwidth_kbps":       {"-5"},
		"keep_jobs_days":       {"1.5"},
		"gc_time":              {"3.30", "24:00", "03:60", "03:5", "abends"},
		"public_url":           {"ubuntu2604:8443", "192.168.137.120", "https://", "https://x:8443/pfad", "https://x:99999", "ftp://x"},
		"smtp_port":            {"0", "70000"},
		"monitor_token":        {"kurz"},
		"auto_rescue_build":    {"ja", "2"},
		"pxe_ip":               {"10.0.0"},
	}
	for k, vs := range ok {
		for _, v := range vs {
			if err := validSetting(k, v); err != nil {
				t.Errorf("%s=%q abgelehnt: %v", k, v, err)
			}
		}
	}
	for k, vs := range bad {
		for _, v := range vs {
			if validSetting(k, v) == nil {
				t.Errorf("%s=%q angenommen", k, v)
			}
		}
	}
}

// The form saves every field; an empty one means the default, not "off".
func TestEmptySettingIsDefault(t *testing.T) {
	s := testServer(t)
	s.setSetting("gc_time", "")
	if got := s.setting("gc_time", "03:30"); got != "03:30" {
		t.Fatalf("leere Bereinigungszeit: %q", got)
	}
	// stored before the form checked it: 0 would hold back every backup
	s.setSetting("max_parallel_backups", "0")
	if n := s.maxParallelBackups(); n != 10 {
		t.Fatalf("Gleichzeitige Backups bei 0: %d", n)
	}
}

func TestSettingsSaveRefusesInvalid(t *testing.T) {
	s := testServer(t)
	save := func(v map[string]string) int {
		b, _ := json.Marshal(v)
		rec := httptest.NewRecorder()
		s.handleSettingsSave(rec, httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(b)), &user{Name: "admin", Role: "admin"})
		if rec.Code != 200 && !strings.Contains(rec.Body.String(), "Gleichzeitige Backups") {
			t.Errorf("Meldung ohne Feldname: %s", rec.Body.String())
		}
		return rec.Code
	}
	if code := save(map[string]string{"max_parallel_backups": "0", "gc_time": "04:00"}); code != 400 {
		t.Fatalf("0 gleichzeitige Backups gespeichert: %d", code)
	}
	if v := s.setting("gc_time", ""); v != "" {
		t.Fatalf("abgelehntes Formular hat trotzdem gespeichert: gc_time=%q", v)
	}
	if code := save(map[string]string{"max_parallel_backups": "", "gc_time": "04:00"}); code != 200 {
		t.Fatalf("gültige Werte abgelehnt: %d", code)
	}
	if v := s.setting("gc_time", ""); v != "04:00" {
		t.Fatalf("gc_time=%q", v)
	}
}

// A queued backup says why it waits instead of showing nothing.
func TestQueuedJobSaysWhy(t *testing.T) {
	s := testServer(t)
	s.db.Exec(`INSERT INTO agents(id,hostname,last_seen) VALUES('a1','pc1',?),('a2','pc2',?)`, now(), now()-3600)
	s.setSetting("max_parallel_backups", "1")
	s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,started) VALUES('a2',0,'backup','{}','running',?,?)`, now(), now())

	id, err := s.enqueue("a1", 0, proto.JobBackup, proto.BackupParams{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if job, err := s.dispatch("a1"); err != nil || job != nil {
		t.Fatalf("Auftrag trotz Limit herausgegeben: %v %v", job, err)
	}
	var msg, status string
	s.db.QueryRow(`SELECT message,status FROM jobs WHERE id=?`, id).Scan(&msg, &status)
	if status != "queued" || !strings.Contains(msg, "Gleichzeitige Backups") {
		t.Fatalf("status=%s Meldung=%q", status, msg)
	}

	// limit free again: the job starts and the waiting note goes away
	s.db.Exec(`UPDATE jobs SET status='success' WHERE agent_id='a2'`)
	if job, err := s.dispatch("a1"); err != nil || job == nil {
		t.Fatalf("Auftrag nicht herausgegeben: %v", err)
	}
	s.db.QueryRow(`SELECT message,status FROM jobs WHERE id=?`, id).Scan(&msg, &status)
	if status != "running" || msg != "" {
		t.Fatalf("status=%s Meldung=%q", status, msg)
	}

	// a machine that is offline: the job says so right away
	id2, _ := s.enqueue("a2", 0, proto.JobBackup, proto.BackupParams{}, "test")
	s.db.QueryRow(`SELECT message FROM jobs WHERE id=?`, id2).Scan(&msg)
	if !strings.Contains(msg, "online") {
		t.Fatalf("offline-Rechner: %q", msg)
	}
}

func TestSetupWarnings(t *testing.T) {
	s := testServer(t)
	s.storeByID(0) // standard storage inside the data directory
	w := strings.Join(s.setupWarnings(), "\n")
	for _, want := range []string{"Server-Adresse", "derselben Platte", "Server-Sicherung"} {
		if !strings.Contains(w, want) {
			t.Errorf("Hinweis %q fehlt:\n%s", want, w)
		}
	}
	s.setSetting("public_url", "https://192.168.1.10:8443")
	s.setSetting("dr_passphrase", "x")
	w = strings.Join(s.setupWarnings(), "\n")
	if strings.Contains(w, "Server-Adresse") || strings.Contains(w, "Server-Sicherung") {
		t.Errorf("erledigte Hinweise noch da:\n%s", w)
	}
}

func TestReachError(t *testing.T) {
	s := testServer(t)
	if err := s.reachError("pc1", "ubuntu2604", "8443", "dns"); !strings.Contains(err.Error(), "Server-Adresse für Agents") {
		t.Errorf("ohne Server-Adresse: %v", err)
	}
	if err := s.reachError("pc1", "10.0.0.1", "8443", "timeout"); !strings.Contains(err.Error(), "Firewall") || !strings.Contains(err.Error(), "Zeitüberschreitung") {
		t.Errorf("Zeitüberschreitung: %v", err)
	}
}
