package server

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bastion/internal/repo"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{DataDir: t.TempDir(), Listen: ":0"}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

// addSnapshot stores a small one-volume backup in a storage.
func addSnapshot(t *testing.T, s *Server, id string, planID, storage int64, content []byte) {
	t.Helper()
	st, err := s.storeByID(storage)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := st.Put(content)
	var tree bytes.Buffer
	tw := repo.NewTreeWriter(&tree)
	tw.Write(&repo.Node{Path: "", Type: repo.TypeDir})
	tw.Write(&repo.Node{Path: "daten", Type: repo.TypeDir})
	tw.Write(&repo.Node{Path: "daten/a.txt", Type: repo.TypeFile, Size: int64(len(content)), Chunks: []repo.ID{fid}})
	tw.Flush()
	tid, _ := st.Put(tree.Bytes())
	snap := repo.Snapshot{ID: id, AgentID: "a1", PlanID: planID, Kind: "files", Time: time.Now(),
		Volumes: []repo.Volume{{Key: "D:", Root: `D:\`, Tree: []repo.ID{tid}, Files: 1, Bytes: int64(len(content))}}}
	if _, err := s.db.Exec(`INSERT INTO snapshots(id,agent_id,plan_id,time,kind,files,bytes,new_bytes,data,storage_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, "a1", planID, snap.Time.Unix(), "files", 1, len(content), 0, jsonStr(snap), storage); err != nil {
		t.Fatal(err)
	}
}

func TestStoragesAndMove(t *testing.T) {
	s := testServer(t)
	def := s.defaultStorage()
	if def == 0 {
		t.Fatal("no default storage created")
	}
	// second storage
	dir := filepath.Join(t.TempDir(), "ziel")
	if err := probeWritable(dir); err != nil {
		t.Fatal(err)
	}
	res, _ := s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('Ziel','local',?,0,?)`, dir, now())
	target, _ := res.LastInsertId()

	res, _ = s.db.Exec(`INSERT INTO plans(name,kind,schedule,storage_id,created) VALUES('P','files','',0,?)`, now())
	plan, _ := res.LastInsertId()
	content := bytes.Repeat([]byte("bastion "), 50000)
	addSnapshot(t, s, "snap1", plan, def, content)

	if got := s.planStorage(plan); got != def {
		t.Fatalf("plan with storage 0 must use default, got %d", got)
	}
	// agent with a running backup uses the plan's storage
	s.db.Exec(`UPDATE plans SET storage_id=? WHERE id=?`, target, plan)
	s.setAgentStore("a1", s.planStorage(plan))
	if _, id, _ := s.storeFor(&principal{AgentID: "a1"}); id != target {
		t.Fatalf("storeFor: got %d want %d", id, target)
	}
	// restore token uses the snapshot's storage
	if _, id, _ := s.storeFor(&principal{AgentID: "x", RestoreJob: 1, RestoreSnap: "snap1"}); id != def {
		t.Fatalf("storeFor(restore): got %d want %d", id, def)
	}

	job, err := s.startMove(plan, target, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		var st string
		s.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, job).Scan(&st)
		if st != "running" {
			if st != "success" {
				var msg string
				s.db.QueryRow(`SELECT message FROM jobs WHERE id=?`, job).Scan(&msg)
				t.Fatalf("move %s: %s", st, msg)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.snapStorage("snap1") != target {
		t.Fatal("snapshot not switched to target storage")
	}
	// the backup is readable from the new storage
	snap, _ := s.loadSnapshot("snap1")
	dst, _ := s.storeByID(target)
	var got []byte
	repo.ReadTree(repo.NewChunkReader(snap.Volumes[0].Tree, dst.Get), func(n *repo.Node) error {
		if n.Type == repo.TypeFile {
			got, _ = io.ReadAll(repo.NewChunkReader(n.Chunks, dst.Get))
		}
		return nil
	})
	if !bytes.Equal(got, content) {
		t.Fatal("content differs after move")
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", "snap1.json")); err != nil {
		t.Fatal("snapshot description missing in target storage")
	}
	// GC of the old storage frees the moved chunks (after the grace period)
	src, _ := s.storeByID(def)
	old := time.Now().Add(-7 * time.Hour)
	src.Walk(func(c repo.ChunkInfo) error {
		p := filepath.Join(src.Dir(), "chunks", c.ID.String()[:2], c.ID.String()[2:4], c.ID.String())
		return os.Chtimes(p, old, old)
	})
	st, err := s.gcStorage(def)
	if err != nil {
		t.Fatal(err)
	}
	if st.Chunks != 0 || st.Freed == 0 {
		t.Fatalf("old storage not cleaned: %+v", st)
	}
	if st2, _ := s.gcStorage(target); st2.Chunks != 2 {
		t.Fatalf("target must keep 2 chunks, has %d", st2.Chunks)
	}
}

func TestIndexLookupAndSearch(t *testing.T) {
	s := testServer(t)
	addSnapshot(t, s, "snapI", 0, s.defaultStorage(), []byte("x"))
	idx, err := s.buildIndex("snapI")
	if err != nil {
		t.Fatal(err)
	}
	vi := idx.Vols["D:"]
	i, ok := vi.lookup("daten/a.txt")
	if !ok || vi.path(i) != "daten/a.txt" {
		t.Fatal("lookup failed")
	}
	s.trees = newIndexCache() // force loading from disk
	idx2, err := s.loadIndex("snapI")
	if err != nil || len(idx2.Vols["D:"].Nodes) != 3 {
		t.Fatal("index not loaded from cache file", err)
	}
}

func TestMigrationOldDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	// running migrate twice must be harmless
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// optional: a copy of a real 0.1 database (BASTION_TEST_DB)
	src := os.Getenv("BASTION_TEST_DB")
	if src == "" {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(dir, "copy.db")
	os.WriteFile(cp, b, 0o600)
	db, err = openDB(cp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var snaps, plans int
	if err := db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE storage_id=0`).Scan(&snaps); err != nil {
		t.Fatal(err)
	}
	db.QueryRow(`SELECT COUNT(*) FROM plans WHERE storage_id=0`).Scan(&plans)
	t.Logf("Kopie migriert: %d Backups und %d Pläne ohne Speicherort (werden beim Start dem Standard zugeordnet)", snaps, plans)
}
