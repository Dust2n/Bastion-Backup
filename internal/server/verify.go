package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"bastion/internal/repo"
)

// Restore check: reads a backup completely on the server, like a restore
// would, without writing it anywhere. Every chunk is decrypted and its
// checksum compared; image backups are also checked for the files a
// machine needs to boot. It runs on demand and on a schedule (latest
// backup of every machine, every verify_days days).

const jobVerify = "verify"

var verifyMu sync.Mutex // one check at a time; they read a lot of data

func (s *Server) startVerify(snapID, by string) (int64, error) {
	var host string
	if err := s.db.QueryRow(`SELECT COALESCE(a.hostname,'?') FROM snapshots s LEFT JOIN agents a ON a.id=s.agent_id WHERE s.id=? AND s.deleted=0`, snapID).Scan(&host); err != nil {
		return 0, errors.New("Backup nicht gefunden")
	}
	res, err := s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,started,created_by,message,snapshot_id) VALUES('',0,?,?,?,?,?,?,?,?)`,
		jobVerify, jsonStr(map[string]string{"snapshot": snapID, "hostname": host}), "running", now(), now(), by, "Prüfung von "+host+" wartet", snapID)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	s.publishJob(id)
	go s.runVerify(id, snapID, host)
	return id, nil
}

func (s *Server) runVerify(job int64, snapID, host string) {
	verifyMu.Lock()
	defer verifyMu.Unlock()
	logf := func(format string, args ...any) {
		s.appendLog(job, []string{time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)})
	}
	finish := func(ok bool, msg string) {
		logf("%s", msg)
		status := "success"
		if !ok {
			status = "failed"
		}
		s.db.Exec(`UPDATE jobs SET status=?,message=?,finished=?,percent=CASE WHEN ? THEN 100 ELSE percent END WHERE id=?`, status, msg, now(), ok, job)
		s.db.Exec(`UPDATE snapshots SET verified=?,verify_ok=?,verify_msg=? WHERE id=?`, now(), ok, msg, snapID)
		s.publishJob(job)
		if !ok {
			go s.notifyFailure(job)
		}
	}
	s.db.Exec(`UPDATE jobs SET message=? WHERE id=?`, "Prüfe Backup von "+host, job)
	s.publishJob(job)
	snap, err := s.loadSnapshot(snapID)
	if err != nil {
		finish(false, "Backup nicht lesbar: "+err.Error())
		return
	}
	st, err := s.snapStore(snapID)
	if err != nil {
		finish(false, "Speicherort nicht verfügbar: "+err.Error())
		return
	}
	logf("Prüfe Backup %s von %s (%s, %s)", snapID, host, snap.Kind, snap.Time.Local().Format("02.01.2006 15:04"))

	// 1. structure: all directory trees readable, all chunks present
	ids := map[repo.ID]struct{}{}
	paths := map[string]map[string]int64{} // volume -> lower-case path -> size
	for _, v := range snap.Volumes {
		pm := map[string]int64{}
		paths[v.Key] = pm
		for _, id := range v.Tree {
			ids[id] = struct{}{}
		}
		err := repo.ReadTree(repo.NewChunkReader(v.Tree, st.Get), func(n *repo.Node) error {
			for _, id := range n.Chunks {
				ids[id] = struct{}{}
			}
			if bootRelevant(n.Path) {
				pm[strings.ToLower(n.Path)] = n.Size
			}
			return nil
		})
		if err != nil {
			finish(false, fmt.Sprintf("Verzeichnisbaum von %s nicht lesbar: %v", v.Key, err))
			return
		}
	}
	logf("%d Datenblöcke zu prüfen", len(ids))

	// 2. content: every chunk decrypted and checked
	start, last := time.Now(), time.Now()
	done, bad := 0, 0
	var firstBad string
	for id := range ids {
		if err := st.Verify(id); err != nil {
			bad++
			if firstBad == "" {
				firstBad = fmt.Sprintf("%s: %v", id, err)
			}
		}
		done++
		if time.Since(last) > 2*time.Second {
			last = time.Now()
			s.db.Exec(`UPDATE jobs SET percent=?,files=?,message=? WHERE id=?`, float64(done)*95/float64(len(ids)), done,
				fmt.Sprintf("Prüfe Backup von %s: %d von %d Blöcken", host, done, len(ids)), job)
			s.trackSpeed(job, int64(done), 0)
			s.publishJob(job)
		}
	}
	s.db.Exec(`UPDATE jobs SET files=? WHERE id=?`, done, job)
	if bad > 0 {
		finish(false, fmt.Sprintf("%d von %d Datenblöcken fehlen oder sind beschädigt (z. B. %s) – Wiederherstellung wäre unvollständig", bad, len(ids), firstBad))
		return
	}
	logf("Alle %d Datenblöcke lesbar und unverändert (%s)", len(ids), time.Since(start).Round(time.Second))

	// 3. bootability of image backups
	if snap.Kind == "image" {
		if snap.Layout == nil {
			finish(false, "Image ohne Datenträger-Layout – Wiederherstellung auf leere Festplatte nicht möglich")
			return
		}
		if msg := bootCheck(snap.OS, paths); msg != "" {
			finish(false, msg)
			return
		}
		logf("Startdateien vorhanden, Datenträger-Layout vollständig")
	}
	finish(true, fmt.Sprintf("Backup vollständig wiederherstellbar: %d Datenblöcke geprüft in %s", len(ids), time.Since(start).Round(time.Second)))
}

// bootRelevant keeps the paths bootCheck looks at (the rest of the tree
// is not kept in memory).
func bootRelevant(p string) bool {
	l := strings.ToLower(p)
	switch {
	case l == "windows/system32/config/system", l == "windows/system32/ntoskrnl.exe",
		l == "windows/system32/winload.efi", l == "windows/system32/winload.exe",
		l == "etc/fstab", l == "sbin/init", l == "usr/sbin/init", l == "usr/lib/systemd/systemd":
		return true
	case strings.HasPrefix(l, "boot/vmlinuz"), strings.HasPrefix(l, "vmlinuz"):
		return true
	}
	return false
}

func bootCheck(osName string, paths map[string]map[string]int64) string {
	has := func(p string) bool {
		for _, pm := range paths {
			if pm[p] > 0 {
				return true
			}
		}
		return false
	}
	hasPrefix := func(prefixes ...string) bool {
		for _, pm := range paths {
			for p, size := range pm {
				for _, pre := range prefixes {
					if strings.HasPrefix(p, pre) && size > 0 {
						return true
					}
				}
			}
		}
		return false
	}
	if osName == "windows" {
		var missing []string
		for _, p := range []string{"windows/system32/config/system", "windows/system32/ntoskrnl.exe"} {
			if !has(p) {
				missing = append(missing, p)
			}
		}
		if !has("windows/system32/winload.efi") && !has("windows/system32/winload.exe") {
			missing = append(missing, "windows/system32/winload.efi")
		}
		if len(missing) > 0 {
			return "Windows-Startdateien fehlen im Backup: " + strings.Join(missing, ", ")
		}
		return ""
	}
	if !has("etc/fstab") {
		return "Linux-Systemdateien fehlen im Backup (etc/fstab)"
	}
	if !hasPrefix("boot/vmlinuz", "vmlinuz") {
		return "Linux-Kernel fehlt im Backup (/boot/vmlinuz*)"
	}
	return ""
}

func (s *Server) handleSnapshotVerify(w http.ResponseWriter, r *http.Request, u *user) {
	id, err := s.startVerify(r.PathValue("id"), u.Name)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	s.audit(u.Name, "snapshot_verify", r.PathValue("id"))
	writeJSON(w, map[string]any{"job_id": id})
}

// scheduledVerify checks the latest backup of every machine that was not
// checked within verify_days.
func (s *Server) scheduledVerify() {
	days := s.settingInt("verify_days", 7)
	if days <= 0 {
		return
	}
	rows, err := s.db.Query(`SELECT s.id FROM snapshots s
		WHERE s.deleted=0 AND s.time=(SELECT MAX(time) FROM snapshots WHERE agent_id=s.agent_id AND deleted=0)
		AND NOT EXISTS (SELECT 1 FROM snapshots v WHERE v.agent_id=s.agent_id AND v.deleted=0 AND v.verified>?)`, now()-int64(days)*86400)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		s.startVerify(id, "Zeitplan")
	}
}
