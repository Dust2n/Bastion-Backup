package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"bastion/internal/layout"
	"bastion/internal/proto"
	"bastion/internal/repo"
)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/rescue/login", s.handleRescueLogin)
	mux.HandleFunc("GET /api/me", s.admin("viewer", func(w http.ResponseWriter, r *http.Request, u *user) {
		writeJSON(w, map[string]any{"name": u.Name, "role": u.Role, "version": proto.Version})
	}))
	mux.HandleFunc("GET /api/dashboard", s.admin("viewer", s.handleDashboard))
	mux.HandleFunc("GET /api/stats", s.admin("viewer", s.handleStats))
	mux.HandleFunc("GET /api/events", s.admin("viewer", s.handleEvents))
	mux.HandleFunc("GET /api/storages", s.admin("viewer", s.handleStorages))
	mux.HandleFunc("POST /api/storages", s.admin("admin", s.handleStorageCreate))
	mux.HandleFunc("PUT /api/storages/{id}", s.admin("admin", s.handleStorageUpdate))
	mux.HandleFunc("DELETE /api/storages/{id}", s.admin("admin", s.handleStorageDelete))
	mux.HandleFunc("GET /api/storages/browse", s.admin("admin", s.handleStorageBrowse))
	mux.HandleFunc("POST /api/plans/{id}/move", s.admin("admin", s.handlePlanMove))
	mux.HandleFunc("GET /api/snapshots/{id}/search", s.admin("operator", s.handleSearch))

	mux.HandleFunc("GET /api/agents", s.admin("viewer", s.handleAgents))
	mux.HandleFunc("GET /api/agents/{id}", s.admin("viewer", s.handleAgent))
	mux.HandleFunc("PATCH /api/agents/{id}", s.admin("operator", s.handleAgentPatch))
	mux.HandleFunc("DELETE /api/agents/{id}", s.admin("admin", s.handleAgentDelete))
	mux.HandleFunc("POST /api/agents/{id}/backup", s.admin("operator", s.handleAgentBackup))
	mux.HandleFunc("POST /api/agents/{id}/update", s.admin("admin", s.handleAgentUpdate))
	mux.HandleFunc("POST /api/agents/{id}/uninstall", s.admin("admin", s.handleAgentUninstall))

	mux.HandleFunc("GET /api/plans", s.admin("viewer", s.handlePlans))
	mux.HandleFunc("POST /api/plans", s.admin("admin", s.handlePlanSave))
	mux.HandleFunc("PUT /api/plans/{id}", s.admin("admin", s.handlePlanSave))
	mux.HandleFunc("DELETE /api/plans/{id}", s.admin("admin", s.handlePlanDelete))
	mux.HandleFunc("POST /api/plans/{id}/run", s.admin("operator", s.handlePlanRun))

	mux.HandleFunc("GET /api/jobs", s.admin("viewer", s.handleJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.admin("viewer", s.handleJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.admin("operator", s.handleJobCancel))

	mux.HandleFunc("GET /api/snapshots", s.admin("viewer", s.handleSnapshots))
	mux.HandleFunc("GET /api/snapshots/{id}", s.admin("viewer", s.handleSnapshot))
	mux.HandleFunc("DELETE /api/snapshots/{id}", s.admin("admin", s.handleSnapshotDelete))
	mux.HandleFunc("POST /api/snapshots/{id}/keep", s.admin("operator", s.handleSnapshotKeep))
	mux.HandleFunc("GET /api/snapshots/{id}/ls", s.admin("operator", s.handleLs))
	mux.HandleFunc("GET /api/snapshots/{id}/download", s.admin("operator", s.handleDownloadFile))
	mux.HandleFunc("POST /api/snapshots/{id}/restore", s.admin("operator", s.handleRestoreFiles))
	mux.HandleFunc("POST /api/snapshots/{id}/restore-image", s.admin("admin", s.handleRestoreImage))

	mux.HandleFunc("GET /api/tokens", s.admin("admin", s.handleTokens))
	mux.HandleFunc("POST /api/tokens", s.admin("admin", s.handleTokenCreate))
	mux.HandleFunc("DELETE /api/tokens/{hash}", s.admin("admin", s.handleTokenDelete))
	mux.HandleFunc("GET /api/install-info", s.admin("viewer", s.handleInstallInfo))
	mux.HandleFunc("POST /api/agent-binaries/rescan", s.admin("admin", func(w http.ResponseWriter, r *http.Request, u *user) {
		s.scanAgentBinaries()
		writeJSON(w, listBinaries())
	}))

	mux.HandleFunc("GET /api/credentials", s.admin("admin", s.handleCredentials))
	mux.HandleFunc("POST /api/credentials", s.admin("admin", s.handleCredentialCreate))
	mux.HandleFunc("DELETE /api/credentials/{id}", s.admin("admin", s.handleCredentialDelete))
	mux.HandleFunc("POST /api/discover/ldap", s.admin("admin", s.handleDiscoverLDAP))
	mux.HandleFunc("POST /api/discover/scan", s.admin("admin", s.handleDiscoverScan))
	mux.HandleFunc("GET /api/rollouts", s.admin("viewer", s.handleRollouts))
	mux.HandleFunc("POST /api/rollouts", s.admin("admin", s.handleRolloutCreate))
	mux.HandleFunc("POST /api/rollouts/{id}/retry", s.admin("admin", s.handleRolloutRetry))
	mux.HandleFunc("POST /api/rollouts/check", s.admin("admin", s.handleRolloutCheck))
	mux.HandleFunc("POST /api/snapshots/{id}/verify", s.admin("operator", s.handleSnapshotVerify))
	mux.HandleFunc("GET /api/server-backup",s.admin("admin", s.handleServerBackupStatus))
	mux.HandleFunc("PUT /api/server-backup", s.admin("admin", s.handleServerBackupSave))
	mux.HandleFunc("POST /api/server-backup/run", s.admin("admin", s.handleServerBackupRun))
	mux.HandleFunc("GET /api/server-backup/download", s.admin("admin", s.handleServerBackupDownload))
	mux.HandleFunc("DELETE /api/rollouts/{id}", s.admin("admin", func(w http.ResponseWriter, r *http.Request, u *user) {
		s.db.Exec(`DELETE FROM rollouts WHERE id=?`, r.PathValue("id"))
		writeJSON(w, map[string]string{"ok": "1"})
	}))

	mux.HandleFunc("GET /api/settings", s.admin("admin", s.handleSettings))
	mux.HandleFunc("PUT /api/settings", s.admin("admin", s.handleSettingsSave))
	mux.HandleFunc("POST /api/settings/test-mail", s.admin("admin", func(w http.ResponseWriter, r *http.Request, u *user) {
		if err := s.sendMail("[Bastion] Test", "Die E-Mail-Benachrichtigung funktioniert."); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
		writeJSON(w, map[string]string{"ok": "1"})
	}))
	mux.HandleFunc("GET /api/users", s.admin("admin", s.handleUsers))
	mux.HandleFunc("POST /api/users", s.admin("admin", s.handleUserCreate))
	mux.HandleFunc("DELETE /api/users/{id}", s.admin("admin", s.handleUserDelete))
	mux.HandleFunc("POST /api/password", s.admin("viewer", s.handlePassword))
	mux.HandleFunc("GET /api/audit", s.admin("admin", s.handleAudit))
	mux.HandleFunc("POST /api/maintenance/gc", s.admin("admin", func(w http.ResponseWriter, r *http.Request, u *user) {
		go s.runGC(u.Name)
		writeJSON(w, map[string]string{"ok": "Bereinigung gestartet"})
	}))
	mux.HandleFunc("POST /api/maintenance/scrub", s.admin("admin", func(w http.ResponseWriter, r *http.Request, u *user) {
		go s.runScrub(u.Name)
		writeJSON(w, map[string]string{"ok": "Integritätsprüfung gestartet"})
	}))
	mux.HandleFunc("GET /api/rescue-status", s.admin("viewer", s.handleRescueStatus))
	mux.HandleFunc("GET /api/rescue/file/{name}", s.admin("operator", func(w http.ResponseWriter, r *http.Request, u *user) {
		switch name := r.PathValue("name"); name {
		case "bastion-rescue-winpe.iso", "bastion-rescue-linux.iso":
			w.Header().Set("Content-Disposition", "attachment; filename="+name)
			http.ServeFile(w, r, s.path("rescue", name))
		default:
			httpErr(w, 404, "unbekannte Datei")
		}
	}))
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// ---------- dashboard ----------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, u *user) {
	d := map[string]any{}
	var agents, online, ok24, failed24, running, queued int
	var snaps int
	var logical int64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(last_seen>?),0) FROM agents WHERE removed=0`, now()-120).Scan(&agents, &online)
	s.db.QueryRow(`SELECT COALESCE(SUM(status='success'),0), COALESCE(SUM(status='failed'),0) FROM jobs WHERE type='backup' AND finished>?`, now()-86400).Scan(&ok24, &failed24)
	s.db.QueryRow(`SELECT COALESCE(SUM(status IN ('running','waiting')),0), COALESCE(SUM(status='queued'),0) FROM jobs`).Scan(&running, &queued)
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM snapshots WHERE deleted=0`).Scan(&snaps, &logical)
	var stats repoStats
	json.Unmarshal([]byte(s.setting("repo_stats", "{}")), &stats)
	d["agents"], d["online"], d["ok24"], d["failed24"], d["running"], d["queued"] = agents, online, ok24, failed24, running, queued
	d["snapshots"], d["logical_bytes"], d["repo"] = snaps, logical, stats
	var scrub map[string]any
	json.Unmarshal([]byte(s.setting("scrub_result", "null")), &scrub)
	d["scrub"] = scrub

	stale := s.settingInt("notify_stale_days", 3)
	rows, _ := s.db.Query(`SELECT a.id,a.hostname,a.last_seen,COALESCE(MAX(sn.time),0) FROM agents a
		LEFT JOIN snapshots sn ON sn.agent_id=a.id AND sn.deleted=0 WHERE a.removed=0 GROUP BY a.id HAVING COALESCE(MAX(sn.time),0) < ? ORDER BY a.hostname`, now()-int64(stale)*86400)
	var problems []map[string]any
	for rows.Next() {
		var id, host string
		var seen, last int64
		rows.Scan(&id, &host, &seen, &last)
		problems = append(problems, map[string]any{"id": id, "hostname": host, "last_seen": seen, "last_backup": last})
	}
	rows.Close()
	d["problems"] = problems
	d["stale_days"] = stale
	d["recent"] = s.queryJobs(`WHERE 1=1`, nil, 12)
	writeJSON(w, d)
}

