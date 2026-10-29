package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"bastion/internal/proto"
)

type plan struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Schedule      string   `json:"schedule"`
	Paths         []string `json:"paths"`
	Excludes      []string `json:"excludes"`
	KeepLast      int      `json:"keep_last"`
	KeepDaily     int      `json:"keep_daily"`
	KeepWeekly    int      `json:"keep_weekly"`
	KeepMonthly   int      `json:"keep_monthly"`
	BandwidthKBps int      `json:"bandwidth_kbps"`
	UseSnapshot   bool     `json:"use_snapshot"`
	Enabled       bool     `json:"enabled"`
	StorageID     int64    `json:"storage_id"` // 0 = standard storage
	Targets       []string `json:"targets"`    // agent:<id> or group:<name>
}

const planCols = `id,name,kind,schedule,paths,excludes,keep_last,keep_daily,keep_weekly,keep_monthly,bandwidth_kbps,use_snapshot,enabled,storage_id`

func scanPlan(sc interface{ Scan(...any) error }) (*plan, error) {
	var p plan
	var paths, excl string
	err := sc.Scan(&p.ID, &p.Name, &p.Kind, &p.Schedule, &paths, &excl, &p.KeepLast, &p.KeepDaily, &p.KeepWeekly,
		&p.KeepMonthly, &p.BandwidthKBps, &p.UseSnapshot, &p.Enabled, &p.StorageID)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(paths), &p.Paths)
	json.Unmarshal([]byte(excl), &p.Excludes)
	return &p, nil
}

func (s *Server) getPlan(id int64) (*plan, error) {
	p, err := scanPlan(s.db.QueryRow(`SELECT `+planCols+` FROM plans WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	rows, _ := s.db.Query(`SELECT target FROM plan_targets WHERE plan_id=?`, id)
	defer rows.Close()
	for rows.Next() {
		var t string
		rows.Scan(&t)
		p.Targets = append(p.Targets, t)
	}
	return p, nil
}

// planAgents resolves the targets of a plan to agent IDs.
func (s *Server) planAgents(p *plan) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range p.Targets {
		var rows *sql.Rows
		switch {
		case strings.HasPrefix(t, "agent:"):
			rows, _ = s.db.Query(`SELECT id FROM agents WHERE id=? AND removed=0`, strings.TrimPrefix(t, "agent:"))
		case strings.HasPrefix(t, "group:"):
			rows, _ = s.db.Query(`SELECT id FROM agents WHERE grp=? AND removed=0`, strings.TrimPrefix(t, "group:"))
		case t == "all":
			rows, _ = s.db.Query(`SELECT id FROM agents WHERE removed=0`)
		default:
			continue
		}
		for rows.Next() {
			var id string
			rows.Scan(&id)
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		rows.Close()
	}
	return out
}

// enqueue creates a job. Backups are not queued twice for the same plan.
func (s *Server) enqueue(agentID string, planID int64, typ string, params any, by string) (int64, error) {
	if typ == proto.JobBackup {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE agent_id=? AND plan_id=? AND type='backup' AND status IN ('queued','running')`,
			agentID, planID).Scan(&n)
		if n > 0 {
			return 0, fmt.Errorf("für diesen Plan ist bereits ein Backup geplant oder aktiv")
		}
	}
	res, err := s.db.Exec(`INSERT INTO jobs(agent_id,plan_id,type,params,status,created,created_by) VALUES(?,?,?,?,?,?,?)`,
		agentID, planID, typ, jsonStr(params), "queued", now(), by)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	s.notifyAgent(agentID)
	s.publishJob(id)
	if typ == proto.JobBackup {
		s.wakeOnLAN(agentID)
	}
	return id, nil
}

