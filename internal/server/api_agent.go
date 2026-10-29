package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bastion/internal/proto"
	"bastion/internal/repo"
)

func (s *Server) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/agent/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/agent/poll", s.agentOnly(s.handlePoll))
	mux.HandleFunc("POST /api/agent/goodbye", s.agentOnly(s.handleGoodbye))
	mux.HandleFunc("POST /api/agent/jobs/{id}/progress", s.agentOrRestore(s.handleProgress))
	mux.HandleFunc("POST /api/agent/jobs/{id}/result", s.agentOrRestore(s.handleResult))
	mux.HandleFunc("POST /api/agent/jobs/{id}/stale", s.agentOnly(s.handleStale))
	mux.HandleFunc("POST /api/agent/chunks/missing", s.agentOnly(s.handleMissing))
	mux.HandleFunc("PUT /api/agent/chunks/{id}", s.agentOnly(s.handlePutChunk))
	mux.HandleFunc("GET /api/agent/chunks/{id}", s.agentOrRestore(s.handleGetChunk))
	mux.HandleFunc("POST /api/agent/snapshots", s.agentOnly(s.handlePostSnapshot))
	mux.HandleFunc("GET /api/agent/snapshots/{id}", s.agentOrRestore(s.handleGetSnapshot))
	mux.HandleFunc("GET /api/agent/download/{path...}", s.agentOrRestore(s.handleDownload))
	mux.HandleFunc("GET /dl/agents/{path...}", s.handlePublicAgentDownload)
	mux.HandleFunc("GET /api/rescue/info", s.agentOrRestore(s.handleRescueInfo))
	mux.HandleFunc("POST /api/rescue/code", s.handleRescueCode)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req proto.EnrollRequest
	if err := readJSON(r, &req); err != nil || req.Token == "" || req.Hostname == "" {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if !s.login.allow("enroll:" + clientIP(r)) {
		httpErr(w, 429, "zu viele Versuche")
		return
	}
	var grp string
	var planID, expires, uses int64
	th := sha(req.Token)
	err := s.db.QueryRow(`SELECT grp,plan_id,expires,uses FROM tokens WHERE hash=?`, th).Scan(&grp, &planID, &expires, &uses)
	if err != nil || (expires > 0 && expires < now()) || uses == 0 {
		s.audit("-", "enroll_rejected", req.Hostname+" "+clientIP(r))
		httpErr(w, 403, "Anmelde-Token ungültig oder abgelaufen")
		return
	}
	if uses > 0 {
		s.db.Exec(`UPDATE tokens SET uses=uses-1 WHERE hash=?`, th)
	}

	// A reinstalled machine (same name and MAC) keeps its ID and history.
	id := ""
	rows, _ := s.db.Query(`SELECT id,macs FROM agents WHERE lower(hostname)=lower(?)`, req.Hostname)
	for rows.Next() {
		var aid, macs string
		rows.Scan(&aid, &macs)
		for _, m := range req.MACs {
			if m != "" && strings.Contains(strings.ToLower(macs), strings.ToLower(m)) {
				id = aid
			}
		}
	}
	rows.Close()
	secret := randToken(32)
	if id == "" {
		id = hex.EncodeToString(random(8))
		_, err = s.db.Exec(`INSERT INTO agents(id,hostname,os,arch,version,ips,macs,secret_hash,grp,enrolled,last_seen) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, req.Hostname, req.OS, req.Arch, req.Version, jsonStr(req.IPs), jsonStr(req.MACs), sha(secret), grp, now(), now())
	} else {
		_, err = s.db.Exec(`UPDATE agents SET secret_hash=?,os=?,arch=?,version=?,ips=?,macs=?,last_seen=?,removed=0 WHERE id=?`,
			sha(secret), req.OS, req.Arch, req.Version, jsonStr(req.IPs), jsonStr(req.MACs), now(), id)
	}
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if planID > 0 {
		s.db.Exec(`INSERT OR IGNORE INTO plan_targets(plan_id,target) VALUES(?,?)`, planID, "agent:"+id)
	}
	s.db.Exec(`UPDATE rollouts SET status='done',message='Agent angemeldet',agent_id=?,updated=? WHERE token_hash=?`, id, now(), th)
	s.audit("-", "enroll", fmt.Sprintf("%s (%s) als %s", req.Hostname, clientIP(r), id))
	writeJSON(w, proto.EnrollResponse{AgentID: id, Secret: secret,
		SigningKey: base64.StdEncoding.EncodeToString(s.signKey.Public().(ed25519.PublicKey))})
}

// handleGoodbye is called by an agent that is being uninstalled. The agent
// is kept with its backups, marked as removed and its secret revoked; it
// no longer takes part in plans and does not show up as "offline".
func (s *Server) handleGoodbye(w http.ResponseWriter, r *http.Request, p *principal) {
	var req struct {
		Reason string `json:"reason"`
	}
	readJSON(r, &req)
	var host string
	s.db.QueryRow(`SELECT hostname FROM agents WHERE id=?`, p.AgentID).Scan(&host)
	s.db.Exec(`UPDATE agents SET removed=?,secret_hash='' WHERE id=?`, now(), p.AgentID)
	s.db.Exec(`UPDATE jobs SET status='canceled',message='Agent deinstalliert',finished=? WHERE agent_id=? AND status IN ('queued','waiting')`, now(), p.AgentID)
	s.audit("-", "agent_removed", fmt.Sprintf("%s (%s): %s", host, p.AgentID, req.Reason))
	s.publish("agent", map[string]any{"id": p.AgentID, "online": false, "removed": true})
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request, p *principal) {
	var info proto.AgentInfo
	if err := readJSON(r, &info); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	var prevSeen int64
	s.db.QueryRow(`SELECT last_seen FROM agents WHERE id=?`, p.AgentID).Scan(&prevSeen)
	if now()-prevSeen >= 120 {
		s.publish("agent", map[string]any{"id": p.AgentID, "online": true})
	}
	q := `UPDATE agents SET hostname=?,os=?,os_version=?,arch=?,version=?,ips=?,macs=?,last_seen=? WHERE id=?`
	args := []any{info.Hostname, info.OS, info.OSVersion, info.Arch, info.Version, jsonStr(info.IPs), jsonStr(info.MACs), now(), p.AgentID}
	if info.Layout != nil {
		q = `UPDATE agents SET hostname=?,os=?,os_version=?,arch=?,version=?,ips=?,macs=?,last_seen=?,layout=? WHERE id=?`
		args = []any{info.Hostname, info.OS, info.OSVersion, info.Arch, info.Version, jsonStr(info.IPs), jsonStr(info.MACs), now(), jsonStr(info.Layout), p.AgentID}
	}
	s.db.Exec(q, args...)
	// jobs the agent forgot about (agent restarted while running); image
	// restores run in the rescue environment, not in the agent
	s.db.Exec(`UPDATE jobs SET status='failed',message='Agent wurde während des Auftrags neu gestartet',finished=?
		WHERE agent_id=? AND status='running' AND id<>? AND type<>'restore_image'`,
		now(), p.AgentID, info.RunningJob)

	wake := s.wakeChan(p.AgentID)
	deadline := time.After(25 * time.Second)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		job, err := s.dispatch(p.AgentID)
		if err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
		if job != nil {
			writeJSON(w, job)
			return
		}
		select {
		case <-wake:
		case <-tick.C:
		case <-deadline:
			s.db.Exec(`UPDATE agents SET last_seen=? WHERE id=?`, now(), p.AgentID)
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// jobFor checks that the principal may report on the job.
func (s *Server) jobFor(r *http.Request, p *principal) (int64, string, string, error) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var agentID, status, typ string
	if err := s.db.QueryRow(`SELECT agent_id,status,type FROM jobs WHERE id=?`, id).Scan(&agentID, &status, &typ); err != nil {
		return 0, "", "", errors.New("Auftrag unbekannt")
	}
	if p.restore() {
		if p.RestoreJob != id {
			return 0, "", "", errors.New("Token gilt nicht für diesen Auftrag")
		}
	} else if agentID != p.AgentID {
		return 0, "", "", errors.New("Auftrag gehört zu einem anderen Agent")
	}
	return id, status, typ, nil
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request, p *principal) {
	id, status, _, err := s.jobFor(r, p)
	if err != nil {
		httpErr(w, 403, "%v", err)
		return
	}
	var pr proto.Progress
	if err := readJSON(r, &pr); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if status == "waiting" {
		s.db.Exec(`UPDATE jobs SET status='running' WHERE id=?`, id)
	}
	s.db.Exec(`UPDATE jobs SET percent=?,message=?,files=?,bytes=?,total=? WHERE id=?`, pr.Percent, pr.Message, pr.Files, pr.Bytes, pr.Total, id)
	s.appendLog(id, pr.Log)
	s.trackSpeed(id, pr.Bytes, pr.Total)
	s.publishJob(id)
	var cancel bool
	s.db.QueryRow(`SELECT cancel FROM jobs WHERE id=?`, id).Scan(&cancel)
	writeJSON(w, proto.ProgressReply{Cancel: cancel})
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, p *principal) {
	id, status, typ, err := s.jobFor(r, p)
	if err != nil {
		httpErr(w, 403, "%v", err)
		return
	}
	var res proto.JobResult
	if err := readJSON(r, &res); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if status != "running" && status != "waiting" && status != "queued" {
		writeJSON(w, map[string]string{"ok": "ignored"})
		return
	}
	st := "failed"
	switch {
	case res.Success && res.Waiting:
		st = "waiting"
	case res.Success:
		st = "success"
	}
	var cancel bool
	s.db.QueryRow(`SELECT cancel FROM jobs WHERE id=?`, id).Scan(&cancel)
	if cancel && !res.Success {
		st = "canceled"
	}
	fin := now()
	if st == "waiting" {
		fin = 0
	}
	pct := 100.0
	if st != "success" {
		pct = 0
	}
	s.db.Exec(`UPDATE jobs SET status=?,message=?,snapshot_id=?,finished=?,percent=CASE WHEN ?>0 THEN ? ELSE percent END WHERE id=?`,
		st, res.Message, res.SnapshotID, fin, pct, pct, id)
	s.appendLog(id, res.Log)
	if st != "waiting" && typ == proto.JobRestoreImage {
		s.db.Exec(`DELETE FROM restore_tokens WHERE job_id=?`, id)
	}
	forgetSpeed(id)
	s.publishJob(id)
	switch st {
	case "failed":
		go s.notifyFailure(id)
	case "success":
		if typ == proto.JobBackup {
			var agentID string
			var planID int64
			s.db.QueryRow(`SELECT agent_id,plan_id FROM jobs WHERE id=?`, id).Scan(&agentID, &planID)
			s.applyRetention(agentID, planID)
		}
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleStale(w http.ResponseWriter, r *http.Request, p *principal) {
	id, status, _, err := s.jobFor(r, p)
	if err != nil {
		httpErr(w, 403, "%v", err)
		return
	}
	var res proto.JobResult
	readJSON(r, &res)
	if status == "waiting" {
		s.db.Exec(`UPDATE jobs SET status='failed',message=?,finished=? WHERE id=?`, res.Message, now(), id)
		s.db.Exec(`DELETE FROM restore_tokens WHERE job_id=?`, id)
		s.publishJob(id)
		go s.notifyFailure(id)
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request, p *principal) {
	var req proto.MissingRequest
	if err := readJSON(r, &req); err != nil || len(req.IDs) > 1000 {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	st, _, err := s.storeFor(p)
	if err != nil {
		httpErr(w, 503, "Speicherort nicht verfügbar: %v", err)
		return
	}
	resp := proto.MissingResponse{Missing: []repo.ID{}}
	for _, id := range req.IDs {
		if !st.Has(id) {
			resp.Missing = append(resp.Missing, id)
		}
	}
	writeJSON(w, resp)
}

func (s *Server) handlePutChunk(w http.ResponseWriter, r *http.Request, p *principal) {
	id, err := repo.ParseID(r.PathValue("id"))
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	st, _, err := s.storeFor(p)
	if err != nil {
		httpErr(w, 503, "Speicherort nicht verfügbar: %v", err)
		return
	}
	if err := st.PutCompressed(id, body); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetChunk(w http.ResponseWriter, r *http.Request, p *principal) {
	id, err := repo.ParseID(r.PathValue("id"))
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	st, _, err := s.storeFor(p)
	if err != nil {
		httpErr(w, 503, "Speicherort nicht verfügbar: %v", err)
		return
	}
	z, err := st.GetCompressed(id)
	if errors.Is(err, repo.ErrNotFound) {
		httpErr(w, 404, "Chunk nicht vorhanden")
		return
	}
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/zstd")
	w.Write(z)
}

func (s *Server) handlePostSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	var snap repo.Snapshot
	if err := readJSON(r, &snap); err != nil || snap.ID == "" || strings.ContainsAny(snap.ID, `/\.`) {
		httpErr(w, 400, "ungültiges Backup")
		return
	}
	snap.AgentID = p.AgentID
	st, storageID, err := s.storeFor(p)
	if err != nil {
		httpErr(w, 503, "Speicherort nicht verfügbar: %v", err)
		return
	}
	var files, bytes, newBytes int64
	for _, v := range snap.Volumes {
		for _, id := range v.Tree {
			if !st.Has(id) {
				httpErr(w, 400, "Verzeichnisbaum unvollständig (%s)", id)
				return
			}
		}
		files += v.Files
		bytes += v.Bytes
		newBytes += v.NewBytes
	}
	data := jsonStr(snap)
	if _, err := s.db.Exec(`INSERT INTO snapshots(id,agent_id,plan_id,time,kind,files,bytes,new_bytes,data,storage_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		snap.ID, snap.AgentID, snap.PlanID, snap.Time.Unix(), snap.Kind, files, bytes, newBytes, data, storageID); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	// also as files, so a storage can be read without the database
	os.WriteFile(s.path("snapshots", snap.ID+".json"), []byte(data), 0o600)
	os.MkdirAll(filepath.Join(st.Dir(), "snapshots"), 0o700)
	os.WriteFile(filepath.Join(st.Dir(), "snapshots", snap.ID+".json"), []byte(data), 0o600)
	go s.buildIndex(snap.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadSnapshot(id string) (*repo.Snapshot, error) {
	var data string
	if err := s.db.QueryRow(`SELECT data FROM snapshots WHERE id=? AND deleted=0`, id).Scan(&data); err != nil {
		return nil, errors.New("Backup nicht gefunden")
	}
	var snap repo.Snapshot
	return &snap, json.Unmarshal([]byte(data), &snap)
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	snap, err := s.loadSnapshot(id)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	allowed := snap.AgentID == p.AgentID && !p.restore()
	if p.restore() {
		allowed = p.RestoreSnap == id
	} else if !allowed {
		// restoring another machine's backup onto this agent
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE agent_id=? AND status='running' AND type='restore_files' AND params LIKE ?`,
			p.AgentID, "%"+id+"%").Scan(&n)
		allowed = n > 0
	}
	if !allowed {
		httpErr(w, 403, "kein Zugriff auf dieses Backup")
		return
	}
	writeJSON(w, snap)
}

func (s *Server) serveDataFile(w http.ResponseWriter, r *http.Request, rel string, allowed ...string) {
	rel = filepath.ToSlash(filepath.Clean("/" + rel))[1:]
	ok := false
	for _, a := range allowed {
		if strings.HasPrefix(rel, a+"/") {
			ok = true
		}
	}
	if !ok {
		httpErr(w, 403, "nicht erlaubt")
		return
	}
	http.ServeFile(w, r, s.path(filepath.FromSlash(rel)))
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, p *principal) {
	s.serveDataFile(w, r, r.PathValue("path"), "agents", "rescue")
}

// Agent binaries are public: push installs download them before enrolling.
func (s *Server) handlePublicAgentDownload(w http.ResponseWriter, r *http.Request) {
	s.serveDataFile(w, r, "agents/"+r.PathValue("path"), "agents")
}

func (s *Server) handleRescueInfo(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.restore() {
		httpErr(w, 403, "nur mit Wiederherstellungs-Token")
		return
	}
	snap, err := s.loadSnapshot(p.RestoreSnap)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	s.db.Exec(`UPDATE jobs SET status='running',message='Wiederherstellungsumgebung verbunden' WHERE id=? AND status IN ('waiting','queued')`, p.RestoreJob)
	s.appendLog(p.RestoreJob, []string{time.Now().Format("15:04:05 ") + "Wiederherstellungsumgebung verbunden von " + clientIP(r)})
	writeJSON(w, proto.RestoreInfo{JobID: p.RestoreJob, Snapshot: snap})
}

func (s *Server) handleRescueCode(w http.ResponseWriter, r *http.Request) {
	if !s.codes.allow(clientIP(r)) {
		httpErr(w, 429, "zu viele Versuche, bitte eine Minute warten")
		return
	}
	var req struct{ Code string }
	if err := readJSON(r, &req); err != nil || req.Code == "" {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	var jobID, expires int64
	var snapID, agentID string
	err := s.db.QueryRow(`SELECT job_id,snapshot_id,agent_id,expires FROM restore_tokens WHERE code_hash=?`,
		sha(strings.ToUpper(strings.TrimSpace(req.Code)))).Scan(&jobID, &snapID, &agentID, &expires)
	if err != nil || expires < now() {
		httpErr(w, 403, "Code ungültig oder abgelaufen")
		return
	}
	snap, err := s.loadSnapshot(snapID)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	tok := randToken(32)
	s.db.Exec(`INSERT INTO restore_tokens(hash,job_id,snapshot_id,agent_id,expires) VALUES(?,?,?,?,?)`, sha(tok), jobID, snapID, agentID, now()+48*3600)
	writeJSON(w, proto.RestoreInfo{JobID: jobID, Token: tok, Snapshot: snap})
}

// newRestoreToken creates the token (and optionally a code) for an image restore job.
func (s *Server) newRestoreToken(jobID int64, snapID, agentID string, withCode bool) (tok, code string) {
	tok = randToken(32)
	codeHash := ""
	if withCode {
		code = randCode(8)
		codeHash = sha(code)
	}
	s.db.Exec(`INSERT INTO restore_tokens(hash,code_hash,job_id,snapshot_id,agent_id,expires) VALUES(?,?,?,?,?,?)`,
		sha(tok), codeHash, jobID, snapID, agentID, now()+48*3600)
	return tok, code
}