// ---------- agents ----------

type agentRow struct {
	ID         string   `json:"id"`
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	OSVersion  string   `json:"os_version"`
	Arch       string   `json:"arch"`
	Version    string   `json:"version"`
	IPs        []string `json:"ips"`
	Group      string   `json:"group"`
	LastSeen   int64    `json:"last_seen"`
	Online     bool     `json:"online"`
	LastBackup int64    `json:"last_backup"`
	LastStatus string   `json:"last_status"`
	Running    string   `json:"running"`
	Plans      []string `json:"plans"`
	Snapshots  int      `json:"snapshots"`
	Update     bool     `json:"update_available"`
	Removed    int64    `json:"removed,omitempty"`
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request, u *user) {
	rows, err := s.db.Query(`SELECT a.id,a.hostname,a.os,a.os_version,a.arch,a.version,a.ips,a.grp,a.last_seen,
		COALESCE((SELECT MAX(time) FROM snapshots WHERE agent_id=a.id AND deleted=0),0),
		COALESCE((SELECT COUNT(*) FROM snapshots WHERE agent_id=a.id AND deleted=0),0),
		COALESCE((SELECT status FROM jobs WHERE agent_id=a.id AND type='backup' AND status NOT IN ('queued','running') ORDER BY id DESC LIMIT 1),''),
		COALESCE((SELECT type FROM jobs WHERE agent_id=a.id AND status IN ('running','waiting') LIMIT 1),''),a.removed
		FROM agents a ORDER BY a.removed>0, lower(a.hostname)`)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	defer rows.Close()
	planNames := s.planNamesByAgent()
	out := []agentRow{}
	for rows.Next() {
		var a agentRow
		var ips string
		rows.Scan(&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.Version, &ips, &a.Group, &a.LastSeen, &a.LastBackup, &a.Snapshots, &a.LastStatus, &a.Running, &a.Removed)
		json.Unmarshal([]byte(ips), &a.IPs)
		a.Online = now()-a.LastSeen < 120 && a.Removed == 0
		a.Plans = planNames[a.ID]
		if b, ok := agentBinaryFor(a.OS, a.Arch); ok && b.Version != "" && b.Version != a.Version {
			a.Update = true
		}
		out = append(out, a)
	}
	writeJSON(w, out)
}

