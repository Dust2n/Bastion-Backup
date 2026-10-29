package server

import (
	"bytes"
	"encoding/base64"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bastion/internal/repo"
)

func TestServerBackupRestore(t *testing.T) {
	s := testServer(t)
	ext := filepath.Join(t.TempDir(), "nas")
	if err := probeWritable(ext); err != nil {
		t.Fatal(err)
	}
	res, _ := s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('NAS','local',?,0,?)`, ext, now())
	nas, _ := res.LastInsertId()
	addSnapshot(t, s, "snapA", 0, nas, []byte("vorher"))

	if _, err := s.runServerBackup("test"); err == nil {
		t.Fatal("backup without passphrase must fail")
	}
	s.setSetting("dr_passphrase", base64.StdEncoding.EncodeToString(s.seal("geheimes-passwort-123")))
	st, err := s.runServerBackup("test")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Offsite || len(st.Locations) != 2 {
		t.Fatalf("status %+v", st)
	}

	// a backup made after the server backup exists only in the storage
	nasStore, _ := s.storeByID(nas)
	fid, _ := nasStore.Put([]byte("nachher"))
	var tree bytes.Buffer
	tw := repo.NewTreeWriter(&tree)
	tw.Write(&repo.Node{Path: "", Type: repo.TypeDir})
	tw.Write(&repo.Node{Path: "b.txt", Type: repo.TypeFile, Size: 7, Chunks: []repo.ID{fid}})
	tw.Flush()
	tid, _ := nasStore.Put(tree.Bytes())
	snapB := repo.Snapshot{ID: "snapB", AgentID: "a1", Kind: "files", Time: time.Now().Add(time.Minute),
		Volumes: []repo.Volume{{Key: "D:", Tree: []repo.ID{tid}, Files: 1, Bytes: 7}}}
	os.MkdirAll(filepath.Join(ext, "snapshots"), 0o700)
	os.WriteFile(filepath.Join(ext, "snapshots", "snapB.json"), []byte(jsonStr(snapB)), 0o600)

	file, err := FindServerBackup(ext)
	if err != nil {
		t.Fatal(err)
	}
	newDir := t.TempDir()
	if err := RestoreServerBackup(file, newDir, "falsch", false, io.Discard); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if err := RestoreServerBackup(file, newDir, "geheimes-passwort-123", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	s2, err := New(Options{DataDir: newDir, Listen: ":0"}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.db.Close()
	if !bytes.Equal(s2.repoKey, s.repoKey) || s2.fp != s.fp {
		t.Fatal("keys or certificate differ after restore")
	}
	s2.importNewerSnapshots()
	for _, id := range []string{"snapA", "snapB"} {
		var n int
		s2.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id=?`, id).Scan(&n)
		if n != 1 {
			t.Fatalf("%s missing after restore", id)
		}
	}
	st2, _ := s2.storeByID(nas)
	if b, err := st2.Get(fid); err != nil || string(b) != "nachher" {
		t.Fatalf("chunk not readable with restored key: %v", err)
	}
	if s2.serverBackupPass() != "geheimes-passwort-123" {
		t.Fatal("passphrase setting not restored")
	}
}
