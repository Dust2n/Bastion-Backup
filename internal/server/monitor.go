package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"bastion/internal/proto"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Monitoring endpoint for external tools (Checkmk, Nagios, scripts):
//
//	GET /api/monitor     Authorization: Bearer monitor:<token>
//
// The token is the setting monitor_token; without it the endpoint does not
// exist. It only allows reading this summary, nothing else.

type monitorAgent struct {
	ID         string `json:"id"`
	Hostname   string `json:"hostname"`
	Online     bool   `json:"online"`
	LastSeen   int64  `json:"last_seen"`
	LastBackup int64  `json:"last_backup"` // newest restorable backup, 0 = none
	BackupAge  int64  `json:"backup_age"`  // seconds, -1 = no backup
	LastStatus string `json:"last_status"` // result of the last backup job
	Stale      bool   `json:"stale"`       // older than notify_stale_days
}

type monitorStorage struct {
	Name    string `json:"name"`
	Online  bool   `json:"online"`
	Error   string `json:"error,omitempty"`
	Free    int64  `json:"free"`
	Total   int64  `json:"total"`
	Used    int64  `json:"used"`
	FreePct int    `json:"free_percent"`
}

type monitorStatus struct {
	Version      string           `json:"version"`
	Time         int64            `json:"time"`
	OK           bool             `json:"ok"`
	Problems     []string         `json:"problems"`
	Agents       []monitorAgent   `json:"agents"`
	Storages     []monitorStorage `json:"storages"`
	Failed24     int              `json:"failed_24h"`
	Running      int              `json:"running"`
	Damaged      int              `json:"damaged_backups"`
	LastCleanup  int64            `json:"last_cleanup"`
	ServerBackup int64            `json:"last_server_backup"` // 0 = never or not set up
}

func (s *Server) monitorStatus() monitorStatus {
	st := monitorStatus{Version: proto.Version, Time: now(), Problems: []string{}, Agents: []monitorAgent{}, Storages: []monitorStorage{}}
	stale := int64(s.settingInt("notify_stale_days", 3)) * 86400
	rows, err := s.db.Query(`SELECT a.id,a.hostname,a.last_seen,
		COALESCE((SELECT MAX(time) FROM snapshots sn WHERE sn.agent_id=a.id AND sn.deleted=0 AND sn.damaged=0),0),
		COALESCE((SELECT status FROM jobs j WHERE j.agent_id=a.id AND j.type='backup' AND j.status IN ('success','failed','canceled') ORDER BY j.id DESC LIMIT 1),'')
		FROM agents a WHERE a.removed=0 ORDER BY a.hostname`)
	if err != nil {
		st.Problems = append(st.Problems, "Datenbank: "+err.Error())
	} else {
		for rows.Next() {
			var a monitorAgent
			rows.Scan(&a.ID, &a.Hostname, &a.LastSeen, &a.LastBackup, &a.LastStatus)
			a.Online = st.Time-a.LastSeen < 120
			a.BackupAge = -1
			if a.LastBackup > 0 {
				a.BackupAge = st.Time - a.LastBackup
			}
			a.Stale = a.LastBackup == 0 || stale > 0 && a.BackupAge > stale
			switch {
			case a.LastBackup == 0:
				st.Problems = append(st.Problems, a.Hostname+": kein Backup vorhanden")
			case a.Stale:
				st.Problems = append(st.Problems, a.Hostname+": letztes Backup zu alt")
			case a.LastStatus == "failed":
				st.Problems = append(st.Problems, a.Hostname+": letztes Backup fehlgeschlagen")
			}
			st.Agents = append(st.Agents, a)
		}
		rows.Close()
	}
	for _, id := range s.storageIDs() {
		row, _, err := s.storageRow(id)
		if err != nil {
			continue
		}
		m := monitorStorage{Name: row.Name}
		if _, err := s.storeByID(id); err != nil {
			m.Error = err.Error()
			st.Problems = append(st.Problems, "Speicherort "+row.Name+" nicht erreichbar")
		} else {
			m.Online = true
			m.Free, m.Total = diskFree(row.Path)
			if m.Total > 0 {
				m.FreePct = int(m.Free * 100 / m.Total)
				if m.FreePct < 10 {
					st.Problems = append(st.Problems, "Speicherort "+row.Name+": weniger als 10 % frei")
				}
			}
		}
		var ss storageStats
		json.Unmarshal([]byte(s.setting("storage_stats:"+itoa(id), "{}")), &ss)
		m.Used = ss.Bytes
		st.Storages = append(st.Storages, m)
	}
	s.db.QueryRow(`SELECT COALESCE(SUM(status='failed'),0) FROM jobs WHERE type='backup' AND finished>?`, st.Time-86400).Scan(&st.Failed24)
	s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&st.Running)
	s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE deleted=0 AND damaged=1`).Scan(&st.Damaged)
	if st.Damaged > 0 {
		st.Problems = append(st.Problems, itoa(int64(st.Damaged))+" beschädigte Backups")
	}
	var gc repoStats
	json.Unmarshal([]byte(s.setting("repo_stats", "{}")), &gc)
	st.LastCleanup = gc.Time
	var dr *serverBackupStatus
	json.Unmarshal([]byte(s.setting("dr_status", "null")), &dr)
	switch {
	case s.setting("dr_passphrase", "") == "":
		st.Problems = append(st.Problems, "Server-Sicherung nicht eingerichtet")
	case dr == nil || !dr.OK:
		st.Problems = append(st.Problems, "letzte Server-Sicherung fehlgeschlagen oder nie gelaufen")
	default:
		st.ServerBackup = dr.Time
		if st.Time-dr.Time > 3*86400 {
			st.Problems = append(st.Problems, "Server-Sicherung älter als 3 Tage")
		}
	}
	st.OK = len(st.Problems) == 0
	return st
}

func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	want := s.setting("monitor_token", "")
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer monitor:")
	if want == "" {
		http.NotFound(w, r)
		return
	}
	if s.login.blocked("ip:" + clientIP(r)) {
		httpErr(w, 429, "zu viele Versuche")
		return
	}
	if len(want) < 16 || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		s.login.fail("ip:" + clientIP(r))
		httpErr(w, 401, "Monitoring-Token falsch")
		return
	}
	writeJSON(w, s.monitorStatus())
}
