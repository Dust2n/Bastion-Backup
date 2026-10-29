package rescue

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"bastion/internal/client"
	"bastion/internal/layout"
	"bastion/internal/proto"
	"bastion/internal/tui"
)

// netIf is a network adapter in the rescue environment.
type netIf struct {
	Index int
	Name  string
	MAC   string
	Up    bool
	IPs   []string // IPv4 with prefix, e.g. 192.168.1.20/24
}

func interfaces() []netIf {
	ifs, _ := net.Interfaces()
	var out []netIf
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) != 6 {
			continue
		}
		n := netIf{Index: i.Index, Name: i.Name, MAC: i.HardwareAddr.String(), Up: i.Flags&net.FlagUp != 0}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				ones, _ := ipn.Mask.Size()
				n.IPs = append(n.IPs, fmt.Sprintf("%s/%d", ipn.IP, ones))
			}
		}
		out = append(out, n)
	}
	return out
}

func hasIPv4() bool {
	for _, i := range interfaces() {
		if len(i.IPs) > 0 {
			return true
		}
	}
	return false
}

func ipSummary() string {
	var ips []string
	for _, i := range interfaces() {
		ips = append(ips, i.IPs...)
	}
	if len(ips) == 0 {
		return tui.Bad("keine IP-Adresse")
	}
	return strings.Join(ips, ", ")
}

// waitForNetwork waits up to d for an IPv4 address (DHCP).
func waitForNetwork(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if hasIPv4() {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return hasIPv4()
}

// parseIPv4 accepts "192.168.1.20/24", "192.168.1.20 255.255.255.0" or
// "192.168.1.20" (then /24).
func parseIPv4(s string) (net.IP, int, error) {
	s = strings.TrimSpace(s)
	prefix := 24
	if i := strings.IndexAny(s, "/ "); i > 0 {
		rest := strings.TrimSpace(s[i+1:])
		s = s[:i]
		if m := net.ParseIP(rest).To4(); m != nil {
			prefix, _ = net.IPMask(m).Size()
		} else if p, err := strconv.Atoi(rest); err == nil && p >= 1 && p <= 32 {
			prefix = p
		} else {
			return nil, 0, fmt.Errorf("Präfix oder Netzmaske %q ungültig", rest)
		}
	}
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return nil, 0, fmt.Errorf("IP-Adresse %q ungültig", s)
	}
	return ip, prefix, nil
}

// autoNetwork sets up the network for a restore started from the running
// system: DHCP if available, otherwise the addresses the machine had
// (stored in the ticket). Adapters that were static get their address at
// once instead of waiting for DHCP.
func (s *session) autoNetwork(saved []proto.NetConfig) {
	if hasIPv4() {
		return
	}
	list := interfaces()
	byMAC := map[string]proto.NetConfig{}
	for _, c := range saved {
		byMAC[strings.ToLower(c.MAC)] = c
	}
	// a single adapter with a single saved setup belongs together even if
	// the MAC differs (e.g. another NIC driver in WinPE)
	match := func(i netIf) (proto.NetConfig, bool) {
		if c, ok := byMAC[strings.ToLower(i.MAC)]; ok {
			return c, true
		}
		if len(list) == 1 && len(saved) == 1 {
			return saved[0], true
		}
		return proto.NetConfig{}, false
	}
	apply := func(onlyStatic bool) int {
		n := 0
		for _, i := range list {
			c, ok := match(i)
			if !ok || len(c.IPs) == 0 || (onlyStatic && c.DHCP) {
				continue
			}
			ip, prefix, err := parseIPv4(c.IPs[0])
			if err != nil {
				continue
			}
			gw := net.ParseIP(c.Gateway).To4()
			if err := setStatic(i, ip, prefix, gw, c.DNS); err != nil {
				say("Netzwerk %s: %v", i.Name, err)
				continue
			}
			say("Netzwerk %s: Adresse %s/%d (Gateway %s) vom bisherigen System übernommen", i.Name, ip, prefix, c.Gateway)
			n++
		}
		return n
	}

	p := s.ui.progress("Netzwerk wird eingerichtet")
	defer p.stop()
	if apply(true) > 0 && waitForNetwork(10*time.Second) {
		return
	}
	p.set(10, "Warte auf eine IP-Adresse per DHCP (bis zu 25 Sekunden) …")
	if waitForNetwork(25 * time.Second) {
		return
	}
	if len(saved) > 0 {
		p.set(60, "Kein DHCP – übernehme die Adresse des bisherigen Systems …")
		if apply(false) > 0 {
			waitForNetwork(10 * time.Second)
		}
	}
}

