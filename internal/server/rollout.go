package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"
	"github.com/masterzen/winrm"
	"golang.org/x/crypto/ssh"
)

// ---------- credentials ----------

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request, u *user) {
	rows, _ := s.db.Query(`SELECT id,name,kind,username,created FROM credentials ORDER BY name`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var name, kind, username string
		rows.Scan(&id, &name, &kind, &username, &created)
		out = append(out, map[string]any{"id": id, "name": name, "kind": kind, "username": username, "created": created})
	}
	writeJSON(w, out)
}

func (s *Server) handleCredentialCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct{ Name, Kind, Username, Secret string }
	if err := readJSON(r, &req); err != nil || req.Name == "" || req.Username == "" || req.Secret == "" {
		httpErr(w, 400, "Name, Benutzer und Passwort/Schlüssel angeben")
		return
	}
	if req.Kind != "windows" && req.Kind != "ssh" {
		httpErr(w, 400, "Art muss windows oder ssh sein")
		return
	}
	s.db.Exec(`INSERT INTO credentials(name,kind,username,secret,created) VALUES(?,?,?,?,?)`, req.Name, req.Kind, req.Username, s.seal(req.Secret), now())
	s.audit(u.Name, "credential_create", req.Name+" ("+req.Username+")")
	writeJSON(w, map[string]string{"ok": "1"})
}

func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request, u *user) {
	s.db.Exec(`DELETE FROM credentials WHERE id=?`, pathID(r))
	s.audit(u.Name, "credential_delete", r.PathValue("id"))
	writeJSON(w, map[string]string{"ok": "1"})
}

type cred struct {
	Kind, User, Secret string
}

func (s *Server) cred(id int64) (*cred, error) {
	var c cred
	var sec []byte
	if err := s.db.QueryRow(`SELECT kind,username,secret FROM credentials WHERE id=?`, id).Scan(&c.Kind, &c.User, &sec); err != nil {
		return nil, errors.New("Zugangsdaten nicht gefunden")
	}
	var err error
	c.Secret, err = s.unseal(sec)
	return &c, err
}

// ---------- discovery ----------

type host struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	OS       string `json:"os"`
	LastSeen int64  `json:"last_logon,omitempty"`
	SMB      bool   `json:"smb"`
	WinRM    bool   `json:"winrm"`
	SSH      bool   `json:"ssh"`
	AgentID  string `json:"agent_id,omitempty"`
}

func (s *Server) markKnown(hosts []host) {
	rows, err := s.db.Query(`SELECT id,lower(hostname),ips FROM agents WHERE removed=0`)
	if err != nil {
		return
	}
	byName, byIP := map[string]string{}, map[string]string{}
	for rows.Next() {
		var id, name, ips string
		rows.Scan(&id, &name, &ips)
		byName[name] = id
		var list []string
		json.Unmarshal([]byte(ips), &list)
		for _, ip := range list {
			byIP[ip] = id
		}
	}
	rows.Close()
	for i := range hosts {
		short := strings.ToLower(strings.SplitN(hosts[i].Name, ".", 2)[0])
		if id, ok := byName[short]; ok && short != "" {
			hosts[i].AgentID = id
		} else if id, ok := byIP[hosts[i].IP]; ok {
			hosts[i].AgentID = id
		}
	}
}

