//go:build windows

package main

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func serviceMode() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

func installService(data, listen string, pxe bool, plainHTTP string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	args := []string{"-data", data, "-listen", listen}
	if pxe {
		args = append(args, "-pxe")
	}
	if plainHTTP != "" {
		args = append(args, "-http", plainHTTP)
	}
	s, err := m.OpenService("BastionServer")
	if err != nil {
		s, err = m.CreateService("BastionServer", exe, mgr.Config{
			DisplayName: "Bastion Backup Server",
			Description: "Zentrale Verwaltung der Bastion-Backups",
			StartType:   mgr.StartAutomatic,
		}, args...)
		if err != nil {
			return err
		}
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 10 * time.Second}}, 86400)
	return s.Start()
}

// installedService returns program and data directory of the installed
// service, taken from its command line.
func installedService() (exe, data string, ok bool) {
	m, err := mgr.Connect()
	if err != nil {
		return "", "", false
	}
	defer m.Disconnect()
	s, err := m.OpenService("BastionServer")
	if err != nil {
		return "", "", false
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return "", "", true
	}
	args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	if err != nil || len(args) == 0 {
		return "", "", true
	}
	exe = args[0]
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "-data" || args[i] == "--data" {
			data = args[i+1]
		}
	}
	return exe, data, true
}

// restartService stops the service, waits until its process is gone and
// starts it again.
func restartService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService("BastionServer")
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			if st, err = s.Query(); err != nil {
				return err
			}
			if st.State == svc.Stopped {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("Dienst ließ sich nicht innerhalb von 60 Sekunden beenden")
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err := s.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if st, err = s.Query(); err != nil {
			return err
		}
		if st.State == svc.Running {
			return nil
		}
		if st.State == svc.Stopped {
			return errors.New("Dienst hat sich nach dem Start wieder beendet (siehe server.log)")
		}
		if time.Now().After(deadline) {
			return errors.New("Dienst ist nach 60 Sekunden nicht gestartet")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// packageManaged: there are no system packages on Windows.
func packageManaged(string) bool { return false }