func (s *Server) wakeOnLAN(agentID string) {
	var last int64
	var macs string
	if s.db.QueryRow(`SELECT last_seen,macs FROM agents WHERE id=?`, agentID).Scan(&last, &macs) != nil || now()-last < 120 {
		return
	}
	var list []string
	json.Unmarshal([]byte(macs), &list)
	for _, m := range list {
		hw, err := net.ParseMAC(m)
		if err != nil || len(hw) != 6 {
			continue
		}
		pkt := make([]byte, 0, 102)
		pkt = append(pkt, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		for i := 0; i < 16; i++ {
			pkt = append(pkt, hw...)
		}
		if c, err := net.Dial("udp4", "255.255.255.255:9"); err == nil {
			c.Write(pkt)
			c.Close()
		}
	}
}

// dispatch hands the next queued job to an agent.
func (s *Server) dispatch(agentID string) (*proto.Job, error) {
	rows, err := s.db.Query(`SELECT id,type,plan_id,params FROM jobs WHERE agent_id=? AND status='queued' ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id     int64
		typ    string
		planID int64
		params string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		rows.Scan(&c.id, &c.typ, &c.planID, &c.params)
		cands = append(cands, c)
	}
	rows.Close()
	for _, c := range cands {
		params := c.params
		storage := int64(0)
		if c.typ == proto.JobBackup {
			s.gcMu.Lock()
			gc := s.gcRunning
			s.gcMu.Unlock()
			s.storeMu.Lock()
			moving := s.moving[c.planID]
			s.storeMu.Unlock()
			var running int
			s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='backup' AND status='running'`).Scan(&running)
			if gc || moving || running >= s.settingInt("max_parallel_backups", 10) {
				continue
			}
			storage = s.planStorage(c.planID)
			if _, err := s.storeByID(storage); err != nil {
				s.db.Exec(`UPDATE jobs SET status='failed',message=?,finished=? WHERE id=?`, "Speicherort nicht verfügbar: "+err.Error(), now(), c.id)
				continue
			}
			bp, err := s.backupParams(c.planID, c.params, agentID)
			if err != nil {
				s.db.Exec(`UPDATE jobs SET status='failed',message=?,finished=? WHERE id=?`, err.Error(), now(), c.id)
				continue
			}
			params = jsonStr(bp)
		}
		if c.typ == proto.JobRestoreFiles {
			var rp proto.RestoreFilesParams
			json.Unmarshal([]byte(c.params), &rp)
			storage = s.snapStorage(rp.SnapshotID)
		}
		res, err := s.db.Exec(`UPDATE jobs SET status='running',started=?,params=? WHERE id=? AND status='queued'`, now(), params, c.id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if storage != 0 {
			s.setAgentStore(agentID, storage)
		}
		s.publishJob(c.id)
		return &proto.Job{ID: c.id, Type: c.typ, Params: json.RawMessage(params)}, nil
	}
	return nil, nil
}

func (s *Server) backupParams(planID int64, stored, agentID string) (*proto.BackupParams, error) {
	var bp proto.BackupParams
	json.Unmarshal([]byte(stored), &bp)
	if planID != 0 {
		p, err := s.getPlan(planID)
		if err != nil {
			return nil, fmt.Errorf("Plan %d existiert nicht mehr", planID)
		}
		bp = proto.BackupParams{PlanID: p.ID, Kind: p.Kind, Paths: p.Paths, Excludes: p.Excludes,
			BandwidthKBps: p.BandwidthKBps, UseSnapshot: p.UseSnapshot}
	}
	if bp.Kind == "" {
		bp.Kind = "image"
	}
	if bp.BandwidthKBps == 0 {
		bp.BandwidthKBps = s.settingInt("bandwidth_kbps", 0)
	}
	// the previous backup only helps if its chunks are in the same storage
	s.db.QueryRow(`SELECT id FROM snapshots WHERE agent_id=? AND plan_id=? AND kind=? AND deleted=0 AND storage_id=? ORDER BY time DESC LIMIT 1`,
		agentID, bp.PlanID, bp.Kind, s.planStorage(planID)).Scan(&bp.Parent)
	return &bp, nil
}

func (s *Server) appendLog(jobID int64, lines []string) {
	if len(lines) == 0 {
		return
	}
	s.db.Exec(`UPDATE jobs SET log=substr(log || ?, -200000) WHERE id=?`, strings.Join(lines, "\n")+"\n", jobID)
}