func (s *Server) planNamesByAgent() map[string][]string {
	res := map[string][]string{}
	rows, err := s.db.Query(`SELECT id FROM plans`)
	if err != nil {
		return res
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		p, err := s.getPlan(id)
		if err != nil {
			continue
		}
		for _, a := range s.planAgents(p) {
			res[a] = append(res[a], p.Name)
		}
	}
	return res
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request, u *user) {
	id := r.PathValue("id")
	var a agentRow
	var ips, macs, lay string
	var enrolled int64
	err := s.db.QueryRow(`SELECT id,hostname,os,os_version,arch,version,ips,macs,grp,last_seen,layout,enrolled,removed FROM agents WHERE id=?`, id).
		Scan(&a.ID, &a.Hostname, &a.OS, &a.OSVersion, &a.Arch, &a.Version, &ips, &macs, &a.Group, &a.LastSeen, &lay, &enrolled, &a.Removed)
	if err != nil {
		httpErr(w, 404, "Agent nicht gefunden")
		return
	}
	json.Unmarshal([]byte(ips), &a.IPs)
	a.Online = now()-a.LastSeen < 120 && a.Removed == 0
	var m *layout.Machine
	json.Unmarshal([]byte(lay), &m)
	var macList []string
	json.Unmarshal([]byte(macs), &macList)
	writeJSON(w, map[string]any{
		"agent": a, "macs": macList, "layout": m, "enrolled": enrolled,
		"snapshots": s.querySnapshots(`WHERE agent_id=? AND deleted=0`, []any{id}),
		"jobs":      s.queryJobs(`WHERE j.agent_id=?`, []any{id}, 30),
		"plans":     s.planNamesByAgent()[id],
	})
}

