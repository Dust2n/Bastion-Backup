package server

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"bastion/internal/proto"
	"bastion/internal/repo"
)

// Server backup (disaster recovery of the backup server itself).
//
// The backups are encrypted with keys/repo.key; without it and the
// database they cannot be used. A server backup is a passphrase-encrypted
// file with the database, all keys and the TLS certificate (agents pin
// its fingerprint). It is written daily into every storage, so a new
// server can be set up from any of them:
//
//	bastion-server restore -from <file or storage folder> -data <data dir>

const (
	serverBackupMagic = "BASTIONSRV1\n"
	serverBackupDir   = "server-backup"
	serverBackupKeep  = 14
)

type serverBackupManifest struct {
	Version     string    `json:"version"`
	Time        time.Time `json:"time"`
	Hostname    string    `json:"hostname"`
	PublicURL   string    `json:"public_url"`
	Fingerprint string    `json:"fingerprint"`
}

type serverBackupStatus struct {
	Time      int64    `json:"time"`
	OK        bool     `json:"ok"`
	Message   string   `json:"message"`
	Locations []string `json:"locations"`
	Offsite   bool     `json:"offsite"` // at least one copy outside the data directory
	Size      int64    `json:"size"`
}

func deriveKey(pass string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
}

func encryptBundle(plain []byte, pass string) ([]byte, error) {
	salt := random(16)
	key, err := deriveKey(pass, salt)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(block)
	nonce := random(g.NonceSize())
	out := append([]byte(serverBackupMagic), salt...)
	out = append(out, nonce...)
	return g.Seal(out, nonce, plain, []byte(serverBackupMagic)), nil
}

func decryptBundle(b []byte, pass string) ([]byte, error) {
	if !bytes.HasPrefix(b, []byte(serverBackupMagic)) {
		return nil, errors.New("keine Bastion-Serversicherung")
	}
	b = b[len(serverBackupMagic):]
	if len(b) < 16+12+16 {
		return nil, errors.New("Datei beschädigt")
	}
	key, err := deriveKey(pass, b[:16])
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(block)
	plain, err := g.Open(nil, b[16:16+g.NonceSize()], b[16+g.NonceSize():], []byte(serverBackupMagic))
	if err != nil {
		return nil, errors.New("Passwort falsch oder Datei beschädigt")
	}
	return plain, nil
}

