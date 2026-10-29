//go:build linux

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const ServiceName = "bastion-agent"

func exeName() string    { return "bastion-agent" }
func InstallDir() string { return "/opt/bastion" }
func StateDir() string {
	if h := os.Getenv("BASTION_HOME"); h != "" {
		return h
	}
	return "/var/lib/bastion"
}

func ConfigPath() string {
	if h := os.Getenv("BASTION_HOME"); h != "" {
		return h + "/agent.json"
	}
	return "/etc/bastion/agent.json"
}
func secureDir(dir string) { os.Chmod(dir, 0o700) }
func IsService() bool      { return false }

func lowPriority(on bool) {
	if on {
		syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
	} else {
		syscall.Setpriority(syscall.PRIO_PROCESS, 0, 0)
	}
}

// RunService just runs fn: systemd manages the process.
func RunService(_ string, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

const unit = `[Unit]
Description=Bastion Backup Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s run
Restart=always
RestartSec=5
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
`

func InstallService(exe string) error {
	if err := os.WriteFile("/etc/systemd/system/bastion-agent.service", []byte(fmt.Sprintf(unit, exe)), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "enable", "bastion-agent").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if out, err := exec.Command("systemctl", "restart", "bastion-agent").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// refreshProgramEntry: Linux has no central program list.
func refreshProgramEntry() {}

func StopService() { exec.Command("systemctl", "stop", "bastion-agent").Run() }

func Uninstall(fromService bool) error {
	exec.Command("systemctl", "disable", "bastion-agent").Run()
	os.Remove("/etc/systemd/system/bastion-agent.service")
	os.RemoveAll("/etc/bastion")
	os.RemoveAll(StateDir())
	os.RemoveAll(InstallDir())
	StopService() // when called from the service this ends the process
	exec.Command("systemctl", "daemon-reload").Run()
	return nil
}
