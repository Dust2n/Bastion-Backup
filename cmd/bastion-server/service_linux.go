//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func serviceMode() bool { return false }

func installService(data, listen string, pxe bool, plainHTTP string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{exe, "-data", data, "-listen", listen}
	if pxe {
		args = append(args, "-pxe")
	}
	if plainHTTP != "" {
		args = append(args, "-http", plainHTTP)
	}
	unit := fmt.Sprintf(`[Unit]
Description=Bastion Backup Server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, strings.Join(args, " "))
	if err := os.WriteFile("/etc/systemd/system/bastion-server.service", []byte(unit), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "enable", "--now", "bastion-server").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
