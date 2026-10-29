package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"bastion/internal/layout"
	"bastion/internal/proto"
	"bastion/internal/repo"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func (s *Server) scheduler(ctx context.Context) {
	last := time.Now()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := time.Now()
		rows, err := s.db.Query(`SELECT id,schedule FROM plans WHERE enabled=1 AND schedule<>''`)
		if err == nil {
			type ps struct {
				id  int64
				sch string
			}
			var due []ps
			for rows.Next() {
				var p ps
				rows.Scan(&p.id, &p.sch)
				sched, err := cronParser.Parse(p.sch)
				if err == nil && !sched.Next(last).After(cur) {
					due = append(due, p)
				}
			}
			rows.Close()
			for _, p := range due {
				pl, err := s.getPlan(p.id)
				if err != nil {
					continue
				}
				for _, a := range s.planAgents(pl) {
					s.enqueue(a, p.id, proto.JobBackup, proto.BackupParams{PlanID: p.id}, "Zeitplan")
				}
			}
		}
		if crossed(last, cur, s.setting("gc_time", "03:30")) {
			go func() {
				if msg, err := s.runGC("Zeitplan"); err != nil {
					s.log.Printf("Bereinigung: %v", err)
				} else {
					s.log.Printf("Bereinigung: %s", msg)
				}
			}()
		}
		if crossed(last, cur, s.setting("scrub_time", "")) {
			go s.runScrub("Zeitplan")
		}
		if crossed(last, cur, s.setting("verify_time", "04:30")) {
			go s.scheduledVerify()
		}
		if s.setting("dr_passphrase", "") != "" && crossed(last, cur, s.setting("dr_time", "02:15")) {
			go s.runServerBackup("Zeitplan")
		}
		if crossed(last, cur, "08:00") {
			go s.notifyStale()
		}
		s.retryRollouts()
		// agents that just went offline
		if orows, err := s.db.Query(`SELECT id FROM agents WHERE last_seen>? AND last_seen<=? AND removed=0`, cur.Unix()-150, cur.Unix()-120); err == nil {
			for orows.Next() {
				var id string
				orows.Scan(&id)
				s.publish("agent", map[string]any{"id": id, "online": false})
			}
			orows.Close()
		}
		// image restores that never came back
		s.db.Exec(`UPDATE jobs SET status='failed',message='Wiederherstellung hat sich innerhalb von 48 Stunden nicht gemeldet',finished=?
			WHERE status='waiting' AND created<?`, now(), now()-48*3600)
		s.db.Exec(`DELETE FROM restore_tokens WHERE expires<?`, now())
		last = cur
	}
}

// crossed reports whether the local time hh:mm lies in (a, b].
func crossed(a, b time.Time, hhmm string) bool {
	var h, m int
	if _, err := fmt.Sscanf(hhmm, "%d:%d", &h, &m); err != nil {
		return false
	}
	t := time.Date(b.Year(), b.Month(), b.Day(), h, m, 0, 0, b.Location())
	return t.After(a) && !t.After(b)
}

// applyRetention marks snapshots the plan no longer keeps as deleted.
func (s *Server) applyRetention(agentID string, planID int64) {
	if planID == 0 {
		return
	}
	p, err := s.getPlan(planID)
	if err != nil || p.KeepLast+p.KeepDaily+p.KeepWeekly+p.KeepMonthly == 0 {
		return
	}
	rows, err := s.db.Query(`SELECT id,time,keep FROM snapshots WHERE agent_id=? AND plan_id=? AND deleted=0 ORDER BY time DESC`, agentID, planID)
	if err != nil {
		return
	}
	type sn struct {
		id   string
		t    time.Time
		keep bool
	}
	var list []sn
	for rows.Next() {
		var x sn
		var ts int64
		rows.Scan(&x.id, &ts, &x.keep)
		x.t = time.Unix(ts, 0)
		list = append(list, x)
	}
	rows.Close()
	keep := map[string]bool{}
	bucket := func(n int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, x := range list {
			if len(seen) >= n {
				return
			}
			k := key(x.t)
			if !seen[k] {
				seen[k] = true
				keep[x.id] = true
			}
		}
	}
	for i, x := range list {
		if i < p.KeepLast || x.keep {
			keep[x.id] = true
		}
	}
	bucket(p.KeepDaily, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(p.KeepWeekly, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprint(y, w) })
	bucket(p.KeepMonthly, func(t time.Time) string { return t.Format("2006-01") })
	for _, x := range list {
		if !keep[x.id] {
			s.db.Exec(`UPDATE snapshots SET deleted=1 WHERE id=?`, x.id)
		}
	}
}

