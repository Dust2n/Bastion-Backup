package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randToken(n int) string { return base64.RawURLEncoding.EncodeToString(random(n)) }

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randCode(n int) string {
	b := random(n)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

func hashPassword(pw string) string {
	salt := random(16)
	h := argon2.IDKey([]byte(pw), salt, 2, 64*1024, 2, 32)
	return "argon2id$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(h)
}

func checkPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 3 || p[0] != "argon2id" {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[1])
	want, err2 := base64.RawStdEncoding.DecodeString(p[2])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, 2, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Server) ensureAdmin() error {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return nil
	}
	pw := randCode(12)
	if _, err := s.db.Exec(`INSERT INTO users(name,pw,role,created) VALUES('admin',?,?,?)`, hashPassword(pw), "admin", now()); err != nil {
		return err
	}
	f := s.path("initial-admin-password.txt")
	os.WriteFile(f, []byte("Benutzer: admin\nPasswort: "+pw+"\n"), 0o600)
	s.log.Printf("Erster Administrator angelegt: admin / %s (auch in %s; bitte nach der Anmeldung ändern)", pw, f)
	return nil
}

type user struct {
	ID   int64
	Name string
	Role string
}

var roleRank = map[string]int{"viewer": 1, "operator": 2, "admin": 3}

// sessionUser returns the logged-in user from the session cookie (web UI)
// or from "Authorization: Bearer session:<token>" (rescue environment).
// header reports the latter, which is not sent automatically by browsers
// and therefore needs no CSRF check.
func (s *Server) sessionUser(r *http.Request) (u *user, header bool) {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer session:") {
		tok, header = strings.TrimPrefix(h, "Bearer session:"), true
	} else if c, err := r.Cookie("bastion_session"); err == nil {
		tok = c.Value
	}
	if tok == "" {
		return nil, false
	}
	var x user
	err := s.db.QueryRow(`SELECT u.id,u.name,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.hash=? AND s.expires>?`,
		sha(tok), now()).Scan(&x.ID, &x.Name, &x.Role)
	if err != nil {
		return nil, false
	}
	return &x, header
}

// admin wraps an admin API handler with authentication, role check and a
// CSRF check (mutating requests need the X-Bastion header, which browsers
// do not send cross-site without CORS).
func (s *Server) admin(role string, h func(http.ResponseWriter, *http.Request, *user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, header := s.sessionUser(r)
		if u == nil {
			httpErr(w, 401, "nicht angemeldet")
			return
		}
		if r.Method != "GET" && !header && r.Header.Get("X-Bastion") != "1" {
			httpErr(w, 403, "CSRF-Prüfung fehlgeschlagen")
			return
		}
		if roleRank[u.Role] < roleRank[role] {
			httpErr(w, 403, "keine Berechtigung (Rolle %s nötig)", role)
			return
		}
		h(w, r, u)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Password string }
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if !s.login.allow(clientIP(r)) {
		httpErr(w, 429, "zu viele Anmeldeversuche, bitte später erneut versuchen")
		return
	}
	var id int64
	var pw string
	err := s.db.QueryRow(`SELECT id,pw FROM users WHERE name=?`, req.Name).Scan(&id, &pw)
	if err != nil || !checkPassword(pw, req.Password) {
		time.Sleep(500 * time.Millisecond)
		s.audit(req.Name, "login_failed", clientIP(r))
		httpErr(w, 401, "Benutzername oder Passwort falsch")
		return
	}
	tok := randToken(32)
	s.db.Exec(`DELETE FROM sessions WHERE expires<?`, now())
	s.db.Exec(`INSERT INTO sessions(hash,user_id,expires) VALUES(?,?,?)`, sha(tok), id, now()+12*3600)
	http.SetCookie(w, &http.Cookie{Name: "bastion_session", Value: tok, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600})
	s.audit(req.Name, "login", clientIP(r))
	writeJSON(w, map[string]string{"ok": "1"})
}