// unreachable is shown when a restore from the running system cannot reach
// the server. Nothing has been changed on the disk yet.
func (s *session) unreachable() (retry bool) {
	for {
		switch s.ui.menu("Server nicht erreichbar", []string{
			"Der Bastion-Server " + s.ticket.Server + " ist nicht erreichbar.",
			"Netz: " + ipSummary(),
			"", "Die Festplatte wurde nicht verändert."}, []tui.Item{
			{Label: "Netzwerk einstellen", Desc: "DHCP oder statische IP-Adresse"},
			{Label: "Erneut verbinden"},
			{Label: "Neustart in das bisherige System", Desc: "Wiederherstellung abbrechen"},
		}) {
		case 0:
			s.networkMenu()
		case 1:
			return true
		case 2, -1:
			return false
		}
	}
}

// ---------- server status (checked in the background) ----------

type serverState struct {
	mu     sync.Mutex
	cfg    Config
	status string
}

func (s *session) startStatusChecker() {
	go func() {
		for {
			s.srv.mu.Lock()
			cfg := s.srv.cfg
			s.srv.mu.Unlock()
			st := serverStatus(cfg)
			s.srv.mu.Lock()
			s.srv.status = st
			s.srv.mu.Unlock()
			time.Sleep(10 * time.Second)
		}
	}()
}

func serverStatus(cfg Config) string {
	if cfg.Server == "" {
		return tui.Warn("keine Server-Adresse")
	}
	c := client.New(cfg.Server, cfg.Fingerprint, "")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	resp, err := c.Do(ctx, "GET", "/", nil, "")
	if err != nil {
		return tui.Bad("nicht erreichbar")
	}
	resp.Body.Close()
	return tui.OK("erreichbar")
}

// statusBar is shown at the bottom of the full-screen UI.
func (s *session) statusBar() (string, string) {
	s.srv.mu.Lock()
	st, server := s.srv.status, s.srv.cfg.Server
	s.srv.mu.Unlock()
	if st == "" {
		st = "wird geprüft"
	}
	left := fmt.Sprintf("Netz: %s   Server: %s %s", ipSummary(), strings.TrimPrefix(server, "https://"), st)
	right := fmt.Sprintf("Tastatur: %s   %s", keyboardName(), time.Now().Format("15:04"))
	return left, right
}

// ---------- menus ----------