type repoStats struct {
	Chunks  int64  `json:"chunks"`
	Bytes   int64  `json:"bytes"`
	Freed   int64  `json:"freed"`
	Time    int64  `json:"time"`
	Message string `json:"message"`
}

// runGC deletes chunks that no snapshot references any more.
func (s *Server) runGC(by string) (string, error) {
	s.gcMu.Lock()
	if s.gcRunning {
		s.gcMu.Unlock()
		return "", errors.New("Bereinigung läuft bereits")
	}
	s.gcRunning = true
	s.gcMu.Unlock()
	defer func() {
		s.gcMu.Lock()
		s.gcRunning = false
		s.gcMu.Unlock()
	}()

	// new backups are held back while gcRunning is set; wait for running ones
	for i := 0; ; i++ {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='backup' AND status='running'`).Scan(&n)
		if n == 0 {
			break
		}
		if i > 720 {
			return "", errors.New("Bereinigung übersprungen: Backups laufen seit über 2 Stunden")
		}
		time.Sleep(10 * time.Second)
	}

	// deleted backups go first, so their chunks become unreferenced
	drows, _ := s.db.Query(`SELECT id,storage_id FROM snapshots WHERE deleted=1`)
	type gone struct {
		id      string
		storage int64
	}
	var removed []gone
	for drows.Next() {
		var g gone
		drows.Scan(&g.id, &g.storage)
		removed = append(removed, g)
	}
	drows.Close()
	for _, g := range removed {
		os.Remove(s.path("snapshots", g.id+".json"))
		os.Remove(s.path("cache", g.id+".idx"))
		if st, err := s.storeByID(g.storage); err == nil {
			os.Remove(filepath.Join(st.Dir(), "snapshots", g.id+".json"))
		}
		s.db.Exec(`DELETE FROM snapshots WHERE id=?`, g.id)
	}

	var total repoStats
	var msgs []string
	for _, sid := range s.storageIDs() {
		st, err := s.gcStorage(sid)
		if err != nil {
			msgs = append(msgs, err.Error())
			continue
		}
		total.Chunks += st.Chunks
		total.Bytes += st.Bytes
		total.Freed += st.Freed
	}
	total.Time = now()
	total.Message = fmt.Sprintf("%d Backups entfernt, %s freigegeben, %d Chunks (%s) belegt",
		len(removed), layout.Human(total.Freed), total.Chunks, layout.Human(total.Bytes))
	if len(msgs) > 0 {
		total.Message += "; Fehler: " + strings.Join(msgs, "; ")
	}
	s.setSetting("repo_stats", jsonStr(total))
	s.audit(by, "gc", total.Message)
	return total.Message, nil
}

// gcStorage deletes the unreferenced chunks of one storage.
func (s *Server) gcStorage(sid int64) (*storageStats, error) {
	store, err := s.storeByID(sid)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprint(sid)
	if row, _, err := s.storageRow(sid); err == nil {
		name = row.Name
	}
	live := map[repo.ID]struct{}{}
	rows, err := s.db.Query(`SELECT id,data FROM snapshots WHERE deleted=0 AND storage_id=?`, sid)
	if err != nil {
		return nil, err
	}
	type sd struct{ id, data string }
	var snaps []sd
	for rows.Next() {
		var x sd
		rows.Scan(&x.id, &x.data)
		snaps = append(snaps, x)
	}
	rows.Close()
	for _, x := range snaps {
		var snap repo.Snapshot
		if err := json.Unmarshal([]byte(x.data), &snap); err != nil {
			return nil, fmt.Errorf("%s: Backup %s nicht lesbar, Bereinigung abgebrochen", name, x.id)
		}
		if err := snap.Refs(store.Get, func(id repo.ID) { live[id] = struct{}{} }); err != nil {
			return nil, fmt.Errorf("%s: Backup %s unvollständig (%v), nichts gelöscht", name, x.id, err)
		}
	}
	// chunks younger than 6 hours may belong to a running upload or move
	cutoff := time.Now().Add(-6 * time.Hour)
	st := &storageStats{Time: now()}
	err = store.Walk(func(c repo.ChunkInfo) error {
		if _, ok := live[c.ID]; !ok && c.ModTime.Before(cutoff) {
			if store.Delete(c.ID) == nil {
				st.Freed += c.Size
			}
			return nil
		}
		st.Chunks++
		st.Bytes += c.Size
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.setSetting(fmt.Sprintf("storage_stats:%d", sid), jsonStr(st))
	return st, nil
}

// updateRepoStats counts stored chunks per storage (at startup).
func (s *Server) updateRepoStats() {
	var total repoStats
	json.Unmarshal([]byte(s.setting("repo_stats", "{}")), &total)
	total.Chunks, total.Bytes = 0, 0
	for _, sid := range s.storageIDs() {
		store, err := s.storeByID(sid)
		if err != nil {
			continue
		}
		st := storageStats{Time: now()}
		store.Walk(func(c repo.ChunkInfo) error {
			st.Chunks++
			st.Bytes += c.Size
			return nil
		})
		s.setSetting(fmt.Sprintf("storage_stats:%d", sid), jsonStr(st))
		total.Chunks += st.Chunks
		total.Bytes += st.Bytes
	}
	if total.Message == "" {
		total.Message = fmt.Sprintf("%d Chunks (%s) belegt", total.Chunks, layout.Human(total.Bytes))
	}
	s.setSetting("repo_stats", jsonStr(total))
}

var scrubMu sync.Mutex

// runScrub verifies every chunk of every storage (decrypt + hash).
func (s *Server) runScrub(by string) {
	if !scrubMu.TryLock() {
		return
	}
	defer scrubMu.Unlock()
	var ok, bad int64
	var bads []string
	for _, sid := range s.storageIDs() {
		store, err := s.storeByID(sid)
		if err != nil {
			bads = append(bads, err.Error())
			continue
		}
		store.Walk(func(c repo.ChunkInfo) error {
			if err := store.Verify(c.ID); err != nil {
				bad++
				if len(bads) < 50 {
					bads = append(bads, err.Error())
				}
			} else {
				ok++
			}
			return nil
		})
	}
	msg := fmt.Sprintf("%d Chunks geprüft, %d fehlerhaft", ok+bad, bad)
	s.setSetting("scrub_result", jsonStr(map[string]any{"time": now(), "ok": ok, "bad": bad, "errors": bads, "message": msg}))
	s.audit(by, "scrub", msg)
	if bad > 0 {
		s.sendMail("[Bastion] Integritätsprüfung: fehlerhafte Daten", msg+"\n\n"+strings.Join(bads, "\n"))
	}
}

// agentBinary is a published agent version for updates and rollout.
type agentBinary struct {
	OSArch    string `json:"os_arch"`
	Version   string `json:"version"`
	Path      string `json:"path"` // relative to data dir, e.g. agents/windows-amd64/bastion-agent.exe
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
	Size      int64  `json:"size"`
}

var (
	binMu    sync.Mutex
	binaries = map[string]agentBinary{}
)

// scanAgentBinaries reads data/agents/<os>-<arch>/{bastion-agent[.exe],VERSION}
// and signs them for the agents' update check.
func (s *Server) scanAgentBinaries() {
	dirs, _ := os.ReadDir(s.path("agents"))
	found := map[string]agentBinary{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		for _, name := range []string{"bastion-agent.exe", "bastion-agent"} {
			p := s.path("agents", d.Name(), name)
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			h := sha256.New()
			n, _ := io.Copy(h, f)
			f.Close()
			sum := hex.EncodeToString(h.Sum(nil))
			ver, _ := os.ReadFile(s.path("agents", d.Name(), "VERSION"))
			found[d.Name()] = agentBinary{
				OSArch: d.Name(), Version: strings.TrimSpace(string(ver)), Path: "agents/" + d.Name() + "/" + name,
				SHA256: sum, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.signKey, []byte(sum))), Size: n,
			}
		}
	}
	binMu.Lock()
	binaries = found
	binMu.Unlock()
}

func agentBinaryFor(osName, arch string) (agentBinary, bool) {
	binMu.Lock()
	defer binMu.Unlock()
	b, ok := binaries[osName+"-"+arch]
	return b, ok
}

func listBinaries() []agentBinary {
	binMu.Lock()
	defer binMu.Unlock()
	var out []agentBinary
	for _, b := range binaries {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OSArch < out[j].OSArch })
	return out
}

// sendMail sends a notification if SMTP is configured.
func (s *Server) sendMail(subject, body string) error {
	host := s.setting("smtp_host", "")
	to := s.setting("notify_to", "")
	if host == "" || to == "" {
		return nil
	}
	port := s.setting("smtp_port", "587")
	from := s.setting("smtp_from", "bastion@"+host)
	user := s.setting("smtp_user", "")
	pass := ""
	if enc := s.setting("smtp_pass", ""); enc != "" {
		b, _ := base64.StdEncoding.DecodeString(enc)
		pass, _ = s.unseal(b)
	}
	var rcpts []string
	for _, r := range strings.Split(to, ",") {
		if r = strings.TrimSpace(r); r != "" {
			rcpts = append(rcpts, r)
		}
	}
	msg := "From: " + from + "\r\nTo: " + strings.Join(rcpts, ", ") + "\r\nSubject: " + mimeHeader(subject) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\nDate: " +
		time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n")
	addr := net.JoinHostPort(host, port)
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, pass, host)
	}
	if port == "465" {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer c.Close()
		if auth != nil {
			if err := c.Auth(auth); err != nil {
				return err
			}
		}
		if err := c.Mail(from); err != nil {
			return err
		}
		for _, r := range rcpts {
			if err := c.Rcpt(r); err != nil {
				return err
			}
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		w.Write([]byte(msg))
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	return smtp.SendMail(addr, auth, from, rcpts, []byte(msg))
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?utf-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

var jobNames = map[string]string{
	proto.JobBackup: "Backup", proto.JobRestoreFiles: "Datei-Wiederherstellung", proto.JobRestoreImage: "Image-Wiederherstellung",
	proto.JobUpdate: "Agent-Update", proto.JobUninstall: "Deinstallation",
	jobStorageMove: "Backups verschieben", jobVerify: "Restore-Prüfung",
}

func (s *Server) notifyFailure(jobID int64) {
	if s.setting("notify_failure", "1") != "1" {
		return
	}
	var typ, msg, logText, host string
	s.db.QueryRow(`SELECT j.type,j.message,j.log,COALESCE(a.hostname,j.agent_id) FROM jobs j LEFT JOIN agents a ON a.id=j.agent_id WHERE j.id=?`, jobID).
		Scan(&typ, &msg, &logText, &host)
	if len(logText) > 4000 {
		logText = "...\n" + logText[len(logText)-4000:]
	}
	body := fmt.Sprintf("%s auf %s ist fehlgeschlagen (Auftrag %d):\n\n%s\n\nProtokoll:\n%s\n\n%s/#/jobs/%d\n",
		jobNames[typ], host, jobID, msg, logText, s.PublicURL(), jobID)
	if err := s.sendMail(fmt.Sprintf("[Bastion] Fehler: %s auf %s", jobNames[typ], host), body); err != nil {
		s.log.Printf("E-Mail: %v", err)
	}
}

func (s *Server) notifyStale() {
	days := s.settingInt("notify_stale_days", 3)
	if days <= 0 {
		return
	}
	limit := now() - int64(days)*86400
	rows, err := s.db.Query(`SELECT a.hostname, a.last_seen, COALESCE(MAX(sn.time),0) FROM agents a
		LEFT JOIN snapshots sn ON sn.agent_id=a.id AND sn.deleted=0 WHERE a.enrolled<? AND a.removed=0 GROUP BY a.id`, limit)
	if err != nil {
		return
	}
	var lines []string
	for rows.Next() {
		var host string
		var seen, last int64
		rows.Scan(&host, &seen, &last)
		if last < limit {
			when := "noch nie"
			if last > 0 {
				when = "zuletzt " + time.Unix(last, 0).Format("02.01.2006 15:04")
			}
			lines = append(lines, fmt.Sprintf("- %s: %s gesichert, zuletzt online %s", host, when, time.Unix(seen, 0).Format("02.01.2006 15:04")))
		}
	}
	rows.Close()
	if len(lines) > 0 {
		s.sendMail(fmt.Sprintf("[Bastion] %d Rechner ohne aktuelles Backup", len(lines)),
			fmt.Sprintf("Folgende Rechner haben seit mehr als %d Tagen kein Backup:\n\n%s\n\n%s\n", days, strings.Join(lines, "\n"), s.PublicURL()))
	}
}