// buildServerBackup creates the encrypted bundle.
func (s *Server) buildServerBackup(pass string) ([]byte, error) {
	tmp := s.path("cache", fmt.Sprintf("db-export-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	// a consistent copy of the live database
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return nil, fmt.Errorf("Datenbank exportieren: %w", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	host, _ := os.Hostname()
	man, _ := json.MarshalIndent(serverBackupManifest{Version: proto.Version, Time: time.Now().UTC(), Hostname: host,
		PublicURL: s.PublicURL(), Fingerprint: s.fp}, "", "  ")
	if err := add("manifest.json", man); err != nil {
		return nil, err
	}
	db, err := os.ReadFile(tmp)
	if err != nil {
		return nil, err
	}
	if err := add("bastion.db", db); err != nil {
		return nil, err
	}
	// the key in use (it may come from BASTION_REPO_KEY instead of the file)
	if err := add("keys/repo.key", s.repoKey); err != nil {
		return nil, err
	}
	for _, f := range []string{"keys/signing.key", "keys/vault.key", "tls/cert.pem", "tls/key.pem"} {
		b, err := os.ReadFile(s.path(filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if err := add(f, b); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return encryptBundle(buf.Bytes(), pass)
}

func (s *Server) serverBackupPass() string {
	v := s.setting("dr_passphrase", "")
	if v == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return ""
	}
	pass, err := s.unseal(b)
	if err != nil {
		return ""
	}
	return pass
}

// runServerBackup writes a server backup into every storage.
func (s *Server) runServerBackup(by string) (serverBackupStatus, error) {
	st := serverBackupStatus{Time: now()}
	save := func(err error) (serverBackupStatus, error) {
		if err != nil {
			st.Message = err.Error()
			s.log.Printf("Server-Sicherung: %v", err)
		}
		s.setSetting("dr_status", jsonStr(st))
		return st, err
	}
	pass := s.serverBackupPass()
	if pass == "" {
		return save(errors.New("kein Passwort für die Server-Sicherung festgelegt"))
	}
	data, err := s.buildServerBackup(pass)
	if err != nil {
		return save(err)
	}
	st.Size = int64(len(data))
	name := "bastion-server-" + time.Now().Format("20060102-150405") + ".bsb"
	dataDir, _ := filepath.Abs(s.opt.DataDir)
	var errs []string
	for _, id := range s.storageIDs() {
		label := fmt.Sprint(id)
		if row, _, err := s.storageRow(id); err == nil {
			label = row.Name
		}
		store, err := s.storeByID(id)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		dir := filepath.Join(store.Dir(), serverBackupDir)
		os.MkdirAll(dir, 0o700)
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		pruneServerBackups(dir)
		st.Locations = append(st.Locations, label)
		if abs, _ := filepath.Abs(store.Dir()); !strings.HasPrefix(strings.ToLower(abs), strings.ToLower(dataDir)+string(filepath.Separator)) {
			st.Offsite = true
		}
	}
	if len(st.Locations) == 0 {
		return save(fmt.Errorf("in keinen Speicherort geschrieben: %s", strings.Join(errs, "; ")))
	}
	st.OK = len(errs) == 0
	st.Message = fmt.Sprintf("Gesichert nach: %s", strings.Join(st.Locations, ", "))
	if len(errs) > 0 {
		st.Message += " · Fehler: " + strings.Join(errs, "; ")
	}
	if !st.Offsite {
		st.Message += " · Achtung: nur auf diesem Server – einen Speicherort auf einem anderen Gerät anlegen"
	}
	s.audit(by, "server_backup", st.Message)
	return save(nil)
}

func pruneServerBackups(dir string) {
	m, _ := filepath.Glob(filepath.Join(dir, "bastion-server-*.bsb"))
	sort.Strings(m)
	for len(m) > serverBackupKeep {
		os.Remove(m[0])
		m = m[1:]
	}
}

// ---------- API ----------

func (s *Server) handleServerBackupStatus(w http.ResponseWriter, r *http.Request, u *user) {
	var st *serverBackupStatus
	json.Unmarshal([]byte(s.setting("dr_status", "null")), &st)
	writeJSON(w, map[string]any{
		"passphrase_set": s.setting("dr_passphrase", "") != "",
		"time":           s.setting("dr_time", "02:15"),
		"last":           st,
	})
}

func (s *Server) handleServerBackupSave(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Passphrase string `json:"passphrase"`
		Time       string `json:"time"`
	}
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if req.Passphrase != "" {
		if len([]rune(req.Passphrase)) < 12 {
			httpErr(w, 400, "Das Passwort muss mindestens 12 Zeichen haben")
			return
		}
		s.setSetting("dr_passphrase", base64.StdEncoding.EncodeToString(s.seal(req.Passphrase)))
		s.audit(u.Name, "server_backup_passphrase", "geändert")
	}
	if req.Time != "" {
		var h, m int
		if _, err := fmt.Sscanf(req.Time, "%d:%d", &h, &m); err != nil || h > 23 || m > 59 {
			httpErr(w, 400, "Uhrzeit im Format HH:MM angeben")
			return
		}
		s.setSetting("dr_time", fmt.Sprintf("%02d:%02d", h, m))
	}
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleServerBackupRun(w http.ResponseWriter, r *http.Request, u *user) {
	st, err := s.runServerBackup(u.Name)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleServerBackupDownload(w http.ResponseWriter, r *http.Request, u *user) {
	pass := s.serverBackupPass()
	if pass == "" {
		httpErr(w, 409, "Zuerst ein Passwort für die Server-Sicherung festlegen")
		return
	}
	data, err := s.buildServerBackup(pass)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.audit(u.Name, "server_backup_download", "")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="bastion-server-`+time.Now().Format("20060102-150405")+`.bsb"`)
	w.Write(data)
}

// ---------- restore (command line, before the server starts) ----------

// FindServerBackup returns the newest server backup in a folder (a
// storage folder, its server-backup subfolder or any folder), or path
// itself if it is a file.
func FindServerBackup(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return path, nil
	}
	var all []string
	for _, d := range []string{path, filepath.Join(path, serverBackupDir)} {
		m, _ := filepath.Glob(filepath.Join(d, "bastion-server-*.bsb"))
		all = append(all, m...)
	}
	if len(all) == 0 {
		return "", fmt.Errorf("keine Server-Sicherung (bastion-server-*.bsb) in %s", path)
	}
	sort.Slice(all, func(i, j int) bool { return filepath.Base(all[i]) < filepath.Base(all[j]) })
	return all[len(all)-1], nil
}

// RestoreServerBackup writes database, keys and certificate from a server
// backup into dataDir. Backups made after the server backup are imported
// from the storages at the next start.
func RestoreServerBackup(file, dataDir, pass string, force bool, out io.Writer) error {
	enc, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	plain, err := decryptBundle(enc, pass)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		return fmt.Errorf("Inhalt beschädigt: %w", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		files[f.Name] = b
	}
	var man serverBackupManifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil {
		return errors.New("Inhalt beschädigt (manifest.json)")
	}
	for _, need := range []string{"bastion.db", "keys/repo.key", "keys/signing.key", "keys/vault.key", "tls/cert.pem", "tls/key.pem"} {
		if _, ok := files[need]; !ok {
			return fmt.Errorf("Inhalt unvollständig (%s fehlt)", need)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bastion.db")); err == nil && !force {
		return fmt.Errorf("%s enthält bereits eine Datenbank – mit -force überschreiben (der bisherige Stand wird nach *.before-restore umbenannt)", dataDir)
	}
	fmt.Fprintf(out, "Sicherung vom %s, Server %s (Version %s)\n", man.Time.Local().Format("02.01.2006 15:04"), man.Hostname, man.Version)
	for _, d := range []string{"", "keys", "tls"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o700); err != nil {
			return err
		}
	}
	stamp := time.Now().Format("20060102-150405")
	for name, b := range files {
		if name == "manifest.json" {
			continue
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(name))
		if _, err := os.Stat(dst); err == nil {
			os.Rename(dst, dst+".before-restore-"+stamp)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s\n", name)
	}
	for _, ext := range []string{"-wal", "-shm"} {
		os.Remove(filepath.Join(dataDir, "bastion.db"+ext))
	}
	db, err := openDB(filepath.Join(dataDir, "bastion.db"))
	if err != nil {
		return fmt.Errorf("wiederhergestellte Datenbank: %w", err)
	}
	defer db.Close()
	db.Exec(`INSERT INTO settings(key,value) VALUES('restore_import_after',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(man.Time.Unix()))
	rows, err := db.Query(`SELECT name,kind,path FROM storages ORDER BY id`)
	if err == nil {
		fmt.Fprintln(out, "Speicherorte (müssen unter diesen Pfaden erreichbar sein):")
		for rows.Next() {
			var n, k, p string
			rows.Scan(&n, &k, &p)
			fmt.Fprintf(out, "  %-20s %s %s\n", n, k, p)
		}
		rows.Close()
	}
	fmt.Fprintf(out, "Fertig. Zertifikat-Fingerabdruck %s – die Agents verbinden sich wieder, sobald der Server unter %s erreichbar ist.\n", man.Fingerprint, man.PublicURL)
	return nil
}

// importNewerSnapshots runs once after a restore: backups made after the
// server backup exist only as snapshots/*.json in the storages. They are
// added if all their chunks are present.
func (s *Server) importNewerSnapshots() {
	var since int64
	if _, err := fmt.Sscan(s.setting("restore_import_after", ""), &since); err != nil || since == 0 {
		return
	}
	n := 0
	for _, id := range s.storageIDs() {
		st, err := s.storeByID(id)
		if err != nil {
			s.log.Printf("Import nach Wiederherstellung: Speicherort %d nicht erreichbar: %v", id, err)
			return // try again at the next start
		}
		files, _ := filepath.Glob(filepath.Join(st.Dir(), "snapshots", "*.json"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var snap repo.Snapshot
			if json.Unmarshal(b, &snap) != nil || snap.ID == "" || snap.Time.Unix() <= since {
				continue
			}
			var exists int
			s.db.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id=?`, snap.ID).Scan(&exists)
			if exists > 0 {
				continue
			}
			complete := true
			if err := snap.Refs(st.Get, func(cid repo.ID) {
				if !st.Has(cid) {
					complete = false
				}
			}); err != nil || !complete {
				s.log.Printf("Import: Backup %s unvollständig, übersprungen", snap.ID)
				continue
			}
			var nfiles, size, newBytes int64
			for _, v := range snap.Volumes {
				nfiles += v.Files
				size += v.Bytes
				newBytes += v.NewBytes
			}
			if _, err := s.db.Exec(`INSERT INTO snapshots(id,agent_id,plan_id,time,kind,files,bytes,new_bytes,data,storage_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
				snap.ID, snap.AgentID, snap.PlanID, snap.Time.Unix(), snap.Kind, nfiles, size, newBytes, string(b), id); err == nil {
				os.WriteFile(s.path("snapshots", snap.ID+".json"), b, 0o600)
				n++
			}
		}
	}
	s.db.Exec(`DELETE FROM settings WHERE key='restore_import_after'`)
	s.log.Printf("Nach Wiederherstellung %d neuere Backups aus den Speicherorten übernommen", n)
	if n > 0 {
		s.audit("-", "restore_import", fmt.Sprintf("%d Backups übernommen", n))
	}
}
