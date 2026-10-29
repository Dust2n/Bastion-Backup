package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bastion/internal/layout"
	"bastion/internal/repo"
)

const jobStorageMove = "storage_move"

// startMove moves the existing backups of a plan to another storage. It
// runs on the server and shows up as a normal job.
func (s *Server) startMove(planID, to int64, by string) (int64, error) {
	if _, err := s.storeByID(to); err != nil {
		return 0, err
	}
	s.storeMu.Lock()
	if s.moving[planID] {
		s.storeMu.Unlock()
		return 0, errors.New("die Backups dieses Plans werden bereits verschoben")
	}
	s.moving[planID] = true
	s.storeMu.Unlock()
	res, err := s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,started,created_by,message) VALUES('',?,?,?,?,?,?,?,?)`,
		planID, jobStorageMove, jsonStr(map[string]int64{"to": to}), "running", now(), now(), by, "warte auf laufende Backups")
	if err != nil {
		s.storeMu.Lock()
		delete(s.moving, planID)
		s.storeMu.Unlock()
		return 0, err
	}
	id, _ := res.LastInsertId()
	go s.runMove(id, planID, to)
	return id, nil
}

func (s *Server) moveCanceled(job int64) bool {
	var c bool
	s.db.QueryRow(`SELECT cancel FROM jobs WHERE id=?`, job).Scan(&c)
	return c
}

func (s *Server) runMove(job, planID, to int64) {
	defer func() {
		s.storeMu.Lock()
		delete(s.moving, planID)
		s.storeMu.Unlock()
	}()
	logf := func(format string, args ...any) {
		s.appendLog(job, []string{time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)})
	}
	finish := func(status, msg string) {
		logf("%s", msg)
		s.db.Exec(`UPDATE jobs SET status=?,message=?,finished=?,percent=CASE WHEN ?='success' THEN 100 ELSE percent END WHERE id=?`,
			status, msg, now(), status, job)
		s.publishJob(job)
		if status == "failed" {
			go s.notifyFailure(job)
		}
	}
	// new backups of the plan are held back (s.moving); wait for running ones
	for i := 0; ; i++ {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='backup' AND status='running' AND plan_id=?`, planID).Scan(&n)
		if n == 0 {
			break
		}
		if i > 720 || s.moveCanceled(job) {
			finish("failed", "Verschieben abgebrochen: ein Backup des Plans läuft noch")
			return
		}
		time.Sleep(10 * time.Second)
	}
	dst, err := s.storeByID(to)
	if err != nil {
		finish("failed", err.Error())
		return
	}
	dstName := fmt.Sprint(to)
	if row, _, err := s.storageRow(to); err == nil {
		dstName = row.Name
	}

	type snapRef struct {
		id, data string
		from     int64
	}
	rows, err := s.db.Query(`SELECT id,data,storage_id FROM snapshots WHERE plan_id=? AND deleted=0 AND storage_id<>? ORDER BY time`, planID, to)
	if err != nil {
		finish("failed", err.Error())
		return
	}
	var snaps []snapRef
	for rows.Next() {
		var x snapRef
		rows.Scan(&x.id, &x.data, &x.from)
		snaps = append(snaps, x)
	}
	rows.Close()
	if len(snaps) == 0 {
		finish("success", "Keine Backups zu verschieben")
		return
	}
	logf("Verschiebe %d Backups nach %s", len(snaps), dstName)

	var chunks, bytes int64
	start := time.Now()
	lastReport := time.Now()
	for i, x := range snaps {
		src, err := s.storeByID(x.from)
		if err != nil {
			finish("failed", fmt.Sprintf("Quelle nicht verfügbar: %v", err))
			return
		}
		var snap repo.Snapshot
		if err := json.Unmarshal([]byte(x.data), &snap); err != nil {
			finish("failed", fmt.Sprintf("Backup %s nicht lesbar", x.id))
			return
		}
		ids := map[repo.ID]struct{}{}
		if err := snap.Refs(src.Get, func(id repo.ID) { ids[id] = struct{}{} }); err != nil {
			finish("failed", fmt.Sprintf("Backup %s unvollständig: %v", x.id, err))
			return
		}
		done := 0
		for id := range ids {
			if done%200 == 0 && s.moveCanceled(job) {
				finish("canceled", fmt.Sprintf("Abgebrochen; %d von %d Backups verschoben", i, len(snaps)))
				return
			}
			n, err := src.CopyTo(dst, id)
			if err == nil {
				err = dst.Verify(id)
			}
			if err != nil {
				finish("failed", fmt.Sprintf("Chunk %s: %v", id, err))
				return
			}
			done++
			chunks++
			bytes += n
			if time.Since(lastReport) > 2*time.Second {
				lastReport = time.Now()
				pct := (float64(i) + float64(done)/float64(len(ids))) * 100 / float64(len(snaps))
				s.db.Exec(`UPDATE jobs SET percent=?,bytes=?,files=?,message=? WHERE id=?`, min(99, pct), bytes, chunks,
					fmt.Sprintf("Backup %d von %d, %s kopiert", i+1, len(snaps), layout.Human(bytes)), job)
				s.trackSpeed(job, bytes, 0)
				s.publishJob(job)
			}
		}
		// all chunks are in place and verified: switch the snapshot over
		os.MkdirAll(filepath.Join(dst.Dir(), "snapshots"), 0o700)
		os.WriteFile(filepath.Join(dst.Dir(), "snapshots", x.id+".json"), []byte(x.data), 0o600)
		s.db.Exec(`UPDATE snapshots SET storage_id=? WHERE id=?`, to, x.id)
		logf("Backup %s verschoben (%d Chunks)", x.id, len(ids))
	}
	go s.updateRepoStats()
	s.db.Exec(`UPDATE jobs SET bytes=?,files=? WHERE id=?`, bytes, chunks, job)
	finish("success", fmt.Sprintf("%d Backups nach %s verschoben, %s kopiert in %s. Der alte Speicherort wird bei der nächsten Bereinigung freigegeben.",
		len(snaps), dstName, layout.Human(bytes), time.Since(start).Round(time.Second)))
}