// handleRescueLogin lets an administrator choose a backup from inside the
// rescue environment. It returns a short-lived session token for the
// Authorization header instead of a cookie.
func (s *Server) handleRescueLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Password string }
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	if !s.login.allow(clientIP(r)) {
		httpErr(w, 429, "zu viele Anmeldeversuche, bitte später erneut versuchen")
		return
	}
	var id int64
	var pw, role string
	err := s.db.QueryRow(`SELECT id,pw,role FROM users WHERE name=?`, req.Name).Scan(&id, &pw, &role)
	if err != nil || !checkPassword(pw, req.Password) {
		time.Sleep(500 * time.Millisecond)
		s.audit(req.Name, "rescue_login_failed", clientIP(r))
		httpErr(w, 401, "Benutzername oder Passwort falsch")
		return
	}
	if role != "admin" {
		httpErr(w, 403, "Für eine Image-Wiederherstellung ist die Rolle Administrator nötig")
		return
	}
	tok := randToken(32)
	s.db.Exec(`INSERT INTO sessions(hash,user_id,expires) VALUES(?,?,?)`, sha(tok), id, now()+2*3600)
	s.audit(req.Name, "rescue_login", clientIP(r))
	writeJSON(w, map[string]string{"token": tok})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("bastion_session"); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE hash=?`, sha(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "bastion_session", Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true})
	writeJSON(w, map[string]string{"ok": "1"})
}

// principal is an authenticated agent or rescue environment.
type principal struct {
	AgentID string
	// set for rescue environments (restore token)
	RestoreJob  int64
	RestoreSnap string
}

func (p *principal) restore() bool { return p.RestoreJob != 0 }

func (s *Server) agentAuth(r *http.Request) (*principal, error) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, errors.New("keine Anmeldung")
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	switch {
	case strings.HasPrefix(tok, "agent:"):
		parts := strings.SplitN(tok, ":", 3)
		if len(parts) != 3 {
			return nil, errors.New("ungültiges Token")
		}
		var hash string
		if err := s.db.QueryRow(`SELECT secret_hash FROM agents WHERE id=?`, parts[1]).Scan(&hash); err != nil {
			return nil, errors.New("Agent unbekannt")
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(sha(parts[2]))) != 1 {
			return nil, errors.New("Agent-Geheimnis falsch")
		}
		return &principal{AgentID: parts[1]}, nil
	case strings.HasPrefix(tok, "restore:"):
		p := &principal{}
		err := s.db.QueryRow(`SELECT job_id,snapshot_id,agent_id FROM restore_tokens WHERE hash=? AND expires>?`,
			sha(strings.TrimPrefix(tok, "restore:")), now()).Scan(&p.RestoreJob, &p.RestoreSnap, &p.AgentID)
		if err != nil {
			return nil, errors.New("Wiederherstellungs-Token ungültig oder abgelaufen")
		}
		return p, nil
	}
	return nil, errors.New("ungültiges Token")
}

func (s *Server) agentOnly(h func(http.ResponseWriter, *http.Request, *principal)) http.HandlerFunc {
	return s.agentOrRestore(func(w http.ResponseWriter, r *http.Request, p *principal) {
		if p.restore() {
			httpErr(w, 403, "nicht erlaubt")
			return
		}
		h(w, r, p)
	})
}

func (s *Server) agentOrRestore(h func(http.ResponseWriter, *http.Request, *principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.agentAuth(r)
		if err != nil {
			httpErr(w, 401, "%v", err)
			return
		}
		h(w, r, p)
	}
}

// seal/open encrypt stored credentials with the vault key.
func (s *Server) seal(plain string) []byte {
	block, _ := aes.NewCipher(s.vault)
	g, _ := cipher.NewGCM(block)
	nonce := random(g.NonceSize())
	return g.Seal(nonce, nonce, []byte(plain), nil)
}

func (s *Server) unseal(b []byte) (string, error) {
	block, _ := aes.NewCipher(s.vault)
	g, _ := cipher.NewGCM(block)
	if len(b) < g.NonceSize() {
		return "", errors.New("Daten beschädigt")
	}
	p, err := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
	return string(p), err
}