func (s *Server) handleAgentPatch(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Group *string `json:"group"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if req.Group != nil {
		s.db.Exec(`UPDATE agents SET grp=? WHERE id=?`, strings.TrimSpace(*req.Group), r.PathValue("id"))
		s.audit(u.Name, "agent_group", r.PathValue("id")+" → "+*req.Group)
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request, u *user) {
	id := r.PathValue("id")
	s.db.Exec(`DELETE FROM agents WHERE id=?`, id)
	s.db.Exec(`DELETE FROM plan_targets WHERE target=?`, "agent:"+id)
	s.db.Exec(`UPDATE jobs SET status='canceled',finished=? WHERE agent_id=? AND status IN ('queued','running','waiting')`, now(), id)
	if r.URL.Query().Get("snapshots") == "1" {
		s.db.Exec(`UPDATE snapshots SET deleted=1 WHERE agent_id=?`, id)
	}
	s.audit(u.Name, "agent_delete", id)
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleAgentBackup(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		PlanID int64 `json:"plan_id"`
	}
	readJSON(r, &req)
	id := r.PathValue("id")
	params := proto.BackupParams{PlanID: req.PlanID, Kind: "image", UseSnapshot: true}
	jid, err := s.enqueue(id, req.PlanID, proto.JobBackup, params, u.Name)
	if err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	s.audit(u.Name, "backup_now", fmt.Sprintf("%s Plan %d", id, req.PlanID))
	writeJSON(w, map[string]any{"job_id": jid})
}

func (s *Server) updateJob(agentID, by string) (int64, error) {
	var osName, arch, ver string
	if err := s.db.QueryRow(`SELECT os,arch,version FROM agents WHERE id=?`, agentID).Scan(&osName, &arch, &ver); err != nil {
		return 0, fmt.Errorf("Agent nicht gefunden")
	}
	b, ok := agentBinaryFor(osName, arch)
	if !ok || b.Version == "" {
		return 0, fmt.Errorf("keine Agent-Version für %s-%s auf dem Server (data/agents/%s-%s/)", osName, arch, osName, arch)
	}
	if b.Version == ver {
		return 0, fmt.Errorf("Agent ist bereits auf Version %s", ver)
	}
	return s.enqueue(agentID, 0, proto.JobUpdate, proto.UpdateParams{
		Version: b.Version, Path: "/api/agent/download/" + b.Path, SHA256: b.SHA256, Signature: b.Signature}, by)
}

func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request, u *user) {
	ids := []string{r.PathValue("id")}
	if ids[0] == "all" {
		ids = nil
		grp := r.URL.Query().Get("group")
		rows, _ := s.db.Query(`SELECT id FROM agents WHERE removed=0 AND (?='' OR grp=?)`, grp, grp)
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
	}
	var queued int
	var errs []string
	for _, id := range ids {
		if _, err := s.updateJob(id, u.Name); err != nil {
			errs = append(errs, err.Error())
		} else {
			queued++
		}
	}
	s.audit(u.Name, "agent_update", fmt.Sprintf("%d Agents", queued))
	if queued == 0 && len(errs) > 0 {
		httpErr(w, 409, "%s", errs[0])
		return
	}
	writeJSON(w, map[string]any{"queued": queued})
}

func (s *Server) handleAgentUninstall(w http.ResponseWriter, r *http.Request, u *user) {
	id := r.PathValue("id")
	jid, err := s.enqueue(id, 0, proto.JobUninstall, struct{}{}, u.Name)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.audit(u.Name, "agent_uninstall", id)
	writeJSON(w, map[string]any{"job_id": jid})
}

// ---------- plans ----------

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request, u *user) {
	rows, err := s.db.Query(`SELECT id FROM plans ORDER BY name`)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []map[string]any{}
	for _, id := range ids {
		p, err := s.getPlan(id)
		if err != nil {
			continue
		}
		next := ""
		if sch, err := cronParser.Parse(p.Schedule); err == nil && p.Enabled {
			next = sch.Next(time.Now()).Format("02.01.2006 15:04")
		}
		out = append(out, map[string]any{"plan": p, "agents": len(s.planAgents(p)), "next": next})
	}
	writeJSON(w, out)
}

func (s *Server) handlePlanSave(w http.ResponseWriter, r *http.Request, u *user) {
	var p plan
	if err := readJSON(r, &p); err != nil {
		httpErr(w, 400, "ungültige Anfrage: %v", err)
		return
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		httpErr(w, 400, "Name fehlt")
		return
	}
	if p.Kind != "image" && p.Kind != "files" {
		httpErr(w, 400, "Art muss image oder files sein")
		return
	}
	if p.Kind == "files" && len(p.Paths) == 0 {
		httpErr(w, 400, "Für Datei-Backups mindestens einen Pfad angeben")
		return
	}
	if p.Schedule != "" {
		if _, err := cronParser.Parse(p.Schedule); err != nil {
			httpErr(w, 400, "Zeitplan ungültig: %v", err)
			return
		}
	}
	clean := func(in []string) []string {
		out := []string{}
		for _, x := range in {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	p.Paths, p.Excludes = clean(p.Paths), clean(p.Excludes)
	if p.StorageID != 0 {
		if _, err := s.storeByID(p.StorageID); err != nil {
			httpErr(w, 400, "Speicherort: %v", err)
			return
		}
	}
	id := pathID(r)
	oldStorage := int64(0)
	if id == 0 {
		res, err := s.db.Exec(`INSERT INTO plans(name,kind,schedule,paths,excludes,keep_last,keep_daily,keep_weekly,keep_monthly,bandwidth_kbps,use_snapshot,enabled,storage_id,created)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.Name, p.Kind, p.Schedule, jsonStr(p.Paths), jsonStr(p.Excludes), p.KeepLast, p.KeepDaily,
			p.KeepWeekly, p.KeepMonthly, p.BandwidthKBps, p.UseSnapshot, p.Enabled, p.StorageID, now())
		if err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
		id, _ = res.LastInsertId()
	} else {
		oldStorage = s.planStorage(id)
		s.db.Exec(`UPDATE plans SET name=?,kind=?,schedule=?,paths=?,excludes=?,keep_last=?,keep_daily=?,keep_weekly=?,keep_monthly=?,bandwidth_kbps=?,use_snapshot=?,enabled=?,storage_id=? WHERE id=?`,
			p.Name, p.Kind, p.Schedule, jsonStr(p.Paths), jsonStr(p.Excludes), p.KeepLast, p.KeepDaily, p.KeepWeekly, p.KeepMonthly,
			p.BandwidthKBps, p.UseSnapshot, p.Enabled, p.StorageID, id)
	}
	s.db.Exec(`DELETE FROM plan_targets WHERE plan_id=?`, id)
	for _, t := range p.Targets {
		s.db.Exec(`INSERT OR IGNORE INTO plan_targets(plan_id,target) VALUES(?,?)`, id, t)
	}
	s.audit(u.Name, "plan_save", fmt.Sprintf("%d %s", id, p.Name))
	resp := map[string]any{"id": id}
	if newStorage := s.planStorage(id); oldStorage != 0 && newStorage != oldStorage && r.URL.Query().Get("move") == "1" {
		job, err := s.startMove(id, newStorage, u.Name)
		if err != nil {
			resp["move_error"] = err.Error()
		} else {
			resp["move_job"] = job
		}
	}
	writeJSON(w, resp)
}

