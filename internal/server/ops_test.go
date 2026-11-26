package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"bastion/internal/client"
	"bastion/internal/proto"
)

// The certificate of a domain controller is only accepted if it is pinned
// or trusted by the system; nothing is sent to an unknown server.
func TestLDAPCertificateCheck(t *testing.T) {
	ts := httptest.NewTLSServer(nil)
	defer ts.Close()
	fp := client.Fingerprint(ts.Certificate().Raw)
	addr := ts.Listener.Addr().String()
	dial := func(pins []string, insecure bool) (error, string) {
		cfg, seen := ldapTLSConfig("127.0.0.1", pins, insecure)
		c, err := tls.Dial("tcp", addr, cfg)
		if err == nil {
			c.Close()
		}
		return err, seen()
	}
	if err, seen := dial(nil, false); err == nil || seen != fp {
		t.Errorf("unbekanntes Zertifikat: Fehler=%v, gemeldeter Fingerabdruck %q", err, seen)
	}
	if err, _ := dial([]string{strings.Repeat("ab", 32)}, false); err == nil {
		t.Error("falscher Fingerabdruck wurde angenommen")
	}
	if err, _ := dial([]string{"SHA256:" + strings.ToUpper(fp)}, false); err != nil {
		t.Errorf("hinterlegter Fingerabdruck abgelehnt: %v", err)
	}
	if err, _ := dial(nil, true); err != nil {
		t.Errorf("ldap_insecure: %v", err)
	}
}

