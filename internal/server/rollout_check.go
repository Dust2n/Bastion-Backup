package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Pre-flight check: tests everything a rollout needs without changing the
// target machine, so problems show up before anything is installed.

type checkStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type checkResult struct {
	Target  string      `json:"target"`
	Method  string      `json:"method"`
	OK      bool        `json:"ok"`
	AgentID string      `json:"agent_id,omitempty"`
	Steps   []checkStep `json:"steps"`
}

func (s *Server) handleRolloutCheck(w http.ResponseWriter, r *http.Request, u *user) {
	var req struct {
		Targets []string `json:"targets"`
		Method  string   `json:"method"`
		CredID  int64    `json:"cred_id"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Targets) == 0 {
		httpErr(w, 400, "Mindestens einen Rechner auswählen")
		return
	}
	if len(req.Targets) > 256 {
		httpErr(w, 400, "höchstens 256 Rechner auf einmal prüfen")
		return
	}
	c, err := s.cred(req.CredID)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	out := make([]checkResult, len(req.Targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, t := range req.Targets {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			out[i] = s.checkTarget(ctx, strings.TrimSpace(t), req.Method, c)
		}(i, t)
	}
	wg.Wait()
	hosts := make([]host, len(out))
	for i := range out {
		hosts[i] = host{Name: out[i].Target, IP: out[i].Target}
	}
	s.markKnown(hosts)
	for i := range out {
		out[i].AgentID = hosts[i].AgentID
	}
	writeJSON(w, out)
}

func (s *Server) checkTarget(ctx context.Context, target, method string, c *cred) checkResult {
	res := checkResult{Target: target, OK: true}
	add := func(name string, err error, detail string) {
		st := checkStep{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			st.Detail = err.Error()
			res.OK = false
		}
		res.Steps = append(res.Steps, st)
	}
	fail := func(name string, err error) checkResult {
		add(name, err, "")
		return res
	}

	if net.ParseIP(target) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, target)
		if err != nil {
			return fail("Namensauflösung", fmt.Errorf("%s ist im DNS nicht bekannt – Namen prüfen oder IP-Adresse verwenden", target))
		}
		add("Namensauflösung", nil, strings.Join(addrs, ", "))
	}

	smb, wrm, sh := probe(target, 445), probe(target, 5985), probe(target, 22)
	var open []string
	for _, p := range []struct {
		ok   bool
		name string
	}{{smb, "SMB 445"}, {wrm, "WinRM 5985"}, {sh, "SSH 22"}} {
		if p.ok {
			open = append(open, p.name)
		}
	}
	if len(open) == 0 {
		return fail("Erreichbarkeit", errors.New("weder SMB (445), WinRM (5985) noch SSH (22) erreichbar – Rechner aus oder Firewall. Bei der Verteilung wird es automatisch später erneut versucht"))
	}
	add("Erreichbarkeit", nil, "offen: "+strings.Join(open, ", "))

	if method == "" || method == "auto" {
		method = autoMethod(target, c)
	}
	res.Method = method
	switch method {
	case "smb":
		if !smb {
			return fail("Methode", errors.New("SMB (445) nicht erreichbar"))
		}
		if _, ok := agentBinaryFor("windows", "amd64"); !ok {
			add("Agent-Programm", errors.New("fehlt in data/agents/windows-amd64/"), "")
		}
		if s.checkSMB(target, c, add) {
			add("Verbindung Rechner → Server", nil, "wird bei der Installation geprüft (SMB kann keine Befehle ausführen)")
		}
	case "winrm":
		if !wrm {
			return fail("Methode", errors.New("WinRM (5985) nicht erreichbar – auf dem Rechner \"winrm quickconfig\" ausführen"))
		}
		if _, ok := agentBinaryFor("windows", "amd64"); !ok {
			add("Agent-Programm", errors.New("fehlt in data/agents/windows-amd64/"), "")
		}
		s.checkWinRM(ctx, target, c, add)
	case "ssh":
		if !sh {
			return fail("Methode", errors.New("SSH (22) nicht erreichbar"))
		}
		s.checkSSH(target, c, add)
	default:
		if c.Kind == "ssh" {
			return fail("Methode", errors.New("SSH-Zugangsdaten, aber Port 22 ist zu"))
		}
		return fail("Methode", errors.New("Windows-Zugangsdaten, aber weder SMB noch WinRM erreichbar"))
	}
	return res
}

// serverHostPort is where agents connect to.
func (s *Server) serverHostPort() (string, string) {
	u, err := url.Parse(s.PublicURL())
	if err != nil {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u.Hostname(), port
}

func (s *Server) checkWinRM(ctx context.Context, target string, c *cred, add func(string, error, string)) {
	cl, err := winrmClient(target, c, 40*time.Second)
	if err != nil {
		add("Anmeldung (WinRM)", err, "")
		return
	}
	host, port := s.serverHostPort()
	script := fmt.Sprintf(`$p = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
"admin=" + $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
try { $t = New-Object Net.Sockets.TcpClient; $a = $t.BeginConnect('%s', %s, $null, $null)
  if ($a.AsyncWaitHandle.WaitOne(5000) -and $t.Connected) { "server=ok" } else { "server=Zeitüberschreitung" }; $t.Close()
} catch { "server=" + $_.Exception.InnerException.Message }`, host, port)
	stdout, stderr, _, err := cl.RunPSWithContext(ctx, script)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "401") {
			msg = "Benutzername oder Passwort falsch bzw. keine WinRM-Berechtigung (" + msg + ")"
		}
		add("Anmeldung (WinRM)", errors.New(msg), "")
		return
	}
	add("Anmeldung (WinRM)", nil, "Befehle können ausgeführt werden")
	vals := map[string]string{}
	for _, l := range strings.Split(stdout, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			vals[k] = v
		}
	}
	if vals["admin"] != "True" {
		add("Adminrechte", fmt.Errorf("Konto ist auf %s kein Administrator %s", target, firstLine(stderr)), "")
	} else {
		add("Adminrechte", nil, "vorhanden")
	}
	if vals["server"] == "ok" {
		add("Verbindung Rechner → Server", nil, host+":"+port+" erreichbar")
	} else {
		add("Verbindung Rechner → Server", fmt.Errorf("%s erreicht %s:%s nicht (%s) – Firewall auf dem Server bzw. im Netz prüfen", target, host, port, vals["server"]), "")
	}
}

func (s *Server) checkSSH(target string, c *cred, add func(string, error, string)) {
	sc, err := s.sshDial(target, c)
	if err != nil {
		add("Anmeldung (SSH)", err, "")
		return
	}
	defer sc.cl.Close()
	add("Anmeldung (SSH)", nil, "angemeldet als "+c.User)
	archOut, _ := sc.run("uname -m", nil)
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[archOut]
	if _, ok := agentBinaryFor("linux", arch); !ok {
		add("Agent-Programm", fmt.Errorf("für %s (linux-%s) nicht auf dem Server", archOut, arch), "")
	} else {
		add("Agent-Programm", nil, "linux-"+arch)
	}
	if out, err := sc.sudo(c, "id -u"); err != nil || strings.TrimSpace(lastLine(out)) != "0" {
		add("Root-Rechte", fmt.Errorf("sudo nicht möglich (%v %s) – Konto in die sudo-Gruppe aufnehmen bzw. bei Schlüsseln NOPASSWD", err, firstLine(out)), "")
	} else {
		add("Root-Rechte", nil, "über "+map[bool]string{true: "root", false: "sudo"}[c.User == "root"])
	}
	host, port := s.serverHostPort()
	test := fmt.Sprintf(`if command -v curl >/dev/null 2>&1; then curl -sk -o /dev/null --max-time 5 https://%[1]s:%[2]s/ && echo ok || echo fail;
elif command -v bash >/dev/null 2>&1; then timeout 5 bash -c 'exec 3<>/dev/tcp/%[1]s/%[2]s' && echo ok || echo fail; else echo unknown; fi`, host, port)
	out, _ := sc.run(test, nil)
	switch lastLine(out) {
	case "ok":
		add("Verbindung Rechner → Server", nil, host+":"+port+" erreichbar")
	case "unknown":
		add("Verbindung Rechner → Server", nil, "nicht prüfbar (weder curl noch bash vorhanden)")
	default:
		add("Verbindung Rechner → Server", fmt.Errorf("%s erreicht %s:%s nicht – Firewall auf dem Server bzw. im Netz prüfen", target, host, port), "")
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