// menu is shown when no restore ticket was found on the disks. It returns
// the restore ticket for the chosen backup.
func (s *session) menu() *proto.RestoreTicket {
	s.srv.cfg = loadConfig()
	s.startStatusChecker()
	if !hasIPv4() {
		p := s.ui.progress("Netzwerk wird eingerichtet")
		p.set(0, "Warte auf eine IP-Adresse per DHCP (bis zu 25 Sekunden) …")
		ok := waitForNetwork(25 * time.Second)
		p.stop()
		if !ok {
			s.ui.message("Kein DHCP", []string{"Es wurde keine IP-Adresse per DHCP zugewiesen.", "Bitte die Netzwerkkarte statisch einrichten."}, true, 0)
			s.networkMenu()
		}
	}
	for {
		lines := []string{"Wie soll das Backup ausgewählt werden?"}
		if l := lastLog(); l != "" {
			lines = append(lines, "", "Zuletzt: "+l)
		}
		items := []tui.Item{
			{Label: "Backup auswählen", Desc: "Mit einem Bastion-Administrator anmelden, Rechner und Backup wählen"},
			{Label: "Wiederherstellungs-Code eingeben", Desc: "Code aus der Weboberfläche (Image wiederherstellen → USB/ISO/PXE)"},
			{Label: "Netzwerk einstellen", Desc: "DHCP oder statische IP-Adresse"},
			{Label: "Server-Adresse ändern", Desc: "Aktuell: " + s.srv.cfg.Server},
			{Label: "Tastatur umschalten", Desc: "Deutsch ↔ Englisch (US), aktuell " + keyboardName()},
			{Label: "Neustart", Desc: "Rechner neu starten"},
			{Label: "Beenden", Desc: "Eingabeaufforderung für Diagnose"},
		}
		var t *proto.RestoreTicket
		var err error
		switch s.ui.menu("Bastion Wiederherstellung", lines, items) {
		case 0:
			t, err = s.chooseBackup()
		case 1:
			t, err = s.ticketFromCode()
		case 2:
			s.networkMenu()
		case 3:
			if v, ok := s.ui.input("Server-Adresse", []string{"Adresse des Bastion-Servers, z. B. https://172.16.50.10:8443"}, "Adresse:", s.srv.cfg.Server); ok {
				s.srv.mu.Lock()
				s.srv.cfg.Server = strings.TrimRight(v, "/")
				s.srv.mu.Unlock()
				if fp, ok := s.ui.input("Zertifikat", []string{"SHA-256-Fingerabdruck des Server-Zertifikats", "(Enter = unverändert lassen)"}, "Fingerabdruck:", s.srv.cfg.Fingerprint); ok {
					s.srv.mu.Lock()
					s.srv.cfg.Fingerprint = fp
					s.srv.mu.Unlock()
				}
				s.srv.mu.Lock()
				s.srv.status = ""
				s.srv.mu.Unlock()
				go func() { st := serverStatus(s.srv.cfg); s.srv.mu.Lock(); s.srv.status = st; s.srv.mu.Unlock() }()
			}
		case 4:
			if keyboardName() == "DE" {
				setKeyboard("us")
			} else {
				setKeyboard("de")
			}
		case 5:
			s.ui.close()
			reboot()
		case 6, -1:
			if s.ui.confirm("Beenden", []string{"Das Wiederherstellungsprogramm wird beendet.", "Neustart später mit: wpeutil reboot"}, "JA") {
				return nil
			}
		}
		if err != nil {
			s.ui.message("Fehler", []string{err.Error()}, true, 0)
		}
		if t != nil {
			return t
		}
	}
}

