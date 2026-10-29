package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS agents(
  id TEXT PRIMARY KEY, hostname TEXT, os TEXT, os_version TEXT DEFAULT '', arch TEXT, version TEXT,
  ips TEXT DEFAULT '[]', macs TEXT DEFAULT '[]', secret_hash TEXT, grp TEXT DEFAULT '', layout TEXT DEFAULT '',
  enrolled INTEGER, last_seen INTEGER DEFAULT 0);
CREATE TABLE IF NOT EXISTS tokens(
  hash TEXT PRIMARY KEY, label TEXT, grp TEXT DEFAULT '', plan_id INTEGER DEFAULT 0,
  created INTEGER, expires INTEGER, uses INTEGER);
CREATE TABLE IF NOT EXISTS plans(
  id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, kind TEXT, schedule TEXT, paths TEXT DEFAULT '[]',
  excludes TEXT DEFAULT '[]', keep_last INTEGER, keep_daily INTEGER, keep_weekly INTEGER, keep_monthly INTEGER,
  bandwidth_kbps INTEGER DEFAULT 0, use_snapshot INTEGER DEFAULT 1, enabled INTEGER DEFAULT 1, created INTEGER);
CREATE TABLE IF NOT EXISTS plan_targets(plan_id INTEGER, target TEXT, PRIMARY KEY(plan_id, target));
CREATE TABLE IF NOT EXISTS jobs(
  id INTEGER PRIMARY KEY AUTOINCREMENT, agent_id TEXT, plan_id INTEGER DEFAULT 0, type TEXT, params TEXT DEFAULT '{}',
  status TEXT, percent REAL DEFAULT 0, message TEXT DEFAULT '', log TEXT DEFAULT '', files INTEGER DEFAULT 0,
  bytes INTEGER DEFAULT 0, total INTEGER DEFAULT 0, snapshot_id TEXT DEFAULT '', created INTEGER,
  started INTEGER DEFAULT 0, finished INTEGER DEFAULT 0, cancel INTEGER DEFAULT 0, created_by TEXT DEFAULT '');
CREATE INDEX IF NOT EXISTS jobs_agent ON jobs(agent_id, status);
CREATE TABLE IF NOT EXISTS snapshots(
  id TEXT PRIMARY KEY, agent_id TEXT, plan_id INTEGER, time INTEGER, kind TEXT, files INTEGER, bytes INTEGER,
  new_bytes INTEGER, data TEXT, deleted INTEGER DEFAULT 0, keep INTEGER DEFAULT 0);
CREATE INDEX IF NOT EXISTS snapshots_agent ON snapshots(agent_id, time);
CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE, pw TEXT, role TEXT, created INTEGER);
CREATE TABLE IF NOT EXISTS sessions(hash TEXT PRIMARY KEY, user_id INTEGER, expires INTEGER);
CREATE TABLE IF NOT EXISTS audit(id INTEGER PRIMARY KEY AUTOINCREMENT, time INTEGER, user TEXT, action TEXT, detail TEXT);
CREATE TABLE IF NOT EXISTS credentials(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, kind TEXT, username TEXT, secret BLOB, created INTEGER);
CREATE TABLE IF NOT EXISTS rollouts(
  id INTEGER PRIMARY KEY AUTOINCREMENT, target TEXT, method TEXT, cred_id INTEGER, plan_id INTEGER DEFAULT 0,
  grp TEXT DEFAULT '', status TEXT, message TEXT DEFAULT '', agent_id TEXT DEFAULT '', created INTEGER, updated INTEGER,
  token_hash TEXT DEFAULT '');
CREATE TABLE IF NOT EXISTS restore_tokens(
  hash TEXT PRIMARY KEY, code_hash TEXT DEFAULT '', job_id INTEGER, snapshot_id TEXT, agent_id TEXT, expires INTEGER);
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS storages(
  id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, kind TEXT, path TEXT, username TEXT DEFAULT '',
  secret BLOB, is_default INTEGER DEFAULT 0, created INTEGER);
`

// migrations add columns to databases created by older versions.
var migrations = []struct{ table, column, def string }{
	{"plans", "storage_id", "INTEGER DEFAULT 0"},
	{"snapshots", "storage_id", "INTEGER DEFAULT 0"},
	{"rollouts", "attempts", "INTEGER DEFAULT 0"},
	{"rollouts", "next_try", "INTEGER DEFAULT 0"},
	{"agents", "removed", "INTEGER DEFAULT 0"},
	{"snapshots", "verified", "INTEGER DEFAULT 0"},
	{"snapshots", "verify_ok", "INTEGER DEFAULT 0"},
	{"snapshots", "verify_msg", "TEXT DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, m.table)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var n string
			rows.Scan(&n)
			if n == m.column {
				found = true
			}
		}
		rows.Close()
		if !found {
			if _, err := db.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.column + ` ` + m.def); err != nil {
				return fmt.Errorf("%s.%s: %w", m.table, m.column, err)
			}
		}
	}
	return nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("Migration: %w", err)
	}
	// 0.2.0 and older could mark a successful rollout as failed
	db.Exec(`UPDATE rollouts SET status='done',message='Agent installiert und angemeldet' WHERE agent_id<>'' AND status='failed'`)
	// rollouts interrupted by a server restart are retried
	db.Exec(`UPDATE rollouts SET status='waiting',message='Server wurde neu gestartet, neuer Versuch',next_try=0 WHERE status IN ('queued','running')`)
	// server-side jobs do not survive a restart
	db.Exec(`UPDATE jobs SET status='failed',message='Server wurde neu gestartet',finished=? WHERE type='verify' AND status='running'`, time.Now().Unix())
	return db, nil
}

func now() int64 { return time.Now().Unix() }

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Server) setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Server) settingInt(key string, def int) int {
	v, err := strconv.Atoi(s.setting(key, ""))
	if err != nil {
		return def
	}
	return v
}

func (s *Server) setSetting(key, value string) {
	s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

func (s *Server) audit(user, action, detail string) {
	s.db.Exec(`INSERT INTO audit(time,user,action,detail) VALUES(?,?,?,?)`, now(), user, action, detail)
	s.log.Printf("[audit] %s: %s %s", user, action, detail)
}
