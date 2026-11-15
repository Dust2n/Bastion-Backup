package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"bastion/internal/proto"
)

func TestCrossed(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 11, day, h, m, 0, 0, time.Local) }
	cases := []struct {
		a, b time.Time
		hhmm string
		want bool
	}{
		{at(15, 3, 29), at(15, 3, 30), "03:30", true},  // normal tick
		{at(15, 3, 30), at(15, 3, 31), "03:30", false}, // already handled
		{at(15, 1, 0), at(15, 2, 0), "03:30", false},   // not yet due
		{at(14, 22, 0), at(15, 1, 0), "23:00", true},   // due yesterday, server was off over midnight
		{at(14, 22, 0), at(15, 1, 0), "03:30", false},  // today's run is still ahead
		{at(10, 12, 0), at(15, 1, 0), "03:30", true},   // several days off: once
		{at(15, 1, 0), at(15, 2, 0), "", false},        // switched off
		{at(15, 1, 0), at(15, 2, 0), "quatsch", false},
	}
	for i, c := range cases {
		if got := crossed(c.a, c.b, c.hhmm); got != c.want {
			t.Errorf("Fall %d (%s): crossed = %v, erwartet %v", i, c.hhmm, got, c.want)
		}
	}
}

// A plan whose time fell into a pause of the server runs once afterwards.
func TestScheduleCatchUp(t *testing.T) {
	s := testServer(t)
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('a1','pc1','x',?,?)`, now(), now())
	res, _ := s.db.Exec(`INSERT INTO plans(name,kind,schedule,keep_last,keep_daily,keep_weekly,keep_monthly,enabled,created)
		VALUES('nachts','image','0 1 * * *',1,0,0,0,1,?)`, now())
	planID, _ := res.LastInsertId()
	s.db.Exec(`INSERT INTO plan_targets(plan_id,target) VALUES(?,'all')`, planID)
	// keep the maintenance goroutines out of this test
	for _, k := range []string{"gc_time", "verify_time", "scrub_time"} {
		s.setSetting(k, "aus")
	}
	queued := func() (n int) {
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='backup' AND plan_id=?`, planID).Scan(&n)
		return
	}

	cur := time.Date(2026, 11, 15, 9, 0, 0, 0, time.Local)
	// first start ever: nothing to make up for
	if last := s.schedulerStart(cur); !last.Equal(cur) {
		t.Fatalf("erster Start: last = %v", last)
	}
	// stopped the evening before, started in the morning: 01:00 was missed
	stopped := time.Date(2026, 11, 14, 20, 0, 0, 0, time.Local)
	s.setSetting("sched_last", strconv.FormatInt(stopped.Unix(), 10))
	last := s.schedulerStart(cur)
	if !last.Equal(stopped) {
		t.Fatalf("last = %v, erwartet %v", last, stopped)
	}
	s.schedulerTick(last, cur)
	if n := queued(); n != 1 {
		t.Fatalf("%d Backups eingeplant, erwartet 1", n)
	}
	if got := s.setting("sched_last", ""); got != strconv.FormatInt(cur.Unix(), 10) {
		t.Fatalf("sched_last = %s", got)
	}
	// next tick: nothing new
	s.schedulerTick(cur, cur.Add(30*time.Second))
	if n := queued(); n != 1 {
		t.Fatalf("%d Backups nach dem zweiten Durchlauf", n)
	}
	// a very long pause is limited, a clock set back is ignored
	s.setSetting("sched_last", strconv.FormatInt(cur.AddDate(0, -3, 0).Unix(), 10))
	if last := s.schedulerStart(cur); cur.Sub(last) != maxCatchUp {
		t.Fatalf("lange Pause: %v zurück", cur.Sub(last))
	}
	s.setSetting("sched_last", strconv.FormatInt(cur.Add(time.Hour).Unix(), 10))
	if last := s.schedulerStart(cur); !last.Equal(cur) {
		t.Fatalf("Uhr zurückgestellt: last = %v", last)
	}
}

// A running job whose machine stopped reporting is closed, so it no longer
// blocks the cleanup; a job that still reports stays untouched.
func TestStalledJobFails(t *testing.T) {
	s := testServer(t)
	secret := "geheim"
	s.db.Exec(`INSERT INTO agents(id,hostname,secret_hash,enrolled,last_seen) VALUES('a1','pc1',?,?,0)`, sha(secret), now())
	add := func(typ, agent string, started, updated int64) int64 {
		res, err := s.db.Exec(`INSERT INTO jobs(agent_id,type,status,created,started,updated) VALUES(?,?,'running',?,?,?)`,
			agent, typ, started, started, updated)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	old := now() - 3*3600
	dead := add("backup", "a1", old, old+60)      // last report long ago
	legacy := add("backup", "a1", old, 0)         // started by an older version
	alive := add("backup", "a1", old, now()-30)   // long backup, still reporting
	restore := add("restore_image", "a1", old, 0) // rescue environment, own limit
	verify := add("verify", "", old, 0)           // server-side job
	status := func(id int64) (st string) {
		s.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, id).Scan(&st)
		return
	}

	s.failStalledJobs()
	for id, want := range map[int64]string{dead: "failed", legacy: "failed", alive: "running", restore: "running", verify: "running"} {
		if got := status(id); got != want {
			t.Errorf("Auftrag %d: %s, erwartet %s", id, got, want)
		}
	}
	var running int
	s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='backup' AND status='running'`).Scan(&running)
	if running != 1 {
		t.Errorf("%d laufende Backups, erwartet 1", running)
	}

	progress := func(id int64) proto.ProgressReply {
		body, _ := json.Marshal(proto.Progress{Percent: 10})
		r := httptest.NewRequest("POST", fmt.Sprintf("/api/agent/jobs/%d/progress", id), bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer agent:a1:"+secret)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("progress %d: %d %s", id, w.Code, w.Body)
		}
		var reply proto.ProgressReply
		json.Unmarshal(w.Body.Bytes(), &reply)
		return reply
	}
	// the machine comes back: it is told to stop, the job stays failed
	if !progress(dead).Cancel || status(dead) != "failed" {
		t.Errorf("abgebrochener Auftrag: Agent bekommt kein Abbruchsignal oder Status %s", status(dead))
	}
	// a report keeps a job alive and the machine online
	s.db.Exec(`UPDATE jobs SET updated=? WHERE id=?`, old, alive)
	if progress(alive).Cancel {
		t.Error("laufender Auftrag wurde abgebrochen")
	}
	s.failStalledJobs()
	if status(alive) != "running" {
		t.Errorf("Auftrag mit frischer Meldung: %s", status(alive))
	}
	var seen int64
	s.db.QueryRow(`SELECT last_seen FROM agents WHERE id='a1'`).Scan(&seen)
	if now()-seen > 5 {
		t.Error("last_seen wurde durch die Fortschrittsmeldung nicht gesetzt")
	}
	// switched off
	s.setSetting("job_stall_minutes", "0")
	s.db.Exec(`UPDATE jobs SET updated=? WHERE id=?`, old, alive)
	s.failStalledJobs()
	if status(alive) != "running" {
		t.Error("Zeitlimit 0 soll abschalten")
	}
}