func (s *Server) handleDiscoverLDAP(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Server string `json:"server"`
		Base   string `json:"base"`
		CredID int64  `json:"cred_id"`
		TLS    bool   `json:"tls"`
	}
	if err := readJSON(r, &req); err != nil || req.Server == "" {
		httpErr(w, 400, "Domänencontroller angeben")
		return
	}
	c, err := s.cred(req.CredID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	url := "ldap://" + req.Server + ":389"
	if req.TLS {
		url = "ldaps://" + req.Server + ":636"
	}
	l, err := ldap.DialURL(url, ldap.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}))
	if err != nil {
		httpErr(w, 502, "Verbindung zu %s: %v", req.Server, err)
		return
	}
	defer l.Close()
	if !req.TLS {
		// try to protect the bind with StartTLS (domain controllers usually support it)
		l.StartTLS(tlsInsecure())
	}
	if err := l.Bind(c.User, c.Secret); err != nil {
		httpErr(w, 403, "Anmeldung am Verzeichnis: %v", err)
		return
	}
	base := req.Base
	if base == "" {
		res, err := l.Search(ldap.NewSearchRequest("", ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 10, false,
			"(objectClass=*)", []string{"defaultNamingContext"}, nil))
		if err != nil || len(res.Entries) == 0 {
			httpErr(w, 502, "Basis-DN nicht ermittelbar, bitte angeben")
			return
		}
		base = res.Entries[0].GetAttributeValue("defaultNamingContext")
	}
	res, err := l.SearchWithPaging(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 60, false,
		"(&(objectCategory=computer)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))",
		[]string{"dNSHostName", "cn", "operatingSystem", "lastLogonTimestamp"}, nil), 500)
	if err != nil {
		httpErr(w, 502, "Suche: %v", err)
		return
	}
	hosts := []host{}
	for _, e := range res.Entries {
		h := host{Name: e.GetAttributeValue("dNSHostName"), OS: e.GetAttributeValue("operatingSystem")}
		if h.Name == "" {
			h.Name = e.GetAttributeValue("cn")
		}
		var ft int64
		fmt.Sscan(e.GetAttributeValue("lastLogonTimestamp"), &ft)
		if ft > 0 {
			h.LastSeen = ft/10000000 - 11644473600
		}
		hosts = append(hosts, h)
	}
	sort.Slice(hosts, func(i, j int) bool { return strings.ToLower(hosts[i].Name) < strings.ToLower(hosts[j].Name) })
	s.markKnown(hosts)
	writeJSON(w, hosts)
}

// tlsInsecure is used for StartTLS to domain controllers, whose certificates
// usually come from an internal CA. It still encrypts the bind password.
func tlsInsecure() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

func probe(host string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 800*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (s *Server) handleDiscoverScan(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct{ CIDR string }
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, "ungültige Anfrage")
		return
	}
	_, ipn, err := net.ParseCIDR(strings.TrimSpace(req.CIDR))
	if err != nil || ipn.IP.To4() == nil {
		httpErr(w, 400, "IPv4-Netz im Format 192.168.1.0/24 angeben")
		return
	}
	ones, bits := ipn.Mask.Size()
	if bits-ones > 12 {
		httpErr(w, 400, "Netz zu groß (maximal /20)")
		return
	}
	start := binary.BigEndian.Uint32(ipn.IP.To4())
	count := uint32(1) << (bits - ones)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 128)
	hosts := []host{}
	for i := uint32(1); i < count-1 || (count <= 2 && i < count); i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, start+i)
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			h := host{IP: ip, SMB: probe(ip, 445), WinRM: probe(ip, 5985), SSH: probe(ip, 22)}
			if !h.SMB && !h.WinRM && !h.SSH {
				return
			}
			if names, err := net.LookupAddr(ip); err == nil && len(names) > 0 {
				h.Name = strings.TrimSuffix(names[0], ".")
			}
			switch {
			case h.SMB || h.WinRM:
				h.OS = "Windows"
			case h.SSH:
				h.OS = "Linux/Unix"
			}
			mu.Lock()
			hosts = append(hosts, h)
			mu.Unlock()
		}(ip.String())
	}
	wg.Wait()
	sort.Slice(hosts, func(i, j int) bool {
		return binary.BigEndian.Uint32(net.ParseIP(hosts[i].IP).To4()) < binary.BigEndian.Uint32(net.ParseIP(hosts[j].IP).To4())
	})
	s.markKnown(hosts)
	writeJSON(w, hosts)
}

// ---------- rollout ----------

func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request, u *user) {
	rows, _ := s.db.Query(`SELECT id,target,method,status,message,agent_id,created,updated FROM rollouts ORDER BY id DESC LIMIT 500`)
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created, updated int64
		var target, method, status, msg, agent string
		rows.Scan(&id, &target, &method, &status, &msg, &agent, &created, &updated)
		out = append(out, map[string]any{"id": id, "target": target, "method": method, "status": status, "message": msg,
			"agent_id": agent, "created": created, "updated": updated})
	}
	writeJSON(w, out)
}

