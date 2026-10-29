package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"bastion/internal/repo"
)

// A storage is a place where backups are kept: a local folder or an SMB
// share. Every plan writes to one storage; deduplication works within a
// storage. All storages use the same repository key.

type storageRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // local | smb
	Path      string `json:"path"`
	Username  string `json:"username,omitempty"`
	Default   bool   `json:"is_default"`
	Created   int64  `json:"created"`
	Free      int64  `json:"free"`
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
	Chunks    int64  `json:"chunks"`
	Snapshots int    `json:"snapshots"`
	Logical   int64  `json:"logical"`
	Plans     int    `json:"plans"`
	Online    bool   `json:"online"`
	Error     string `json:"error,omitempty"`
}

type storageStats struct {
	Chunks int64 `json:"chunks"`
	Bytes  int64 `json:"bytes"`
	Freed  int64 `json:"freed"`
	Time   int64 `json:"time"`
}

func (s *Server) initStorages() error {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM storages`).Scan(&n)
	if n == 0 {
		path := s.setting("repo_dir", s.path("repo"))
		if _, err := s.db.Exec(`INSERT INTO storages(name,kind,path,is_default,created) VALUES('Standard','local',?,1,?)`, path, now()); err != nil {
			return err
		}
	}
	def := s.defaultStorage()
	if def == 0 {
		return errors.New("kein Standard-Speicherort")
	}
	// backups from 0.1 have no storage yet: they live in the standard one
	s.db.Exec(`UPDATE snapshots SET storage_id=? WHERE storage_id=0`, def)
	_, err := s.storeByID(def)
	return err
}

func (s *Server) defaultStorage() int64 {
	var id int64
	s.db.QueryRow(`SELECT id FROM storages WHERE is_default=1 ORDER BY id LIMIT 1`).Scan(&id)
	return id
}

func (s *Server) storageRow(id int64) (*storageRow, string, error) {
	var r storageRow
	var secret []byte
	err := s.db.QueryRow(`SELECT id,name,kind,path,username,secret,is_default,created FROM storages WHERE id=?`, id).
		Scan(&r.ID, &r.Name, &r.Kind, &r.Path, &r.Username, &secret, &r.Default, &r.Created)
	if err != nil {
		return nil, "", fmt.Errorf("Speicherort %d existiert nicht", id)
	}
	pass := ""
	if len(secret) > 0 {
		pass, _ = s.unseal(secret)
	}
	return &r, pass, nil
}

// storeByID opens (once) the repository of a storage; 0 = standard.
func (s *Server) storeByID(id int64) (*repo.Store, error) {
	if id == 0 {
		id = s.defaultStorage()
	}
	s.storeMu.Lock()
	st, ok := s.stores[id]
	s.storeMu.Unlock()
	if ok {
		return st, nil
	}
	row, pass, err := s.storageRow(id)
	if err != nil {
		return nil, err
	}
	if row.Kind == "smb" {
		if err := connectShare(row.Path, row.Username, pass); err != nil {
			return nil, fmt.Errorf("%s: %w", row.Name, err)
		}
	}
	st, err = repo.Open(row.Path, s.repoKey)
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", row.Name, row.Path, err)
	}
	s.storeMu.Lock()
	s.stores[id] = st
	s.storeMu.Unlock()
	return st, nil
}

func (s *Server) planStorage(planID int64) int64 {
	var id int64
	if planID != 0 {
		s.db.QueryRow(`SELECT storage_id FROM plans WHERE id=?`, planID).Scan(&id)
	}
	if id == 0 {
		id = s.defaultStorage()
	}
	return id
}

func (s *Server) snapStorage(snapID string) int64 {
	var id int64
	s.db.QueryRow(`SELECT storage_id FROM snapshots WHERE id=?`, snapID).Scan(&id)
	if id == 0 {
		id = s.defaultStorage()
	}
	return id
}

func (s *Server) snapStore(snapID string) (*repo.Store, error) {
	return s.storeByID(s.snapStorage(snapID))
}

func (s *Server) setAgentStore(agentID string, storage int64) {
	s.storeMu.Lock()
	s.agentStore[agentID] = storage
	s.storeMu.Unlock()
}

// storeFor returns the repository an agent or rescue environment works
// with: the storage of its running job, the snapshot of a restore token,
// or the standard storage.
func (s *Server) storeFor(p *principal) (*repo.Store, int64, error) {
	var id int64
	if p.restore() {
		id = s.snapStorage(p.RestoreSnap)
	} else {
		s.storeMu.Lock()
		id = s.agentStore[p.AgentID]
		s.storeMu.Unlock()
	}
	if id == 0 {
		id = s.defaultStorage()
	}
	st, err := s.storeByID(id)
	return st, id, err
}

func (s *Server) storageIDs() []int64 {
	rows, err := s.db.Query(`SELECT id FROM storages ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

// ---------- API ----------

func (s *Server) handleStorages(w http.ResponseWriter, r *http.Request, u *user) {
	out := []*storageRow{}
	for _, id := range s.storageIDs() {
		row, _, err := s.storageRow(id)
		if err != nil {
			continue
		}
		if _, err := s.storeByID(id); err != nil {
			row.Error = err.Error()
		} else {
			row.Online = true
			row.Free, row.Total = diskFree(row.Path)
		}
		var st storageStats
		json.Unmarshal([]byte(s.setting(fmt.Sprintf("storage_stats:%d", id), "{}")), &st)
		row.Used, row.Chunks = st.Bytes, st.Chunks
		s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM snapshots WHERE storage_id=? AND deleted=0`, id).Scan(&row.Snapshots, &row.Logical)
		q := `SELECT COUNT(*) FROM plans WHERE storage_id=?`
		if row.Default {
			q = `SELECT COUNT(*) FROM plans WHERE storage_id=? OR storage_id=0`
		}
		s.db.QueryRow(q, id).Scan(&row.Plans)
		out = append(out, row)
	}
	writeJSON(w, out)
}

func (s *Server) handleStorageCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Name, Kind, Path, Username, Password string
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	req.Name, req.Path = strings.TrimSpace(req.Name), strings.TrimSpace(req.Path)
	if req.Name == "" || req.Path == "" {
		httpErr(w, 400, "Name und Pfad angeben")
		return
	}
	switch req.Kind {
	case "local":
		if !filepath.IsAbs(req.Path) {
			httpErr(w, 400, "Bitte einen absoluten Pfad angeben, z. B. D:\\Backups")
			return
		}
	case "smb":
		if !strings.HasPrefix(req.Path, `\\`) {
			httpErr(w, 400, `Netzwerkpfad im Format \\server\freigabe\ordner angeben`)
			return
		}
		if err := connectShare(req.Path, req.Username, req.Password); err != nil {
			httpErr(w, 400, "Anmeldung an der Freigabe: %v", err)
			return
		}
	default:
		httpErr(w, 400, "Art muss local oder smb sein")
		return
	}
	var dup int
	s.db.QueryRow(`SELECT COUNT(*) FROM storages WHERE lower(path)=lower(?)`, req.Path).Scan(&dup)
	if dup > 0 {
		httpErr(w, 409, "Dieser Pfad ist bereits ein Speicherort")
		return
	}
	if err := probeWritable(req.Path); err != nil {
		httpErr(w, 400, "Ordner nicht beschreibbar: %v", err)
		return
	}
	var secret []byte
	if req.Password != "" {
		secret = s.seal(req.Password)
	}
	res, err := s.db.Exec(`INSERT INTO storages(name,kind,path,username,secret,is_default,created) VALUES(?,?,?,?,?,0,?)`,
		req.Name, req.Kind, req.Path, req.Username, secret, now())
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	id, _ := res.LastInsertId()
	if _, err := s.storeByID(id); err != nil {
		s.db.Exec(`DELETE FROM storages WHERE id=?`, id)
		httpErr(w, 400, "%v", err)
		return
	}
	s.audit(u.Name, "storage_create", req.Name+" "+req.Path)
	writeJSON(w, map[string]any{"id": id})
}

func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, ".bastion-probe")
	if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(p)
}

func (s *Server) handleStorageUpdate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Name    *string `json:"name"`
		Default bool    `json:"is_default"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	id := pathID(r)
	if _, _, err := s.storageRow(id); err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		s.db.Exec(`UPDATE storages SET name=? WHERE id=?`, strings.TrimSpace(*req.Name), id)
	}
	if req.Default {
		if _, err := s.storeByID(id); err != nil {
			httpErr(w, 400, "Speicherort nicht erreichbar: %v", err)
			return
		}
		// plans that used "standard" keep their current storage
		old := s.defaultStorage()
		s.db.Exec(`UPDATE plans SET storage_id=? WHERE storage_id=0`, old)
		s.db.Exec(`UPDATE storages SET is_default=(id=?)`, id)
	}
	s.audit(u.Name, "storage_update", fmt.Sprint(id))
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleStorageDelete(w http.ResponseWriter, r *http.Request, u *user) {
	id := pathID(r)
	row, _, err := s.storageRow(id)
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	if row.Default {
		httpErr(w, 400, "Der Standard-Speicherort kann nicht entfernt werden")
		return
	}
	var snaps, plans int
	s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE storage_id=?`, id).Scan(&snaps)
	s.db.QueryRow(`SELECT COUNT(*) FROM plans WHERE storage_id=?`, id).Scan(&plans)
	if snaps > 0 || plans > 0 {
		httpErr(w, 409, "Speicherort wird noch von %d Plänen und %d Backups verwendet", plans, snaps)
		return
	}
	s.db.Exec(`DELETE FROM storages WHERE id=?`, id)
	s.storeMu.Lock()
	delete(s.stores, id)
	s.storeMu.Unlock()
	s.audit(u.Name, "storage_delete", row.Name+" (Dateien bleiben in "+row.Path+")")
	writeJSON(w, map[string]string{"ok": "1"})
}

type dirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Free  int64  `json:"free,omitempty"`
	Total int64  `json:"total,omitempty"`
	Label string `json:"label,omitempty"`
}

// handleStorageBrowse lists drives and folders of the server for the
// folder picker.
func (s *Server) handleStorageBrowse(w http.ResponseWriter, r *http.Request, u *user) {
	p := r.URL.Query().Get("path")
	out := []dirEntry{}
	if p == "" {
		for _, d := range listDrives() {
			out = append(out, d)
		}
		writeJSON(w, map[string]any{"path": "", "entries": out})
		return
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" && len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "$") || e.Name() == "System Volume Information" {
			continue
		}
		out = append(out, dirEntry{Name: e.Name(), Path: filepath.Join(p, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	free, total := diskFree(p)
	writeJSON(w, map[string]any{"path": p, "parent": parent, "entries": out, "free": free, "total": total})
}
