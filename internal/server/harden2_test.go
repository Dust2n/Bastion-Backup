package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"bastion/internal/repo"
)

func do(s *Server, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "10.9.9.9:1000"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func addUser(s *Server, name, pw string) {
	s.db.Exec(`INSERT INTO users(name,pw,role,created) VALUES(?,?,'admin',?)`, name, hashPassword(pw), now())
}

func rescueLogin(t *testing.T, s *Server, name, pw string) (string, int) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"Name": name, "Password": pw})
	w := do(s, "POST", "/api/rescue/login", string(b), nil)
	var out struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Token, w.Code
}

// Successful logins never lock anybody out; wrong passwords do, per
// address, and the table of attempts empties itself again.
func TestLoginLimit(t *testing.T) {
	s := testServer(t)
	s.login = newRateLimiter(3, time.Minute) // keep the test fast (argon2 + delay per failure)
	addUser(s, "chef", "richtig-richtig")
	for i := 0; i < 8; i++ {
		if _, code := rescueLogin(t, s, "chef", "richtig-richtig"); code != 200 {
			t.Fatalf("%d. erfolgreiche Anmeldung: %d", i+1, code)
		}
	}
	for i := 0; i < 3; i++ {
		if _, code := rescueLogin(t, s, "chef", "falsch"); code != 401 {
			t.Fatalf("falsches Passwort: %d", code)
		}
	}
	if _, code := rescueLogin(t, s, "chef", "richtig-richtig"); code != 429 {
		t.Fatalf("nach drei Fehlversuchen: %d, erwartet 429", code)
	}
	// the window passes
	s.login.mu.Lock()
	for k, h := range s.login.hits {
		for i := range h {
			h[i] = h[i].Add(-2 * time.Minute)
		}
		s.login.hits[k] = h
	}
	s.login.mu.Unlock()
	if _, code := rescueLogin(t, s, "chef", "richtig-richtig"); code != 200 {
		t.Fatalf("nach Ablauf: %d", code)
	}
	s.login.sweep()
	if n := len(s.login.hits); n != 0 {
		t.Errorf("%d Einträge bleiben in der Tabelle", n)
	}
}

// Changing the password ends every other session of the user.
func TestPasswordChangeEndsSessions(t *testing.T) {
	s := testServer(t)
	addUser(s, "chef", "altes-passwort")
	other, _ := rescueLogin(t, s, "chef", "altes-passwort")
	mine, _ := rescueLogin(t, s, "chef", "altes-passwort")
	auth := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer session:" + tok} }
	if w := do(s, "GET", "/api/me", "", auth(other)); w.Code != 200 {
		t.Fatalf("Sitzung vor dem Wechsel: %d", w.Code)
	}
	if w := do(s, "POST", "/api/password", `{"Old":"altes-passwort","New":"neues-passwort-1"}`, auth(mine)); w.Code != 200 {
		t.Fatalf("Passwortwechsel: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/me", "", auth(other)); w.Code != 401 {
		t.Errorf("fremde Sitzung nach dem Wechsel: %d, erwartet 401", w.Code)
	}
	if w := do(s, "GET", "/api/me", "", auth(mine)); w.Code != 200 {
		t.Errorf("eigene Sitzung nach dem Wechsel: %d", w.Code)
	}
}

// Without a login nobody can make the server buffer a large body, and
// chunks are only accepted while a backup of that machine is running.
func TestRequestLimits(t *testing.T) {
	s := testServer(t)
	big := strings.Repeat("x", maxJSONBody+1)
	if w := do(s, "POST", "/api/login", big, nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("zu große Anfrage: %d", w.Code)
	}

	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled) VALUES('a1','pc1',?,?)`, sha("geheim"), now())
	data := []byte("inhalt eines chunks")
	id := repo.Hash(data)
	put := func() int {
		r := httptest.NewRequest("PUT", "/api/agent/chunks/"+id.String(), bytes.NewReader(repo.Compress(data)))
		r.Header.Set("Authorization", "Bearer agent:a1:geheim")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if code := put(); code != http.StatusConflict {
		t.Errorf("Chunk ohne laufendes Backup: %d, erwartet 409", code)
	}
	s.db.Exec(`INSERT INTO jobs(agent_id,type,status,created,started,updated) VALUES('a1','backup','running',?,?,?)`, now(), now(), now())
	if code := put(); code != http.StatusNoContent {
		t.Errorf("Chunk während des Backups: %d", code)
	}
	st, _ := s.storeByID(0)
	if !st.Has(id) {
		t.Error("Chunk nicht gespeichert")
	}
}

func TestMonitor(t *testing.T) {
	s := testServer(t)
	if w := do(s, "GET", "/api/monitor", "", nil); w.Code != 404 {
		t.Fatalf("ohne Token eingerichtet: %d", w.Code)
	}
	s.setSetting("monitor_token", "ein-langes-monitoring-token")
	if w := do(s, "GET", "/api/monitor", "", map[string]string{"Authorization": "Bearer monitor:falsch"}); w.Code != 401 {
		t.Fatalf("falsches Token: %d", w.Code)
	}
	// the monitoring token opens nothing else
	if w := do(s, "GET", "/api/agents", "", map[string]string{"Authorization": "Bearer monitor:ein-langes-monitoring-token"}); w.Code != 401 {
		t.Fatalf("Admin-API mit Monitoring-Token: %d", w.Code)
	}
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('a1','frisch','x',?,?)`, now(), now())
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('a2','alt','x',?,0)`, now())
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,removed) VALUES('a3','weg','x',?,1)`, now())
	addSnapshot(t, s, "s1", 0, 0, []byte("eins"))
	addSnapshot(t, s, "s2", 0, 0, []byte("zwei"))
	s.db.Exec(`UPDATE snapshots SET agent_id='a2',time=? WHERE id='s2'`, now()-30*86400)

	w := do(s, "GET", "/api/monitor", "", map[string]string{"Authorization": "Bearer monitor:ein-langes-monitoring-token"})
	if w.Code != 200 {
		t.Fatalf("Abfrage: %d %s", w.Code, w.Body)
	}
	var st monitorStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Agents) != 2 || len(st.Storages) != 1 || st.OK {
		t.Fatalf("Status: %+v", st)
	}
	by := map[string]monitorAgent{}
	for _, a := range st.Agents {
		by[a.Hostname] = a
	}
	if a := by["frisch"]; a.Stale || !a.Online || a.BackupAge < 0 {
		t.Errorf("frisch: %+v", a)
	}
	if a := by["alt"]; !a.Stale || a.Online {
		t.Errorf("alt: %+v", a)
	}
	joined := strings.Join(st.Problems, "|")
	for _, want := range []string{"alt: letztes Backup zu alt", "Server-Sicherung nicht eingerichtet"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Problem %q fehlt in %q", want, joined)
		}
	}
	if strings.Contains(joined, "frisch") {
		t.Errorf("frischer Rechner als Problem gemeldet: %q", joined)
	}
}