func (s *Server) handleRolloutCreate(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Targets []string `json:"targets"`
		Method  string   `json:"method"`
		CredID  int64    `json:"cred_id"`
		PlanID  int64    `json:"plan_id"`
		Group   string   `json:"group"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Targets) == 0 {
		httpErr(w, 400, "Mindestens einen Rechner auswählen")
		return
	}
	if _, err := s.cred(req.CredID); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if req.Method == "" {
		req.Method = "auto"
	}
	var ids []int64
	for _, t := range req.Targets {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		res, err := s.db.Exec(`INSERT INTO rollouts(target,method,cred_id,plan_id,grp,status,created,updated) VALUES(?,?,?,?,?,?,?,?)`,
			t, req.Method, req.CredID, req.PlanID, req.Group, "queued", now(), now())
		if err == nil {
			id, _ := res.LastInsertId()
			ids = append(ids, id)
		}
	}
	s.audit(u.Name, "rollout", fmt.Sprintf("%d Rechner (%s)", len(ids), req.Method))
	go s.runRollouts(ids)
	writeJSON(w, map[string]any{"queued": len(ids)})
}

func (s *Server) handleRolloutRetry(w http.ResponseWriter, r *http.Request, u *user) {
	id := pathID(r)
	// a manual retry starts a new retry window
	s.db.Exec(`UPDATE rollouts SET status='queued',message='',attempts=0,next_try=0,created=?,updated=? WHERE id=? AND status<>'done'`, now(), now(), id)
	go s.runRollouts([]int64{id})
	writeJSON(w, map[string]string{"ok": "1"})
}

// unreachableErr marks failures that may go away by themselves (machine
// switched off, not in the network yet), so the rollout is retried later.
type unreachableErr struct{ msg string }

func (e unreachableErr) Error() string { return e.msg }

func unreachable(format string, args ...any) error {
	return unreachableErr{fmt.Sprintf(format, args...)}
}

func retryable(err error) bool {
	var u unreachableErr
	if errors.As(err, &u) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"dial tcp", "i/o timeout", "connection refused", "no route to host", "host is unreachable", "actively refused"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// rolloutFailed retries unreachable machines every rollout_retry_minutes
// for rollout_retry_hours; other errors (wrong password, missing rights)
// need the administrator and fail at once.
func (s *Server) rolloutFailed(id int64, err error) {
	hours := int64(s.settingInt("rollout_retry_hours", 24))
	every := int64(s.settingInt("rollout_retry_minutes", 15))
	if !retryable(err) || hours <= 0 || every <= 0 {
		s.rolloutStatus(id, "failed", err.Error())
		return
	}
	var attempts, created int64
	s.db.QueryRow(`SELECT attempts,created FROM rollouts WHERE id=?`, id).Scan(&attempts, &created)
	attempts++
	next := now() + every*60
	if next > created+hours*3600 {
		s.db.Exec(`UPDATE rollouts SET attempts=? WHERE id=?`, attempts, id)
		s.rolloutStatus(id, "failed", fmt.Sprintf("%v (nach %d Versuchen in %d h aufgegeben)", err, attempts, hours))
		return
	}
	s.db.Exec(`UPDATE rollouts SET attempts=?,next_try=? WHERE id=?`, attempts, next, id)
	s.rolloutStatus(id, "waiting", fmt.Sprintf("%v – neuer Versuch um %s (Versuch %d)", err, time.Unix(next, 0).Format("15:04"), attempts+1))
}

// retryRollouts starts waiting rollouts that are due (called by the scheduler).
func (s *Server) retryRollouts() {
	rows, err := s.db.Query(`SELECT id FROM rollouts WHERE status='waiting' AND next_try<=?`, now())
	if err != nil {
		return
	}
	var due []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		due = append(due, id)
	}
	rows.Close()
	var ids []int64
	for _, id := range due {
		res, _ := s.db.Exec(`UPDATE rollouts SET status='queued',updated=? WHERE id=? AND status='waiting'`, now(), id)
		if n, _ := res.RowsAffected(); n > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		go s.runRollouts(ids)
	}
}

var rolloutSem = make(chan struct{}, 8)

func (s *Server) runRollouts(ids []int64) {
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			rolloutSem <- struct{}{}
			defer func() { <-rolloutSem }()
			s.runRollout(id)
		}(id)
	}
	wg.Wait()
}

func (s *Server) rolloutStatus(id int64, status, msg string) {
	// the agent may have enrolled already (status done) before the push
	// reports its last step; that must not be overwritten
	res, _ := s.db.Exec(`UPDATE rollouts SET status=?,message=?,updated=? WHERE id=? AND (status<>'done' OR ?<>'running')`, status, msg, now(), id, status)
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	s.publish("rollout", map[string]any{"id": id, "status": status, "message": msg})
}

func (s *Server) runRollout(id int64) {
	var target, method, grp string
	var credID, planID int64
	if err := s.db.QueryRow(`SELECT target,method,cred_id,plan_id,grp FROM rollouts WHERE id=?`, id).Scan(&target, &method, &credID, &planID, &grp); err != nil {
		return
	}
	c, err := s.cred(credID)
	if err != nil {
		s.rolloutStatus(id, "failed", err.Error())
		return
	}
	if method == "auto" {
		s.rolloutStatus(id, "running", "prüfe erreichbare Dienste")
		if method = autoMethod(target, c); method == "" {
			s.rolloutFailed(id, unreachable("Weder SMB (445), WinRM (5985) noch SSH (22) erreichbar – Rechner aus oder Firewall"))
			return
		}
	}
	tok := s.createToken("rollout:"+target, grp, planID, 1, 1)
	s.db.Exec(`UPDATE rollouts SET method=?,token_hash=? WHERE id=?`, method, sha(tok), id)
	s.rolloutStatus(id, "running", "verbinde ("+method+")")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	progress := func(msg string) { s.rolloutStatus(id, "running", msg) }
	switch method {
	case "smb":
		err = s.pushSMB(ctx, target, c, tok, progress)
	case "winrm":
		err = s.pushWinRM(ctx, target, c, tok, progress)
	case "ssh":
		err = s.pushSSH(ctx, target, c, tok, progress)
	default:
		err = fmt.Errorf("unbekannte Methode %s", method)
	}
	if err != nil {
		s.rolloutFailed(id, err)
		return
	}
	// enrollment sets the agent (and status done); give it a moment
	for i := 0; i < 30; i++ {
		var agent string
		s.db.QueryRow(`SELECT agent_id FROM rollouts WHERE id=?`, id).Scan(&agent)
		if agent != "" {
			s.rolloutStatus(id, "done", "Agent installiert und angemeldet")
			return
		}
		time.Sleep(2 * time.Second)
	}
	s.rolloutStatus(id, "failed", "installiert, aber der Agent hat sich nicht angemeldet – erreicht der Rechner "+s.PublicURL()+"?")
}

// installScript downloads the agent from this server (checking the pinned
// certificate) and installs it.
func (s *Server) installScript(tok string) string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'
[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12
[Net.ServicePointManager]::ServerCertificateValidationCallback={param($a,$c) ([BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($c.GetRawCertData())).Replace('-','').ToLower() -eq '%[3]s')}
$exe = Join-Path $env:TEMP 'bastion-agent-setup.exe'
(New-Object Net.WebClient).DownloadFile('%[1]s/dl/agents/windows-amd64/bastion-agent.exe', $exe)
& $exe install --server '%[1]s' --token '%[2]s' --fingerprint '%[3]s'
$rc = $LASTEXITCODE
Remove-Item $exe -Force -ErrorAction SilentlyContinue
exit $rc`, s.PublicURL(), tok, s.fp)
}

