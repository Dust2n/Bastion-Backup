package server

import (
	"strings"
	"testing"
	"time"

	"bastion/internal/repo"
)

func waitJob(t *testing.T, s *Server, id int64) (string, string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		var st, msg string
		s.db.QueryRow(`SELECT status,message FROM jobs WHERE id=?`, id).Scan(&st, &msg)
		if st != "running" {
			return st, msg
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return "", ""
}

func TestVerify(t *testing.T) {
	s := testServer(t)
	def := s.defaultStorage()
	addSnapshot(t, s, "ok1", 0, def, []byte("inhalt eins"))
	id, err := s.startVerify("ok1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if st, msg := waitJob(t, s, id); st != "success" {
		t.Fatalf("intact backup: %s %s", st, msg)
	}
	var verified int64
	var ok bool
	s.db.QueryRow(`SELECT verified,verify_ok FROM snapshots WHERE id='ok1'`).Scan(&verified, &ok)
	if verified == 0 || !ok {
		t.Fatal("result not stored")
	}

	// a missing file chunk must be found
	addSnapshot(t, s, "bad1", 0, def, []byte("inhalt zwei, wird gelöscht"))
	snap, _ := s.loadSnapshot("bad1")
	st, _ := s.storeByID(def)
	tree := map[repo.ID]bool{}
	for _, id := range snap.Volumes[0].Tree {
		tree[id] = true
	}
	snap.Refs(st.Get, func(id repo.ID) {
		if !tree[id] {
			st.Delete(id)
		}
	})
	id, _ = s.startVerify("bad1", "test")
	if st, msg := waitJob(t, s, id); st != "failed" || !strings.Contains(msg, "fehlen oder sind beschädigt") {
		t.Fatalf("damaged backup: %s %s", st, msg)
	}
}

func TestBootCheck(t *testing.T) {
	win := map[string]map[string]int64{"C:": {
		"windows/system32/config/system": 1, "windows/system32/ntoskrnl.exe": 1, "windows/system32/winload.efi": 1}}
	if msg := bootCheck("windows", win); msg != "" {
		t.Fatal(msg)
	}
	delete(win["C:"], "windows/system32/ntoskrnl.exe")
	if msg := bootCheck("windows", win); !strings.Contains(msg, "ntoskrnl") {
		t.Fatal("missing kernel not reported")
	}
	lin := map[string]map[string]int64{"/": {"etc/fstab": 1}, "/boot": {"vmlinuz-6.1": 1}}
	if msg := bootCheck("linux", lin); msg != "" {
		t.Fatal(msg)
	}
}