func TestJSONCharset(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, map[string]string{"a": "ä"})
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// Backups without a plan can be moved to another storage.
func TestMoveUnplanned(t *testing.T) {
	s := testServer(t)
	def := s.defaultStorage()
	dir := filepath.Join(t.TempDir(), "ziel")
	if err := probeWritable(dir); err != nil {
		t.Fatal(err)
	}
	res, _ := s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('Ziel','local',?,0,?)`, dir, now())
	target, _ := res.LastInsertId()
	res, _ = s.db.Exec(`INSERT INTO plans(name,kind,schedule,storage_id,created) VALUES('P','files','',0,?)`, now())
	planID, _ := res.LastInsertId()
	addSnapshot(t, s, "ohne-plan", 0, def, []byte("eins"))
	addSnapshot(t, s, "plan-geloescht", 999, def, []byte("zwei"))
	addSnapshot(t, s, "mit-plan", planID, def, []byte("drei"))

	addUser(s, "chef", "richtig-richtig")
	tok, _ := rescueLogin(t, s, "chef", "richtig-richtig")
	auth := map[string]string{"Authorization": "Bearer session:" + tok}
	var list []storageRow
	json.Unmarshal(do(s, "GET", "/api/storages", "", auth).Body.Bytes(), &list)
	if len(list) != 2 || list[0].Unplanned != 2 {
		t.Fatalf("Speicherorte: %+v", list)
	}
	if w := do(s, "POST", "/api/plans/0/move", `{}`, auth); w.Code != 400 {
		t.Fatalf("ohne Ziel: %d", w.Code)
	}
	w := do(s, "POST", "/api/plans/0/move", `{"to":`+itoa(target)+`}`, auth)
	if w.Code != 200 {
		t.Fatalf("Verschieben: %d %s", w.Code, w.Body)
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	deadline := time.Now().Add(20 * time.Second)
	for {
		var st, msg string
		s.db.QueryRow(`SELECT status,message FROM jobs WHERE id=?`, out.JobID).Scan(&st, &msg)
		if st == "success" {
			break
		}
		if st == "failed" || time.Now().After(deadline) {
			t.Fatalf("Auftrag: %s %s", st, msg)
		}
		time.Sleep(20 * time.Millisecond)
	}
	where := func(id string) (sid int64) {
		s.db.QueryRow(`SELECT storage_id FROM snapshots WHERE id=?`, id).Scan(&sid)
		return
	}
	if where("ohne-plan") != target || where("plan-geloescht") != target || where("mit-plan") != def {
		t.Errorf("Speicherorte nach dem Verschieben: %d %d %d", where("ohne-plan"), where("plan-geloescht"), where("mit-plan"))
	}
	dst, _ := s.storeByID(target)
	for _, id := range []string{"ohne-plan", "plan-geloescht"} {
		snap, err := s.loadSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := missingChunks(snap, dst); err != nil || m != 0 {
			t.Errorf("%s am Ziel: %d fehlen, %v", id, m, err)
		}
	}
}

// Machines that are gone do not get a job every night, and a job nobody
// fetched does not wait for ever.
func TestOfflineMachines(t *testing.T) {
	s := testServer(t)
	long := now() - 60*86400
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('da','da','x',?,?)`, now(), now())
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('weg','weg','x',?,?)`, long, long)
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('benannt','benannt','x',?,?)`, long, long)
	p := &plan{Targets: []string{"all", "agent:benannt"}}
	got := strings.Join(s.reachablePlanAgents(p), ",")
	if !strings.Contains(got, "da") || !strings.Contains(got, "benannt") || strings.Contains(got, "weg") {
		t.Errorf("Zeitplan erfasst: %s", got)
	}
	s.setSetting("skip_offline_days", "0")
	if n := len(s.reachablePlanAgents(p)); n != 3 {
		t.Errorf("skip_offline_days=0: %d Rechner", n)
	}

	ins := func(created int64, status string) int64 {
		res, _ := s.db.Exec(`INSERT INTO jobs(agent_id,type,status,created) VALUES('weg','backup',?,?)`, status, created)
		id, _ := res.LastInsertId()
		return id
	}
	old, fresh, running := ins(now()-30*3600, "queued"), ins(now()-3600, "queued"), ins(now()-30*3600, "running")
	s.expireQueuedBackups()
	st := func(id int64) (v string) {
		s.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, id).Scan(&v)
		return
	}
	if st(old) != "failed" || st(fresh) != "queued" || st(running) != "running" {
		t.Errorf("Aufträge: alt=%s frisch=%s laufend=%s", st(old), st(fresh), st(running))
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// "bastion-server update" publishes the agent packages, copies the support
// files and swaps the program, keeping the old one.
func TestApplyUpdate(t *testing.T) {
	own := runtime.GOOS + "-" + runtime.GOARCH
	rel, data, app := t.TempDir(), t.TempDir(), t.TempDir()
	// the platforms of a release, plus this one if it is none of them
	plats := []string{"windows-amd64", "linux-amd64", "linux-arm64"}
	if !slices.Contains(plats, own) {
		plats = append(plats, own)
	}
	for _, p := range plats {
		writeFile(t, filepath.Join(rel, p, binName("bastion-agent", p)), "agent neu "+p)
		writeFile(t, filepath.Join(rel, p, binName("bastion-server", p)), "server neu "+p)
		writeFile(t, filepath.Join(rel, p, "VERSION"), "9.9.9\r\n")
	}
	writeFile(t, filepath.Join(rel, "support", "wiki.md"), "handbuch neu")
	writeFile(t, filepath.Join(rel, "support", "rescue", "winpe", "build.ps1"), "skript neu")
	exe := filepath.Join(app, binName("bastion-server", own))
	writeFile(t, exe, "server alt")
	writeFile(t, filepath.Join(data, "agents", own, binName("bastion-agent", own)), "agent alt")

	var log bytes.Buffer
	plan, err := ApplyUpdate(rel, data, exe, &log)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if !plan.Server || plan.Version != "9.9.9" || len(plan.Agents) != len(plats) {
		t.Errorf("Plan: %+v", plan)
	}
	if got := readFile(exe); got != "server neu "+own {
		t.Errorf("Programm: %q", got)
	}
	if got := readFile(exe + ".old"); got != "server alt" {
		t.Errorf("altes Programm: %q", got)
	}
	for _, p := range plats {
		if got := readFile(filepath.Join(data, "agents", p, binName("bastion-agent", p))); got != "agent neu "+p {
			t.Errorf("Agent %s: %q", p, got)
		}
		if got := readFile(filepath.Join(data, "agents", p, "VERSION")); strings.TrimSpace(got) != "9.9.9" {
			t.Errorf("Agent-Version %s: %q", p, got)
		}
	}
	if readFile(filepath.Join(app, "wiki.md")) != "handbuch neu" || readFile(filepath.Join(app, "rescue", "winpe", "build.ps1")) != "skript neu" {
		t.Error("Begleitdateien nicht kopiert")
	}
	// a second run with the installed program as source changes nothing
	if _, err := ApplyUpdate(rel, data, exe, &log); err != nil {
		t.Fatal(err)
	}
	// a folder that is no release is refused before anything is touched
	if _, err := ApplyUpdate(t.TempDir(), data, exe, &log); err == nil {
		t.Error("leerer Ordner wurde als Release angenommen")
	}
	if got := readFile(exe); got != "server neu "+own {
		t.Errorf("Programm nach abgelehntem Update: %q", got)
	}
}

// The rescue system is built as a server job; only a complete result
// replaces the existing files.
func TestRescueBuildJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("verwendet ein Shell-Skript als Ersatz für den Build")
	}
	s := testServer(t)
	app := t.TempDir()
	oldApp, oldCmd := appDir, rescueBuildCommand
	defer func() { appDir, rescueBuildCommand = oldApp, oldCmd }()
	appDir = func() string { return app }

	if _, err := s.startRescueBuild("test"); err == nil {
		t.Fatal("Build ohne Skript gestartet")
	}
	writeFile(t, filepath.Join(app, "rescue", "winpe", "build.ps1"), "x")
	if _, err := s.startRescueBuild("test"); err == nil {
		t.Fatal("Build ohne Agent-Paket gestartet")
	}
	writeFile(t, s.path("agents", "windows-amd64", "bastion-agent.exe"), "agent")
	writeFile(t, s.path("agents", "windows-amd64", "VERSION"), "1.2.3\r\n")
	writeFile(t, s.path("rescue", "winpe.wim"), "altes wim")
	os.MkdirAll(s.path("rescue", "drivers"), 0o755)

	wait := func(id int64) (string, string) {
		deadline := time.Now().Add(20 * time.Second)
		for {
			var st, logText string
			s.db.QueryRow(`SELECT status,log FROM jobs WHERE id=?`, id).Scan(&st, &logText)
			if st != "running" || time.Now().After(deadline) {
				return st, logText
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var gotArgs []string
	script := func(body string) {
		rescueBuildCommand = func(script, serverURL, fp, agentExe, out, drivers string) (*exec.Cmd, error) {
			gotArgs = []string{script, serverURL, fp, agentExe, out, drivers}
			return exec.Command("/bin/sh", "-c", body, "sh", out), nil
		}
	}
	// a failing build leaves the old files alone
	script(`echo "Kopiere WinPE"; echo "oscdimg 50% complete" >&2; exit 3`)
	id, err := s.startRescueBuild("test")
	if err != nil {
		t.Fatal(err)
	}
	if st, logText := wait(id); st != "failed" || !strings.Contains(logText, "Kopiere WinPE") || !strings.Contains(logText, "50% complete") {
		t.Fatalf("fehlgeschlagener Build: %s\n%s", st, logText)
	}
	if readFile(s.path("rescue", "winpe.wim")) != "altes wim" {
		t.Error("alte Datei nach fehlgeschlagenem Build verändert")
	}
	// success without output is not accepted either
	script(`exit 0`)
	id, _ = s.startRescueBuild("test")
	if st, _ := wait(id); st != "failed" {
		t.Fatalf("Build ohne Ergebnis: %s", st)
	}
	// a complete build is published
	script(`head -c 2000000 /dev/zero > "$1/winpe.wim"; head -c 3000000 /dev/zero > "$1/bastion-rescue-winpe.iso"; echo Fertig`)
	id, _ = s.startRescueBuild("test")
	if st, logText := wait(id); st != "success" {
		t.Fatalf("Build: %s\n%s", st, logText)
	}
	if fi, err := os.Stat(s.path("rescue", "winpe.wim")); err != nil || fi.Size() != 2000000 {
		t.Error("winpe.wim nicht veröffentlicht")
	}
	if fi, err := os.Stat(s.path("rescue", "bastion-rescue-winpe.iso")); err != nil || fi.Size() != 3000000 {
		t.Error("ISO nicht veröffentlicht")
	}
	if gotArgs[1] != s.PublicURL() || gotArgs[2] != s.fp || gotArgs[5] != s.path("rescue", "drivers") {
		t.Errorf("Aufruf: %v", gotArgs)
	}
	if _, err := os.Stat(gotArgs[4]); err == nil {
		t.Error("Arbeitsordner nicht aufgeräumt")
	}
	var built struct{ Agent string }
	json.Unmarshal([]byte(s.setting("rescue_built", "{}")), &built)
	if built.Agent != "1.2.3" {
		t.Errorf("rescue_built: %+v", built)
	}
}

// After an update the server does what the administrator configured, once.
func TestAfterVersionChange(t *testing.T) {
	s := testServer(t)
	s.db.Exec(`INSERT INTO agents(id,hostname,os,arch,version,secret_hash,enrolled) VALUES('a1','pc','linux','amd64','0.0.1','x',?)`, now())
	writeFile(t, s.path("agents", "linux-amd64", "bastion-agent"), "agent")
	writeFile(t, s.path("agents", "linux-amd64", "VERSION"), proto.Version)
	s.scanAgentBinaries()
	updates := func() (n int) {
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='update'`).Scan(&n)
		return
	}
	s.setSetting("auto_update_agents", "1")
	s.afterVersionChange() // first start: only records the version
	if updates() != 0 || s.setting("server_version", "") != proto.Version {
		t.Fatalf("erster Start: %d Updates, Version %q", updates(), s.setting("server_version", ""))
	}
	s.setSetting("server_version", "0.0.1")
	s.afterVersionChange()
	if updates() != 1 {
		t.Fatalf("nach Versionswechsel: %d Updates", updates())
	}
	s.afterVersionChange() // same version again: nothing
	if updates() != 1 {
		t.Fatalf("zweiter Start: %d Updates", updates())
	}
	s.setSetting("server_version", "0.0.1")
	s.setSetting("auto_update_agents", "")
	s.db.Exec(`DELETE FROM jobs`)
	s.afterVersionChange()
	if updates() != 0 {
		t.Fatal("Agents ohne Einstellung aktualisiert")
	}
}