func (s *session) networkMenu() {
	list := interfaces()
	if len(list) == 0 {
		s.ui.message("Netzwerk", []string{"Keine Netzwerkkarte gefunden.", "Wahrscheinlich fehlt ein Treiber (bei VMs z. B. Intel e1000 statt virtio wählen)."}, true, 0)
		return
	}
	rows := make([][]string, len(list))
	for i, n := range list {
		ip := "keine Adresse"
		if len(n.IPs) > 0 {
			ip = strings.Join(n.IPs, ", ")
		}
		rows[i] = []string{n.Name, n.MAC, ip}
	}
	idx := 0
	if len(list) > 1 {
		if idx = s.ui.selectRow("Netzwerkkarte wählen", nil, []string{"Name", "MAC", "Adresse"}, rows, []int{26, 17, 22}); idx < 0 {
			return
		}
	}
	ifc := list[idx]
	switch s.ui.menu("Netzwerk: "+ifc.Name, []string{"MAC " + ifc.MAC + " · " + rows[idx][2]}, []tui.Item{
		{Label: "Automatisch (DHCP)"}, {Label: "Statische Adresse"}, {Label: "Zurück"}}) {
	case 0:
		p := s.ui.progress("Netzwerk")
		p.set(10, "Fordere Adresse per DHCP an …")
		if err := setDHCP(ifc); err != nil {
			say("Fehler: %v", err)
		}
		ok := waitForNetwork(30 * time.Second)
		p.stop()
		if ok {
			s.ui.message("Netzwerk bereit", []string{"Adresse: " + ipSummary()}, false, 3*time.Second)
		} else {
			s.ui.message("Kein DHCP", []string{"Keine Adresse per DHCP erhalten.", "Bitte eine statische Adresse eintragen."}, true, 0)
		}
	case 1:
		v, ok := s.ui.input("Statische Adresse", []string{"Format: 192.168.1.50/24 oder 192.168.1.50 255.255.255.0"}, "IP-Adresse:", "")
		if !ok {
			return
		}
		ip, prefix, err := parseIPv4(v)
		if err != nil {
			s.ui.message("Eingabe ungültig", []string{err.Error()}, true, 0)
			return
		}
		var gw net.IP
		if v, _ := s.ui.input("Statische Adresse", []string{fmt.Sprintf("Adresse %s/%d", ip, prefix)}, "Gateway (leer = keins):", ""); v != "" {
			if gw = net.ParseIP(v).To4(); gw == nil {
				s.ui.message("Eingabe ungültig", []string{"Gateway " + v + " ist keine IPv4-Adresse"}, true, 0)
				return
			}
		}
		var dns []string
		v, _ = s.ui.input("Statische Adresse", []string{"Mehrere Server mit Komma trennen"}, "DNS-Server (leer = keiner):", "")
		for _, d := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			if net.ParseIP(d).To4() != nil {
				dns = append(dns, d)
			}
		}
		if err := setStatic(ifc, ip, prefix, gw, dns); err != nil {
			s.ui.message("Fehler", []string{err.Error()}, true, 0)
			return
		}
		time.Sleep(3 * time.Second)
		s.ui.message("Adresse gesetzt", []string{fmt.Sprintf("%s/%d auf %s", ip, prefix, ifc.Name), "Aktuell: " + ipSummary()}, false, 3*time.Second)
	}
}

func (s *session) ticketFromCode() (*proto.RestoreTicket, error) {
	cfg := s.cfg()
	if cfg.Server == "" {
		return nil, fmt.Errorf("zuerst die Server-Adresse eintragen")
	}
	code, ok := s.ui.input("Wiederherstellungs-Code", []string{"Den 8-stelligen Code aus der Weboberfläche eingeben."}, "Code:", "")
	if !ok {
		return nil, nil
	}
	return redeemCode(client.New(cfg.Server, cfg.Fingerprint, ""), cfg, strings.ToUpper(strings.ReplaceAll(code, " ", "")))
}

func (s *session) cfg() Config {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	return s.srv.cfg
}

func redeemCode(c *client.Client, cfg Config, code string) (*proto.RestoreTicket, error) {
	var info proto.RestoreInfo
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.JSON(ctx, "POST", "/api/rescue/code", map[string]string{"code": code}, &info); err != nil {
		return nil, fmt.Errorf("Code nicht akzeptiert: %v", err)
	}
	return &proto.RestoreTicket{Server: cfg.Server, Fingerprint: cfg.Fingerprint, Token: info.Token,
		JobID: info.JobID, SnapshotID: info.Snapshot.ID, DiskNumber: -1}, nil
}

