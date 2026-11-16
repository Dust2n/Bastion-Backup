package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bastion/internal/agent"
	"bastion/internal/client"
	"bastion/internal/proto"
)

// readTree returns relative path -> content of all files below dir.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBackupRestoreEndToEnd runs a real agent against a real server over
// TLS: file backup, incremental backup after a change, restore into a new
// folder, and a comparison of every file.
func TestBackupRestoreEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("Ende-zu-Ende-Test")
	}
	s := testServer(t)
	ts := httptest.NewTLSServer(s.Handler())
	defer ts.Close()

	// source data: empty file, small (inline) file, several chunks, subfolders
	src := filepath.Join(t.TempDir(), "quelle")
	big := make([]byte, 5<<20)
	rand.Read(big)
	files := map[string][]byte{
		"leer.txt":              {},
		"klein.txt":             []byte("hallo"),
		"ordner/mittel.bin":     big[:300<<10],
		"ordner/tief/gross.bin": big,
		"ordner/tief/kopie.bin": big, // same content: deduplicated
		"umlaut äöü/datei.txt":  []byte("Grüße"),
	}
	for name, data := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	resp, code := enroll(t, s, s.createToken("t", "", 0, 0, 1), "e2e", nil)
	if code != 200 {
		t.Fatalf("Anmeldung: %d", code)
	}
	cfg := &agent.Config{Server: ts.URL, Fingerprint: client.Fingerprint(ts.Certificate().Raw),
		AgentID: resp.AgentID, Secret: resp.Secret, SigningKey: resp.SigningKey}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agent.New(cfg, log.New(io.Discard, "", 0)).Run(ctx)
		close(done)
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	run := func(typ string, params any) (int64, string) {
		t.Helper()
		id, err := s.enqueue(resp.AgentID, 0, typ, params, "test")
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			var status, msg, snap, logText string
			s.db.QueryRow(`SELECT status,message,snapshot_id,log FROM jobs WHERE id=?`, id).Scan(&status, &msg, &snap, &logText)
			switch status {
			case "success":
				return id, snap
			case "failed", "canceled":
				t.Fatalf("%s: %s %s\n%s", typ, status, msg, logText)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: Zeitlimit", typ)
		return 0, ""
	}
	backup := proto.BackupParams{Kind: "files", Paths: []string{src}}

	_, snap1 := run(proto.JobBackup, backup)
	if snap1 == "" {
		t.Fatal("kein Backup gespeichert")
	}
	st, _ := s.storeByID(0)
	chunks := func() int { return chunkCount(st) }
	first := chunks()

	// change one file, add one: the second backup must only add little
	os.WriteFile(filepath.Join(src, "klein.txt"), []byte("geändert, jetzt länger als vorher"), 0o644)
	os.WriteFile(filepath.Join(src, "neu.txt"), make([]byte, 10<<10), 0o644)
	_, snap2 := run(proto.JobBackup, backup)
	if snap2 == snap1 {
		t.Fatal("zweites Backup hat keine eigene ID")
	}
	if added := chunks() - first; added > 4 {
		t.Errorf("inkrementelles Backup hat %d neue Chunks geschrieben", added)
	}
	var parent string
	s.db.QueryRow(`SELECT json_extract(params,'$.parent') FROM jobs WHERE snapshot_id=?`, snap2).Scan(&parent)
	if parent != snap1 {
		t.Errorf("zweites Backup baut auf %q auf, erwartet %q", parent, snap1)
	}

	// both backups restore to exactly the state they were taken from
	want2 := readTree(t, src)
	want1 := map[string]string{}
	for name, data := range files {
		want1[name] = string(data)
	}
	for i, c := range []struct {
		snap string
		want map[string]string
	}{{snap1, want1}, {snap2, want2}} {
		target := filepath.Join(t.TempDir(), fmt.Sprintf("ziel%d", i))
		run(proto.JobRestoreFiles, proto.RestoreFilesParams{SnapshotID: c.snap, Volume: src, Target: target, Overwrite: "always"})
		got := readTree(t, target)
		if len(got) != len(c.want) {
			t.Errorf("Backup %d: %d Dateien wiederhergestellt, erwartet %d: %v", i+1, len(got), len(c.want), keys(got))
		}
		for name, data := range c.want {
			if g, ok := got[name]; !ok {
				t.Errorf("Backup %d: %s fehlt", i+1, name)
			} else if g != data {
				t.Errorf("Backup %d: %s hat anderen Inhalt (%d statt %d Bytes)", i+1, name, len(g), len(data))
			}
		}
	}

	// the server's own checks agree
	for _, id := range []string{snap1, snap2} {
		snap, err := s.loadSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if m, err := missingChunks(snap, st); err != nil || m != 0 {
			t.Errorf("%s: %d Chunks fehlen, %v", id, m, err)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