func (s *Server) handlePlanMove(w http.ResponseWriter, r *http.Request, u *user) {
	id := pathID(r)
	if _, err := s.getPlan(id); err != nil {
		httpErr(w, 404, "Plan nicht gefunden")
		return
	}
	job, err := s.startMove(id, s.planStorage(id), u.Name)
	if err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	s.audit(u.Name, "plan_move", fmt.Sprint(id))
	writeJSON(w, map[string]any{"job_id": job})
}

// handleStats feeds the dashboard charts.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, u *user) {
	type day struct {
		Day     string `json:"day"`
		OK      int    `json:"ok"`
		Failed  int    `json:"failed"`
		NewByte int64  `json:"new_bytes"`
	}
	days := make([]day, 14)
	idx := map[string]int{}
	start := time.Now().AddDate(0, 0, -13)
	for i := range days {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		days[i].Day = d
		idx[d] = i
	}
	from := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local).Unix()
	rows, err := s.db.Query(`SELECT finished,status FROM jobs WHERE type='backup' AND finished>=? AND status IN ('success','failed')`, from)
	if err == nil {
		for rows.Next() {
			var t int64
			var st string
			rows.Scan(&t, &st)
			if i, ok := idx[time.Unix(t, 0).Format("2006-01-02")]; ok {
				if st == "success" {
					days[i].OK++
				} else {
					days[i].Failed++
				}
			}
		}
		rows.Close()
	}
	if rows, err := s.db.Query(`SELECT time,new_bytes FROM snapshots WHERE time>=?`, from); err == nil {
		for rows.Next() {
			var t, b int64
			rows.Scan(&t, &b)
			if i, ok := idx[time.Unix(t, 0).Format("2006-01-02")]; ok {
				days[i].NewByte += b
			}
		}
		rows.Close()
	}
	writeJSON(w, map[string]any{"days": days})
}