// Old jobs and audit entries go, the job of an existing backup stays.
func TestPruneHistory(t *testing.T) {
	s := testServer(t)
	old := now() - 400*86400
	job := func(status string, finished int64) int64 {
		res, _ := s.db.Exec(`INSERT INTO jobs(agent_id,type,status,created,finished,log) VALUES('a1','backup',?,?,?,'protokoll')`, status, finished, finished)
		id, _ := res.LastInsertId()
		return id
	}
	gone, kept, recent, running := job("success", old), job("success", old), job("failed", now()-86400), job("running", 0)
	addSnapshot(t, s, "s1", 0, 0, []byte("eins"))
	s.db.Exec(`UPDATE snapshots SET job_id=? WHERE id='s1'`, kept)
	s.db.Exec(`INSERT INTO audit(time,user,action,detail) VALUES(?,?,?,?)`, now()-3*365*86400, "x", "alt", "")
	s.db.Exec(`INSERT INTO audit(time,user,action,detail) VALUES(?,?,?,?)`, now(), "x", "neu", "")

	s.pruneHistory()
	exists := func(id int64) (n int) {
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE id=?`, id).Scan(&n)
		return
	}
	if exists(gone) != 0 || exists(kept) != 1 || exists(recent) != 1 || exists(running) != 1 {
		t.Errorf("Aufträge: alt=%d mit-Backup=%d neu=%d laufend=%d", exists(gone), exists(kept), exists(recent), exists(running))
	}
	var audits int
	s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action IN ('alt','neu')`).Scan(&audits)
	if audits != 1 {
		t.Errorf("%d Audit-Einträge, erwartet 1", audits)
	}
	// switched off
	s.setSetting("keep_jobs_days", "0")
	old2 := job("success", old)
	s.pruneHistory()
	if exists(old2) != 1 {
		t.Error("keep_jobs_days=0 soll nichts löschen")
	}
}

// A panic in a background task is logged; the process goes on.
func TestBackgroundPanic(t *testing.T) {
	s := testServer(t)
	var buf lockedBuffer
	s.log.SetOutput(&buf)
	s.bg("Test", func() {
		var m map[string]int
		m["x"] = 1
	})
	// the log line is written after f has returned, in the recover of bg
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "interner Fehler in Test") {
		if time.Now().After(deadline) {
			t.Fatalf("Panic nicht protokolliert: %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lockedBuffer is a bytes.Buffer that may be written and read concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The Linux install command pins the key of this server's certificate.
func TestLinuxInstallPinsKey(t *testing.T) {
	s := testServer(t)
	c, err := x509.ParseCertificate(s.tlsCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	want := "--pinnedpubkey 'sha256//" + base64.StdEncoding.EncodeToString(h[:]) + "'"
	if cmd := s.installCommands("tok")["linux"]; !strings.Contains(cmd, want) {
		t.Errorf("Befehl ohne Schlüssel-Pin: %s", cmd)
	}
}
