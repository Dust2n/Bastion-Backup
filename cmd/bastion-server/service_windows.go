//go:build windows

package main

import (
	"os"
	"time"

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