func (s *Server) handlePlanDelete(w http.ResponseWriter, r *http.Request, u *user) {
	id := pathID(r)
	s.db.Exec(`DELETE FROM plans WHERE id=?`, id)
	s.db.Exec(`DELETE FROM plan_targets WHERE plan_id=?`, id)
	s.db.Exec(`UPDATE jobs SET status='canceled',finished=? WHERE plan_id=? AND status='queued'`, now(), id)
	s.audit(u.Name, "plan_delete", fmt.Sprint(id))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handlePlanRun(w http.ResponseWriter, r *http.Request, u *user) {
	p, err := s.getPlan(pathID(r))
	if err != nil {
		httpErr(w, 404, "Plan nicht gefunden")
		return
	}
	n := 0
	for _, a := range s.planAgents(p) {
		if _, err := s.enqueue(a, p.ID, proto.JobBackup, proto.BackupParams{PlanID: p.ID}, u.Name); err == nil {
			n++
		}
	}
	s.audit(u.Name, "plan_run", p.Name)
	writeJSON(w, map[string]any{"queued": n})
}

// ---------- jobs ----------

type jobRow struct {
	ID         int64   `json:"id"`
	AgentID    string  `json:"agent_id"`
	Hostname   string  `json:"hostname"`
	PlanID     int64   `json:"plan_id"`
	PlanName   string  `json:"plan_name"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
	Percent    float64 `json:"percent"`
	Message    string  `json:"message"`
	Files      int64   `json:"files"`
	Bytes      int64   `json:"bytes"`
	Total      int64   `json:"total"`
	SnapshotID string  `json:"snapshot_id"`
	Created    int64   `json:"created"`
	Started    int64   `json:"started"`
	Finished   int64   `json:"finished"`
	CreatedBy  string  `json:"created_by"`
	Speed      float64 `json:"speed,omitempty"` // bytes per second
	ETA        int64   `json:"eta,omitempty"`   // seconds left
	Log        string  `json:"log,omitempty"`
	Params     any     `json:"params,omitempty"`
}

func (s *Server) queryJobs(where string, args []any, limit int) []jobRow {
	rows, err := s.db.Query(`SELECT j.id,j.agent_id,COALESCE(a.hostname,CASE WHEN j.agent_id='' THEN 'Server' ELSE '?' END),j.plan_id,COALESCE(p.name,''),j.type,j.status,j.percent,j.message,
		j.files,j.bytes,j.total,j.snapshot_id,j.created,j.started,j.finished,j.created_by
		FROM jobs j LEFT JOIN agents a ON a.id=j.agent_id LEFT JOIN plans p ON p.id=j.plan_id `+where+
		fmt.Sprintf(` ORDER BY j.id DESC LIMIT %d`, limit), args...)
	out := []jobRow{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var j jobRow
		rows.Scan(&j.ID, &j.AgentID, &j.Hostname, &j.PlanID, &j.PlanName, &j.Type, &j.Status, &j.Percent, &j.Message,
			&j.Files, &j.Bytes, &j.Total, &j.SnapshotID, &j.Created, &j.Started, &j.Finished, &j.CreatedBy)
		if j.Status == "running" {
			j.Speed, j.ETA = jobSpeed(j.ID, j.Bytes, j.Total)
		}
		out = append(out, j)
	}
	return out
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, u *user) {
	q := r.URL.Query()
	where, args := "WHERE 1=1", []any{}
	if a := q.Get("agent"); a != "" {
		where += " AND j.agent_id=?"
		args = append(args, a)
	}
	if st := q.Get("status"); st == "active" {
		where += " AND j.status IN ('queued','running','waiting')"
	} else if st != "" {
		where += " AND j.status=?"
		args = append(args, st)
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	writeJSON(w, s.queryJobs(where, args, limit))
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request, u *user) {
	jobs := s.queryJobs("WHERE j.id=?", []any{pathID(r)}, 1)
	if len(jobs) == 0 {
		httpErr(w, 404, "Auftrag nicht gefunden")
		return
	}
	j := jobs[0]
	var params string
	s.db.QueryRow(`SELECT log,params FROM jobs WHERE id=?`, j.ID).Scan(&j.Log, &params)
	var pm map[string]any
	json.Unmarshal([]byte(params), &pm)
	delete(pm, "token") // never show restore tokens
	j.Params = pm
	writeJSON(w, j)
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request, u *user) {
	id := pathID(r)
	s.db.Exec(`UPDATE jobs SET status='canceled',finished=?,message='vom Administrator abgebrochen' WHERE id=? AND status IN ('queued','waiting')`, now(), id)
	s.db.Exec(`UPDATE jobs SET cancel=1 WHERE id=? AND status='running'`, id)
	s.db.Exec(`DELETE FROM restore_tokens WHERE job_id=? AND (SELECT status FROM jobs WHERE id=?)='canceled'`, id, id)
	s.publishJob(id)
	s.audit(u.Name, "job_cancel", fmt.Sprint(id))
	writeJSON(w, map[string]string{"ok": "1"})
}

// ---------- snapshots ----------

type snapRow struct {
	ID       string `json:"id"`
	AgentID  string `json:"agent_id"`
	Hostname string `json:"hostname"`
	PlanID   int64  `json:"plan_id"`
	PlanName string `json:"plan_name"`
	Time     int64  `json:"time"`
	Kind     string `json:"kind"`
	Files    int64  `json:"files"`
	Bytes    int64  `json:"bytes"`
	NewBytes int64  `json:"new_bytes"`
	Keep     bool   `json:"keep"`
	Verified int64  `json:"verified,omitempty"`
	VerifyOK bool   `json:"verify_ok"`
	VerifyMs string `json:"verify_msg,omitempty"`
}

func (s *Server) querySnapshots(where string, args []any) []snapRow {
	rows, err := s.db.Query(`SELECT s.id,s.agent_id,COALESCE(a.hostname,'?'),s.plan_id,COALESCE(p.name,''),s.time,s.kind,s.files,s.bytes,s.new_bytes,s.keep,s.verified,s.verify_ok,s.verify_msg
		FROM snapshots s LEFT JOIN agents a ON a.id=s.agent_id LEFT JOIN plans p ON p.id=s.plan_id `+
		strings.Replace(strings.Replace(where, "agent_id", "s.agent_id", 1), "deleted", "s.deleted", 1)+` ORDER BY s.time DESC LIMIT 500`, args...)
	out := []snapRow{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var x snapRow
		rows.Scan(&x.ID, &x.AgentID, &x.Hostname, &x.PlanID, &x.PlanName, &x.Time, &x.Kind, &x.Files, &x.Bytes, &x.NewBytes, &x.Keep, &x.Verified, &x.VerifyOK, &x.VerifyMs)
		out = append(out, x)
	}
	return out
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request, u *user) {
	if a := r.URL.Query().Get("agent"); a != "" {
		writeJSON(w, s.querySnapshots(`WHERE agent_id=? AND deleted=0`, []any{a}))
		return
	}
	writeJSON(w, s.querySnapshots(`WHERE deleted=0`, nil))
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, u *user) {
	snap, err := s.loadSnapshot(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	vols := make([]repo.Volume, len(snap.Volumes))
	sizes := map[string]int64{}
	for i, v := range snap.Volumes {
		sizes[v.Key] = v.Bytes
		v.Tree = nil // not needed by the UI
		vols[i] = v
	}
	snap.Volumes = nil
	var disk *layout.Disk
	var required int64
	if snap.Layout != nil {
		for _, d := range snap.Layout.Disks {
			if d.System {
				dd := d
				disk = &dd
				required = layout.Required(layout.WithBackupSizes(d, sizes))
			}
		}
	}
	var verified int64
	var verifyOK bool
	var verifyMsg string
	s.db.QueryRow(`SELECT verified,verify_ok,verify_msg FROM snapshots WHERE id=?`, snap.ID).Scan(&verified, &verifyOK, &verifyMsg)
	writeJSON(w, map[string]any{"snapshot": snap, "volumes": vols, "system_disk": disk, "required_disk": required,
		"verified": verified, "verify_ok": verifyOK, "verify_msg": verifyMsg})
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request, u *user) {
	s.db.Exec(`UPDATE snapshots SET deleted=1 WHERE id=?`, r.PathValue("id"))
	s.audit(u.Name, "snapshot_delete", r.PathValue("id"))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleSnapshotKeep(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct{ Keep bool }
	readJSON(r, &req)
	s.db.Exec(`UPDATE snapshots SET keep=? WHERE id=?`, req.Keep, r.PathValue("id"))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleRestoreFiles(w http.ResponseWriter, r *http.Request, u *user) {
	snap, err := s.loadSnapshot(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	var req struct {
		AgentID   string   `json:"agent_id"`
		Volume    string   `json:"volume"`
		Paths     []string `json:"paths"`
		Target    string   `json:"target"`
		Overwrite string   `json:"overwrite"`
		Format    bool     `json:"format"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if req.AgentID == "" {
		req.AgentID = snap.AgentID
	}
	if req.AgentID != snap.AgentID && req.Target == "" {
		httpErr(w, 400, "Bei Wiederherstellung auf einen anderen Rechner muss ein Zielordner angegeben werden")
		return
	}
	if len(req.Paths) == 0 && req.Target == "" && !req.Format {
		var lay *layout.Machine = snap.Layout
		if lay != nil && snap.Kind == "image" {
			for _, d := range lay.Disks {
				for _, p := range d.Partitions {
					if p.Volume == req.Volume && p.Role == layout.RoleSystem {
						httpErr(w, 400, "Das System-Volume bitte über die Image-Wiederherstellung zurücksetzen")
						return
					}
				}
			}
		}
	}
	jid, err := s.enqueue(req.AgentID, 0, proto.JobRestoreFiles, proto.RestoreFilesParams{
		SnapshotID: snap.ID, Volume: req.Volume, Paths: req.Paths, Target: req.Target, Overwrite: req.Overwrite, Format: req.Format}, u.Name)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.audit(u.Name, "restore_files", fmt.Sprintf("%s %s %d Pfade → %s %s", snap.ID, req.Volume, len(req.Paths), req.AgentID, req.Target))
	writeJSON(w, map[string]any{"job_id": jid})
}

// handleRestoreImage starts a full image restore: "live" stages the rescue
// system on the running machine and reboots it; "media" waits for a USB/PXE
// rescue system that logs in with the shown code.
func (s *Server) handleRestoreImage(w http.ResponseWriter, r *http.Request, u *user) {
	snap, err := s.loadSnapshot(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	if snap.Kind != "image" || snap.Layout == nil {
		httpErr(w, 400, "Nur Image-Backups können als ganzes System wiederhergestellt werden")
		return
	}
	var req struct {
		AgentID  string `json:"agent_id"`
		Mode     string `json:"mode"`
		DelaySec int    `json:"delay_sec"`
		Message  string `json:"message"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if req.AgentID == "" {
		req.AgentID = snap.AgentID
	}
	params := proto.RestoreImageParams{SnapshotID: snap.ID, DiskNumber: -1, DelaySec: req.DelaySec, Message: req.Message}
	status := "queued"
	if req.Mode == "media" {
		status = "waiting"
	} else {
		var osName string
		s.db.QueryRow(`SELECT os FROM agents WHERE id=?`, req.AgentID).Scan(&osName)
		if osName != snap.OS {
			httpErr(w, 400, "Der Ziel-Agent läuft unter %s, das Backup stammt von %s", osName, snap.OS)
			return
		}
	}
	res, err := s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,created_by,message) VALUES(?,?,?,?,?,?,?,?)`,
		req.AgentID, 0, proto.JobRestoreImage, "{}", status, now(), u.Name, "")
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	jid, _ := res.LastInsertId()
	tok, code := s.newRestoreToken(jid, snap.ID, req.AgentID, true)
	params.Token = tok
	stored := params
	if req.Mode == "media" {
		stored.Token = ""
		s.db.Exec(`UPDATE jobs SET message=? WHERE id=?`, "Wartet auf Rettungsmedium (Code "+code+")", jid)
	}
	s.db.Exec(`UPDATE jobs SET params=? WHERE id=?`, jsonStr(stored), jid)
	s.notifyAgent(req.AgentID)
	s.audit(u.Name, "restore_image", fmt.Sprintf("%s → %s (%s)", snap.ID, req.AgentID, req.Mode))
	writeJSON(w, map[string]any{"job_id": jid, "code": code})
}

// ---------- tokens & install ----------

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request, u *user) {
	rows, _ := s.db.Query(`SELECT hash,label,grp,plan_id,created,expires,uses FROM tokens WHERE label NOT LIKE 'rollout:%' ORDER BY created DESC`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var hash, label, grp string
		var plan, created, expires, uses int64
		rows.Scan(&hash, &label, &grp, &plan, &created, &expires, &uses)
		out = append(out, map[string]any{"hash": hash, "label": label, "group": grp, "plan_id": plan, "created": created, "expires": expires, "uses": uses})
	}
	writeJSON(w, out)
}

func (s *Server) createToken(label, grp string, planID int64, days int, uses int) string {
	tok := randCode(6) + "-" + randCode(6) + "-" + randCode(6)
	var exp int64
	if days > 0 {
		exp = now() + int64(days)*86400
	}
	s.db.Exec(`INSERT INTO tokens(hash,label,grp,plan_id,created,expires,uses) VALUES(?,?,?,?,?,?,?)`, sha(tok), label, grp, planID, now(), exp, uses)
	return tok
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Label  string `json:"label"`
		Group  string `json:"group"`
		PlanID int64  `json:"plan_id"`
		Days   int    `json:"days"`
		Uses   int    `json:"uses"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if req.Uses == 0 {
		req.Uses = -1
	}
	tok := s.createToken(req.Label, req.Group, req.PlanID, req.Days, req.Uses)
	s.audit(u.Name, "token_create", req.Label)
	writeJSON(w, map[string]any{"token": tok, "commands": s.installCommands(tok)})
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request, u *user) {
	s.db.Exec(`DELETE FROM tokens WHERE hash=?`, r.PathValue("hash"))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) installCommands(tok string) map[string]string {
	url, fp := s.PublicURL(), s.fp
	return map[string]string{
		"windows": fmt.Sprintf(`bastion-agent.exe install --server %s --token %s --fingerprint %s`, url, tok, fp),
		"windows_download": fmt.Sprintf(`powershell -NoProfile -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::ServerCertificateValidationCallback={param($a,$c)([BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($c.GetRawCertData())).Replace('-','').ToLower() -eq '%[3]s')}; (New-Object Net.WebClient).DownloadFile('%[1]s/dl/agents/windows-amd64/bastion-agent.exe', \"$env:TEMP\bastion-agent.exe\"); & \"$env:TEMP\bastion-agent.exe\" install --server %[1]s --token %[2]s --fingerprint %[3]s"`,
			url, tok, fp),
		"linux": fmt.Sprintf(`curl -fsSk %[1]s/dl/agents/linux-amd64/bastion-agent -o /tmp/bastion-agent && chmod +x /tmp/bastion-agent && sudo /tmp/bastion-agent install --server %[1]s --token %[2]s --fingerprint %[3]s`,
			url, tok, fp),
		"gpo": fmt.Sprintf(`\\SERVER\Freigabe\bastion-agent.exe install --server %s --token %s --fingerprint %s`, url, tok, fp),
	}
}

func (s *Server) handleInstallInfo(w http.ResponseWriter, r *http.Request, u *user) {
	writeJSON(w, map[string]any{"server": s.PublicURL(), "fingerprint": s.fp, "binaries": listBinaries(), "version": proto.Version})
}

// ---------- settings & users ----------

var settingKeys = []string{"public_url", "max_parallel_backups", "bandwidth_kbps", "gc_time", "scrub_time",
	"smtp_host", "smtp_port", "smtp_user", "smtp_from", "notify_to", "notify_failure", "notify_stale_days",
	"pxe_enabled", "pxe_ip", "pxe_dhcp", "rollout_retry_minutes", "rollout_retry_hours", "verify_time", "verify_days"}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, u *user) {
	out := map[string]string{}
	for _, k := range settingKeys {
		out[k] = s.setting(k, "")
	}
	out["smtp_pass_set"] = ""
	if s.setting("smtp_pass", "") != "" {
		out["smtp_pass_set"] = "1"
	}
	out["effective_public_url"] = s.PublicURL()
	out["fingerprint"] = s.fp
	out["data_dir"] = s.opt.DataDir
	writeJSON(w, out)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request, u *user) {
	var req map[string]string
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	for _, k := range settingKeys {
		if v, ok := req[k]; ok {
			s.setSetting(k, strings.TrimSpace(v))
		}
	}
	if pw, ok := req["smtp_pass"]; ok && pw != "" {
		s.setSetting("smtp_pass", base64.StdEncoding.EncodeToString(s.seal(pw)))
	}
	s.audit(u.Name, "settings", "")
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, u *user) {
	rows, _ := s.db.Query(`SELECT id,name,role,created FROM users ORDER BY name`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var name, role string
		rows.Scan(&id, &name, &role, &created)
		out = append(out, map[string]any{"id": id, "name": name, "role": role, "created": created})
	}
	writeJSON(w, out)
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct{ Name, Password, Role string }
	if err := readJSON(r, &req); err != nil || req.Name == "" || len(req.Password) < 10 || roleRank[req.Role] == 0 {
		httpErr(w, 400, "Name, Rolle und ein Passwort mit mindestens 10 Zeichen angeben")
		return
	}
	if _, err := s.db.Exec(`INSERT INTO users(name,pw,role,created) VALUES(?,?,?,?)`, req.Name, hashPassword(req.Password), req.Role, now()); err != nil {
		httpErr(w, 409, "Benutzer existiert bereits")
		return
	}
	s.audit(u.Name, "user_create", req.Name+" ("+req.Role+")")
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request, u *user) {
	if pathID(r) == u.ID {
		httpErr(w, 400, "Sie können sich nicht selbst löschen")
		return
	}
	s.db.Exec(`DELETE FROM users WHERE id=?`, pathID(r))
	s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, pathID(r))
	s.audit(u.Name, "user_delete", r.PathValue("id"))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct{ Old, New string }
	if err := readJSON(r, &req); err != nil || len(req.New) < 10 {
		httpErr(w, 400, "Das neue Passwort braucht mindestens 10 Zeichen")
		return
	}
	var pw string
	s.db.QueryRow(`SELECT pw FROM users WHERE id=?`, u.ID).Scan(&pw)
	if !checkPassword(pw, req.Old) {
		httpErr(w, 403, "Altes Passwort falsch")
		return
	}
	s.db.Exec(`UPDATE users SET pw=? WHERE id=?`, hashPassword(req.New), u.ID)
	os.Remove(s.path("initial-admin-password.txt"))
	s.audit(u.Name, "password_change", "")
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, u *user) {
	rows, _ := s.db.Query(`SELECT time,user,action,detail FROM audit ORDER BY id DESC LIMIT 300`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var t int64
		var usr, action, detail string
		rows.Scan(&t, &usr, &action, &detail)
		out = append(out, map[string]any{"time": t, "user": usr, "action": action, "detail": detail})
	}
	writeJSON(w, out)
}

func (s *Server) handleRescueStatus(w http.ResponseWriter, r *http.Request, u *user) {
	exists := func(p ...string) bool {
		_, err := os.Stat(s.path(p...))
		return err == nil
	}
	writeJSON(w, map[string]any{
		"winpe":       exists("rescue", "winpe.wim"),
		"winpe_iso":   exists("rescue", "bastion-rescue-winpe.iso"),
		"linux":       exists("rescue", "linux-vmlinuz") && exists("rescue", "linux-initrd.img"),
		"ipxe_efi":    exists("pxe", "ipxe.efi"),
		"ipxe_bios":   exists("pxe", "undionly.kpxe"),
		"wimboot":     exists("pxe", "wimboot"),
		"pxe_enabled": s.setting("pxe_enabled", "") == "1" || s.opt.PXE,
		"data_dir":    s.opt.DataDir,
		"server":      s.PublicURL(),
		"fingerprint": s.fp,
	})
}
