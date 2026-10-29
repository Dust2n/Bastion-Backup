//go:build windows

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const smbPushSupported = true

// smbConnect logs on to IPC$ and ADMIN$ of target. It logs on to IPC$
// first, so a wrong password and missing admin rights on the share can be
// told apart. close disconnects both.
func smbConnect(target string, c *cred) (share string, close func(), err error) {
	user := c.User
	if strings.HasPrefix(user, `.\`) {
		// ".\" means "local account" – of the target, not of this server
		user = strings.SplitN(target, ".", 2)[0] + user[1:]
	}
	ipc := `\\` + target + `\IPC$`
	if err := wnetConnect(ipc, user, c.Secret); err != nil {
		switch err {
		case windows.ERROR_LOGON_FAILURE, windows.Errno(1327), windows.Errno(1330), windows.Errno(1331):
			return "", nil, fmt.Errorf("Anmeldung an %s fehlgeschlagen: Benutzername oder Passwort falsch bzw. Konto gesperrt (%v)", target, err)
		case windows.Errno(53), windows.Errno(67), windows.Errno(1231), windows.Errno(64), windows.Errno(121):
			return "", nil, unreachable("%s ist per SMB nicht erreichbar (%v) – Rechner aus oder Firewall: \"Datei- und Druckerfreigabe\" zulassen", target, err)
		default:
			return "", nil, fmt.Errorf("Anmeldung an %s fehlgeschlagen: %v", target, err)
		}
	}
	share = `\\` + target + `\ADMIN$`
	if err := wnetConnect(share, user, c.Secret); err != nil {
		wnetDisconnect(ipc)
		if err == windows.ERROR_ACCESS_DENIED {
			return "", nil, fmt.Errorf("Anmeldung an %s erfolgreich, aber kein Zugriff auf ADMIN$. Windows entzieht lokalen Konten (außer dem eingebauten \"Administrator\") bei Netzwerkzugriffen die Adminrechte (Remote-UAC). "+
				"Lösung: ein Domänenkonto verwenden, das eingebaute Administrator-Konto, oder auf %s einmalig als Administrator ausführen: "+
				"reg add HKLM\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Policies\\System /v LocalAccountTokenFilterPolicy /t REG_DWORD /d 1 /f", target, target)
		}
		return "", nil, fmt.Errorf("Zugriff auf %s fehlgeschlagen: %v (Datei- und Druckerfreigabe aktiv?)", share, err)
	}
	return share, func() { wnetDisconnect(share); wnetDisconnect(ipc) }, nil
}

func connectSCM(target string) (*mgr.Mgr, error) {
	m, err := mgr.ConnectRemote(target)
	if err != nil {
		return nil, fmt.Errorf("Dienststeuerung auf %s nicht erreichbar: %v (Remote-UAC: bei lokalen Konten LocalAccountTokenFilterPolicy=1 nötig)", target, err)
	}
	return m, nil
}

// checkSMB tests everything pushSMB needs without changing the target.
func (s *Server) checkSMB(target string, c *cred, add func(name string, err error, detail string)) bool {
	_, closeShare, err := smbConnect(target, c)
	add("Anmeldung und ADMIN$", err, "Anmeldung erfolgreich, Adminfreigabe beschreibbar")
	if err != nil {
		return false
	}
	defer closeShare()
	m, err := connectSCM(target)
	add("Dienststeuerung", err, "Dienste können angelegt werden")
	if err != nil {
		return false
	}
	m.Disconnect()
	return true
}

// pushSMB installs the agent like PsExec: copy it to ADMIN$, run it as a
// temporary service that installs the real agent service, remove it.
func (s *Server) pushSMB(ctx context.Context, target string, c *cred, tok string, progress func(string)) error {
	bin, ok := agentBinaryFor("windows", "amd64")
	if !ok {
		return errors.New("Agent-Programm fehlt in data/agents/windows-amd64/")
	}
	share, closeShare, err := smbConnect(target, c)
	if err != nil {
		return err
	}
	defer closeShare()

	progress("kopiere Agent nach ADMIN$")
	dst := share + `\Temp\bastion-agent-setup.exe`
	logPath := share + `\Temp\bastion-install.log`
	os.Remove(logPath)
	if err := copyLocal(s.path(bin.Path), dst); err != nil {
		return fmt.Errorf("Kopieren nach %s: %v", dst, err)
	}
	defer os.Remove(dst)

	m, err := connectSCM(target)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	const name = "BastionInstall"
	if old, err := m.OpenService(name); err == nil {
		old.Control(svc.Stop)
		old.Delete()
		old.Close()
		time.Sleep(2 * time.Second)
	}
	sv, err := m.CreateService(name, `C:\Windows\Temp\bastion-agent-setup.exe`, mgr.Config{
		DisplayName: "Bastion Agent Installation", StartType: mgr.StartManual,
	}, "svc-install", "--server", s.PublicURL(), "--token", tok, "--fingerprint", s.fp)
	if err != nil {
		return fmt.Errorf("Installationsdienst anlegen: %v", err)
	}
	defer func() {
		sv.Delete()
		sv.Close()
	}()
	progress("installiere")
	if err := sv.Start(); err != nil {
		return fmt.Errorf("Installationsdienst starten: %v", err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		st, err := sv.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	logText := ""
	if f, err := os.Open(logPath); err == nil {
		b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
		f.Close()
		logText = strings.TrimSpace(string(b))
		os.Remove(logPath)
	}
	if strings.Contains(logText, "FEHLER") {
		return fmt.Errorf("Installation auf %s: %s", target, firstLine(strings.TrimPrefix(logText[strings.Index(logText, "FEHLER"):], "FEHLER: ")))
	}
	progress("installiert, warte auf Anmeldung")
	return nil
}

func copyLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
