package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"bastion/internal/proto"
	"bastion/internal/repo"
)

func enroll(t *testing.T, s *Server, tok, host string, macs []string) (proto.EnrollResponse, int) {
	t.Helper()
	b, _ := json.Marshal(proto.EnrollRequest{Token: tok, Hostname: host, MACs: macs, OS: "windows", Arch: "amd64"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/agent/enroll", bytes.NewReader(b))
	req.RemoteAddr = "10.0.0.1:1234"
	s.handleEnroll(rec, req)
	var resp proto.EnrollResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp, rec.Code
}

// A second enrollment with the same name and MAC must not take over the
// first agent; only an administrator can merge the history.
func TestEnrollNoTakeover(t *testing.T) {
	s := testServer(t)
	tok := s.createToken("t", "", 0, 0, -1)
	first, code := enroll(t, s, tok, "PC1", []string{"00:11:22:33:44:55"})
	if code != 200 {
		t.Fatalf("enroll: %d", code)
	}
	addSnapshot(t, s, "snap1", 0, 0, []byte("hallo"))
	s.db.Exec(`UPDATE snapshots SET agent_id=?`, first.AgentID)

	second, code := enroll(t, s, tok, "pc1", []string{"00-11-22-33-44-55"})
	if code != 200 {
		t.Fatalf("enroll 2: %d", code)
	}
	if second.AgentID == first.AgentID {
		t.Fatal("zweite Anmeldung hat die ID des ersten Agents übernommen")
	}
	var replaces string
	s.db.QueryRow(`SELECT replaces FROM agents WHERE id=?`, second.AgentID).Scan(&replaces)
	if replaces != first.AgentID {
		t.Fatalf("replaces = %q, want %q", replaces, first.AgentID)
	}
	// the old secret still works, the old backups stay with the old agent
	var owner string
	s.db.QueryRow(`SELECT agent_id FROM snapshots WHERE id='snap1'`).Scan(&owner)
	if owner != first.AgentID {
		t.Fatal("Backups wurden ohne Bestätigung übertragen")
	}

	// administrator merge
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/agents/"+second.AgentID+"/merge", nil)
	req.SetPathValue("id", second.AgentID)
	s.handleAgentMerge(rec, req, &user{Name: "admin", Role: "admin"})
	if rec.Code != 200 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body)
	}
	s.db.QueryRow(`SELECT agent_id FROM snapshots WHERE id='snap1'`).Scan(&owner)
	if owner != second.AgentID {
		t.Fatal("Backups nach Übernahme nicht beim neuen Agent")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE id=?`, first.AgentID).Scan(&n)
	if n != 0 {
		t.Fatal("alter Eintrag nicht entfernt")
	}
}

func TestEnrollTokenUses(t *testing.T) {
	s := testServer(t)
	tok := s.createToken("t", "", 0, 0, 1)
	if _, code := enroll(t, s, tok, "A", nil); code != 200 {
		t.Fatalf("1. Anmeldung: %d", code)
	}
	if _, code := enroll(t, s, tok, "B", nil); code != 403 {
		t.Fatalf("2. Anmeldung mit verbrauchtem Token: %d", code)
	}
}

func postSnapshot(s *Server, agentID string, snap repo.Snapshot) int {
	b, _ := json.Marshal(snap)
	rec := httptest.NewRecorder()
	s.handlePostSnapshot(rec, httptest.NewRequest("POST", "/api/agent/snapshots", bytes.NewReader(b)), &principal{AgentID: agentID})
	return rec.Code
}

// Snapshots are only accepted for the running backup job, with the job's
// plan and the server's time.
func TestSnapshotBoundToJob(t *testing.T) {
	s := testServer(t)
	st, _ := s.storeByID(0)
	var tree bytes.Buffer
	tw := repo.NewTreeWriter(&tree)
	tw.Write(&repo.Node{Path: "", Type: repo.TypeDir})
	tw.Flush()
	tid, _ := st.Put(tree.Bytes())
	forged := repo.Snapshot{ID: "20990101T000000-aaaaaa", PlanID: 99, Kind: "files", Time: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		Volumes: []repo.Volume{{Key: "C:", Tree: []repo.ID{tid}}}}

	if code := postSnapshot(s, "a1", forged); code != http.StatusConflict {
		t.Fatalf("ohne Auftrag: %d", code)
	}
	started := now() - 60
	s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,started) VALUES('a1',7,'backup',?,'running',?,?)`,
		jsonStr(proto.BackupParams{PlanID: 7, Kind: "image"}), started, started)
	if code := postSnapshot(s, "a1", forged); code != http.StatusNoContent {
		t.Fatalf("mit Auftrag: %d", code)
	}
	var plan, ts int64
	var kind string
	s.db.QueryRow(`SELECT plan_id,time,kind FROM snapshots WHERE id=?`, forged.ID).Scan(&plan, &ts, &kind)
	if plan != 7 || ts != started || kind != "image" {
		t.Fatalf("plan=%d time=%d kind=%s: Werte des Agents übernommen", plan, ts, kind)
	}
	forged.ID = "20990101T000000-bbbbbb"
	if code := postSnapshot(s, "a1", forged); code != http.StatusConflict {
		t.Fatalf("zweites Backup im selben Auftrag: %d", code)
	}
}

// A damaged chunk is quarantined, its backups are flagged and no longer
// used as the base of incremental backups.
func TestScrubQuarantine(t *testing.T) {
	s := testServer(t)
	addSnapshot(t, s, "snap1", 1, s.defaultStorage(), []byte("wichtige Daten"))
	st, _ := s.storeByID(0)
	fid := repo.Hash([]byte("wichtige Daten"))
	p := st.Dir() + "/chunks/" + fid.String()[:2] + "/" + fid.String()[2:4] + "/" + fid.String()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	os.WriteFile(p, b, 0o600)

	s.runScrub("test")
	if st.Has(fid) {
		t.Fatal("beschädigter Chunk nicht in Quarantäne")
	}
	var damaged bool
	s.db.QueryRow(`SELECT damaged FROM snapshots WHERE id='snap1'`).Scan(&damaged)
	if !damaged {
		t.Fatal("Backup nicht als beschädigt markiert")
	}
	s.db.Exec(`INSERT INTO plans(id,name,kind,schedule,keep_last,keep_daily,keep_weekly,keep_monthly,created) VALUES(1,'p','files','',0,0,0,0,0)`)
	bp, err := s.backupParams(1, "{}", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if bp.Parent != "" {
		t.Fatal("beschädigtes Backup als Basis für inkrementelles Backup verwendet")
	}
	// the chunk can be uploaded again
	if _, err := st.Put([]byte("wichtige Daten")); err != nil || !st.Has(fid) {
		t.Fatalf("erneutes Hochladen: %v", err)
	}
}
