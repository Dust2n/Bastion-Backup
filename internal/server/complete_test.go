package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"bastion/internal/proto"
	"bastion/internal/repo"
)

func TestSameMachine(t *testing.T) {
	local := []string{"127.0.0.1/8", "172.16.50.10/24", "fe80::1/64"}
	cases := []struct {
		host string
		ips  []string
		want bool
	}{
		{"backup-s01", []string{"172.16.50.10"}, true},
		{"BACKUP-S01", []string{"10.0.0.5/24", "172.16.50.10/24"}, true},
		{"BACKUP-S01", []string{"172.16.50.20"}, false}, // same name, other machine
		{"PC1", []string{"172.16.50.10"}, false},
		{"BACKUP-S01", []string{"127.0.0.1"}, false}, // loopback proves nothing
		{"BACKUP-S01", nil, false},
		{"", []string{"172.16.50.10"}, false},
	}
	for _, c := range cases {
		if got := sameMachine(c.host, c.ips, "BACKUP-S01", local); got != c.want {
			t.Errorf("sameMachine(%q, %v) = %v", c.host, c.ips, got)
		}
	}
}

// The agent on the server's own machine is told to leave out the local
// repositories; other agents are not.
func TestOwnRepositoryExcluded(t *testing.T) {
	s := testServer(t)
	dir := filepath.Join(t.TempDir(), "ziel")
	s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('Ziel','local',?,0,?)`, dir, now())
	s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('NAS','smb','\\nas\backup',0,?)`, now())
	paths := s.ownDataPaths()
	want := map[string]bool{s.path("cache"): true, s.path("repo"): true, dir: true}
	if len(paths) != len(want) {
		t.Fatalf("ownDataPaths = %v", paths)
	}
	for _, p := range paths {
		if !want[p] {
			t.Errorf("unerwarteter Pfad %q", p)
		}
	}
	// an agent that is not this machine gets no extra excludes
	s.db.Exec(`INSERT INTO agents(id,hostname,ips,secret_hash,enrolled) VALUES('a1','ganz-anderer-rechner','["192.0.2.1"]','x',?)`, now())
	bp, err := s.backupParams(0, `{"kind":"image"}`, "a1")
	if err != nil || len(bp.ExcludePaths) != 0 {
		t.Fatalf("fremder Agent: %v, %v", bp, err)
	}
}

// A snapshot that references chunks the storage does not have is refused,
// and the backup it was based on no longer serves as a base.
func TestIncompleteSnapshotRefused(t *testing.T) {
	s := testServer(t)
	st, _ := s.storeByID(0)
	addSnapshot(t, s, "basis", 7, 0, []byte("alter inhalt"))

	have, _ := st.Put([]byte("vorhanden"))
	gone := repo.Hash([]byte("nie hochgeladen"))
	build := func(chunks ...repo.ID) repo.Snapshot {
		var tree bytes.Buffer
		tw := repo.NewTreeWriter(&tree)
		tw.Write(&repo.Node{Path: "", Type: repo.TypeDir})
		tw.Write(&repo.Node{Path: "a.bin", Type: repo.TypeFile, Size: 9, Chunks: chunks})
		tw.Flush()
		tid, _ := st.Put(tree.Bytes())
		return repo.Snapshot{ID: "20261115T000000-abcdef", Kind: "files", Time: time.Now(),
			Volumes: []repo.Volume{{Key: "D:", Tree: []repo.ID{tid}, Files: 1, Bytes: 9}}}
	}
	params, _ := json.Marshal(proto.BackupParams{PlanID: 7, Kind: "files", Parent: "basis"})
	s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,started) VALUES('a1',7,'backup',?,'running',?,?)`,
		string(params), now(), now())

	if code := postSnapshot(s, "a1", build(have, gone)); code != http.StatusBadRequest {
		t.Fatalf("unvollständiges Backup: %d", code)
	}
	var n, damaged int
	s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id='20261115T000000-abcdef'`).Scan(&n)
	s.db.QueryRow(`SELECT damaged FROM snapshots WHERE id='basis'`).Scan(&damaged)
	if n != 0 || damaged != 1 {
		t.Fatalf("gespeichert=%d, Basis beschädigt=%d", n, damaged)
	}
	// the same job may deliver a complete backup afterwards
	if code := postSnapshot(s, "a1", build(have)); code != http.StatusNoContent {
		t.Fatalf("vollständiges Backup: %d", code)
	}
}

// One backup with a missing tree chunk must not stop the cleanup of the
// whole storage; a storage that is not reachable must delete nothing.
func TestCleanupSurvivesIncompleteBackup(t *testing.T) {
	s := testServer(t)
	def := s.defaultStorage()
	st, _ := s.storeByID(def)
	addSnapshot(t, s, "gut", 0, def, []byte("guter inhalt"))

	// second volume is fine, first volume lost its tree
	keep, _ := st.Put([]byte("zweites volume"))
	var tree bytes.Buffer
	tw := repo.NewTreeWriter(&tree)
	tw.Write(&repo.Node{Path: "b.bin", Type: repo.TypeFile, Size: 14, Chunks: []repo.ID{keep}})
	tw.Flush()
	tid, _ := st.Put(tree.Bytes())
	lost := repo.Hash([]byte("verlorener baum"))
	bad := repo.Snapshot{ID: "kaputt", AgentID: "a1", Kind: "files", Time: time.Now(),
		Volumes: []repo.Volume{{Key: "C:", Tree: []repo.ID{lost}}, {Key: "D:", Tree: []repo.ID{tid}}}}
	s.db.Exec(`INSERT INTO snapshots(id,agent_id,plan_id,time,kind,files,bytes,new_bytes,data,storage_id) VALUES('kaputt','a1',0,?,'files',1,14,0,?,?)`,
		now(), jsonStr(bad), def)

	// in a pack of its own: a pack nobody needs is deleted, while a single
	// unused chunk in a pack that is otherwise needed stays until the pack
	// is worth rewriting
	st.Seal()
	orphan, _ := st.Put([]byte("gehört zu keinem backup"))
	old := time.Now().Add(-24 * time.Hour)
	st.Walk(func(c repo.ChunkInfo) error {
		return osChtimes(st, c.ID, old)
	})

	if _, err := s.gcStorage(def); err != nil {
		t.Fatalf("Bereinigung: %v", err)
	}
	var damaged int
	s.db.QueryRow(`SELECT damaged FROM snapshots WHERE id='kaputt'`).Scan(&damaged)
	if damaged != 1 {
		t.Error("unvollständiges Backup nicht als beschädigt markiert")
	}
	if st.Has(orphan) {
		t.Error("verwaister Chunk nicht gelöscht")
	}
	if !st.Has(keep) || !st.Has(tid) {
		t.Error("Chunks des lesbaren Volumes wurden gelöscht")
	}
	var goodSnap repo.Snapshot
	var data string
	s.db.QueryRow(`SELECT data FROM snapshots WHERE id='gut'`).Scan(&data)
	json.Unmarshal([]byte(data), &goodSnap)
	if m, err := missingChunks(&goodSnap, st); err != nil || m != 0 {
		t.Errorf("gutes Backup nach der Bereinigung: %d fehlen, %v", m, err)
	}
}