func autoMethod(target string, c *cred) string {
	switch {
	case c.Kind == "ssh" && probe(target, 22):
		return "ssh"
	case c.Kind == "windows" && smbPushSupported && probe(target, 445):
		return "smb"
	case c.Kind == "windows" && probe(target, 5985):
		return "winrm"
	}
	return ""
}

func winrmClient(target string, c *cred, timeout time.Duration) (*winrm.Client, error) {
	endpoint := winrm.NewEndpoint(target, 5985, false, true, nil, nil, nil, timeout)
	params := *winrm.DefaultParameters
	params.TransportDecorator = func() winrm.Transporter {
		enc, err := winrm.NewEncryption("ntlm")
		if err != nil {
			return &winrm.ClientNTLM{}
		}
		return enc
	}
	return winrm.NewClientWithParameters(endpoint, c.User, c.Secret, &params)
}

func (s *Server) pushWinRM(ctx context.Context, target string, c *cred, tok string, progress func(string)) error {
	if _, ok := agentBinaryFor("windows", "amd64"); !ok {
		return errors.New("Agent-Programm fehlt in data/agents/windows-amd64/")
	}
	client, err := winrmClient(target, c, 5*time.Minute)
	if err != nil {
		return err
	}
	progress("installiere über WinRM")
	stdout, stderr, code, err := client.RunPSWithContext(ctx, s.installScript(tok))
	if err != nil {
		return fmt.Errorf("WinRM: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("Installation fehlgeschlagen (Code %d): %s %s", code, strings.TrimSpace(stdout), firstLine(stderr))
	}
	progress("installiert, warte auf Anmeldung")
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

// hostKey implements trust-on-first-use for SSH host keys.
func (s *Server) hostKey(target string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		k := "sshhost:" + strings.ToLower(target)
		got := base64.StdEncoding.EncodeToString(key.Marshal())
		if known := s.setting(k, ""); known != "" {
			if known != got {
				return fmt.Errorf("SSH-Hostschlüssel von %s hat sich geändert (möglicher Angriff). Eintrag %s in den Einstellungen löschen, falls gewollt", target, k)
			}
			return nil
		}
		s.setSetting(k, got)
		return nil
	}
}

func shellQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'" }

type sshConn struct {
	cl    *ssh.Client
	isKey bool
}

func (s *Server) sshDial(target string, c *cred) (*sshConn, error) {
	cfg := &ssh.ClientConfig{User: c.User, HostKeyCallback: s.hostKey(target), Timeout: 15 * time.Second}
	isKey := strings.Contains(c.Secret, "PRIVATE KEY")
	if isKey {
		signer, err := ssh.ParsePrivateKey([]byte(c.Secret))
		if err != nil {
			return nil, fmt.Errorf("privater Schlüssel: %v", err)
		}
		cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	} else {
		cfg.Auth = []ssh.AuthMethod{ssh.Password(c.Secret), ssh.KeyboardInteractive(func(_, _ string, q []string, _ []bool) ([]string, error) {
			a := make([]string, len(q))
			for i := range a {
				a[i] = c.Secret
			}
			return a, nil
		})}
	}
	cl, err := ssh.Dial("tcp", net.JoinHostPort(target, "22"), cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH: %w", err)
	}
	return &sshConn{cl: cl, isKey: isKey}, nil
}

func (sc *sshConn) run(cmd string, stdin []byte) (string, error) {
	sess, err := sc.cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	out, err := sess.CombinedOutput(cmd)
	return strings.TrimSpace(string(out)), err
}

// sudo runs cmd as root, with the password on stdin where needed.
func (sc *sshConn) sudo(c *cred, cmd string) (string, error) {
	switch {
	case c.User == "root":
		return sc.run("sh -c "+shellQuote(cmd), nil)
	case sc.isKey:
		return sc.run("sudo -n sh -c "+shellQuote(cmd), nil)
	default:
		return sc.run("sudo -S -p '' sh -c "+shellQuote(cmd), []byte(c.Secret+"\n"))
	}
}

func (s *Server) pushSSH(ctx context.Context, target string, c *cred, tok string, progress func(string)) error {
	sc, err := s.sshDial(target, c)
	if err != nil {
		return err
	}
	defer sc.cl.Close()
	run := sc.run
	archOut, err := run("uname -m", nil)
	if err != nil {
		return fmt.Errorf("uname: %v", err)
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[archOut]
	bin, ok := agentBinaryFor("linux", arch)
	if !ok {
		return fmt.Errorf("Agent-Programm für linux-%s fehlt in data/agents/linux-%s/", arch, arch)
	}
	data, err := os.ReadFile(s.path(bin.Path))
	if err != nil {
		return err
	}
	progress("übertrage Agent")
	tmp := "/tmp/bastion-agent-" + randCode(6)
	if out, err := run("umask 077 && cat > "+tmp+" && chmod 700 "+tmp+" && sha256sum "+tmp, data); err != nil || !strings.HasPrefix(out, bin.SHA256) {
		return fmt.Errorf("Übertragung fehlgeschlagen: %v %s", err, out)
	}
	progress("installiere")
	inst := fmt.Sprintf("%s install --server %s --token %s --fingerprint %s; rc=$?; rm -f %s; exit $rc",
		tmp, shellQuote(s.PublicURL()), shellQuote(tok), shellQuote(s.fp), tmp)
	out, err := sc.sudo(c, inst)
	if err != nil {
		run("rm -f "+tmp, nil)
		return fmt.Errorf("Installation: %v: %s", err, out)
	}
	progress("installiert, warte auf Anmeldung")
	return nil
}