// chooseBackup logs in with a Bastion administrator, lets the user pick a
// machine and one of its image backups and creates the restore job.
func (s *session) chooseBackup() (*proto.RestoreTicket, error) {
	cfg := s.cfg()
	if cfg.Server == "" {
		return nil, fmt.Errorf("zuerst die Server-Adresse eintragen")
	}
	name, ok := s.ui.input("Anmeldung", []string{"Anmelden mit einem Bastion-Benutzer (Rolle Administrator)."}, "Benutzer:", "admin")
	if !ok {
		return nil, nil
	}
	pw, ok := s.ui.password("Anmeldung", "Passwort:")
	if !ok {
		return nil, nil
	}
	c := client.New(cfg.Server, cfg.Fingerprint, "")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var login struct{ Token string }
	if _, err := c.JSON(ctx, "POST", "/api/rescue/login", map[string]string{"name": name, "password": pw}, &login); err != nil {
		return nil, fmt.Errorf("Anmeldung fehlgeschlagen: %v", err)
	}
	c.Auth = "session:" + login.Token

	var agents []struct {
		ID         string `json:"id"`
		Hostname   string `json:"hostname"`
		OS         string `json:"os"`
		OSVersion  string `json:"os_version"`
		LastBackup int64  `json:"last_backup"`
	}
	if _, err := c.JSON(ctx, "GET", "/api/agents", nil, &agents); err != nil {
		return nil, err
	}
	shown := agents[:0]
	hidden := 0
	for _, a := range agents {
		if a.OS == runtime.GOOS {
			shown = append(shown, a)
		} else {
			hidden++
		}
	}
	if len(shown) == 0 {
		return nil, fmt.Errorf("keine Rechner mit %s-Backups vorhanden", runtime.GOOS)
	}
	for {
		rows := make([][]string, len(shown))
		for i, a := range shown {
			last := "noch kein Backup"
			if a.LastBackup > 0 {
				last = time.Unix(a.LastBackup, 0).Format("02.01.2006 15:04")
			}
			rows[i] = []string{a.Hostname, a.OSVersion, last}
		}
		var info []string
		if hidden > 0 {
			info = []string{fmt.Sprintf("%d Rechner mit anderem Betriebssystem sind ausgeblendet.", hidden)}
		}
		n := s.ui.selectRow("Rechner wählen", info, []string{"Rechner", "System", "Letztes Backup"}, rows, []int{20, 40, 16})
		if n < 0 {
			return nil, nil
		}
		agent := shown[n]

		var snaps []struct {
			ID       string `json:"id"`
			Time     int64  `json:"time"`
			Kind     string `json:"kind"`
			PlanName string `json:"plan_name"`
			Files    int64  `json:"files"`
			Bytes    int64  `json:"bytes"`
		}
		if _, err := c.JSON(ctx, "GET", "/api/snapshots?agent="+agent.ID, nil, &snaps); err != nil {
			return nil, err
		}
		images := snaps[:0]
		for _, sn := range snaps {
			if sn.Kind == "image" {
				images = append(images, sn)
			}
		}
		if len(images) == 0 {
			s.ui.message("Keine Image-Backups", []string{agent.Hostname + " hat noch kein Image-Backup."}, true, 0)
			continue
		}
		rows = make([][]string, len(images))
		for i, sn := range images {
			plan := sn.PlanName
			if plan == "" {
				plan = "manuell"
			}
			rows[i] = []string{time.Unix(sn.Time, 0).Format("02.01.2006 15:04"), layout.Human(sn.Bytes), fmt.Sprintf("%d", sn.Files), plan}
		}
		n = s.ui.selectRow("Backup von "+agent.Hostname, []string{"Das neueste Backup steht oben."}, []string{"Zeitpunkt", "Größe", "Dateien", "Plan"}, rows, []int{16, 11, 9, 24})
		if n < 0 {
			continue
		}
		snap := images[n]
		var job struct {
			JobID int64  `json:"job_id"`
			Code  string `json:"code"`
		}
		if _, err := c.JSON(ctx, "POST", "/api/snapshots/"+snap.ID+"/restore-image",
			map[string]any{"agent_id": agent.ID, "mode": "media"}, &job); err != nil {
			return nil, fmt.Errorf("Auftrag anlegen: %v", err)
		}
		say("Auftrag %d angelegt: %s vom %s", job.JobID, agent.Hostname, time.Unix(snap.Time, 0).Format("02.01.2006 15:04"))
		return redeemCode(client.New(cfg.Server, cfg.Fingerprint, ""), cfg, job.Code)
	}
}
