//go:build windows

package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"bastion/internal/proto"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const ServiceName = "BastionAgent"

func exeName() string { return "bastion-agent.exe" }

func InstallDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "Bastion")
}

func StateDir() string {
	if h := os.Getenv("BASTION_HOME"); h != "" {
		return h
	}
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "Bastion")
}

func ConfigPath() string { return filepath.Join(StateDir(), "agent.json") }

// secureDir limits access to SYSTEM and Administrators (by SID, so it works
// on every language version).
func secureDir(dir string) {
	for _, args := range [][]string{
		{dir, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/Q"},
		{filepath.Join(dir, "*"), "/reset", "/T", "/Q"}, // existing files inherit from the folder
	} {
		cmd := exec.Command("icacls", args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmd.Run()
	}
}

// lowPriority lowers the CPU priority while backing up, so users barely
// notice backups during work. (Background mode would also drop I/O to
// "very low", which starves backups on busy disks.)
func lowPriority(on bool) {
	if on {
		windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
	} else {
		windows.SetPriorityClass(windows.CurrentProcess(), windows.NORMAL_PRIORITY_CLASS)
	}
}

func IsService() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

type handler struct {
	run func(ctx context.Context) error
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
				}
				return false, 0
			}
		case err := <-done:
			cancel()
			if err != nil {
				return false, 1
			}
			return false, 0
		}
	}
}

// RunService runs fn as the Windows service name.
func RunService(name string, fn func(ctx context.Context) error) error {
	return svc.Run(name, &handler{run: fn})
}

func InstallService(exe string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err == nil {
		cfg, _ := s.Config()
		cfg.BinaryPathName = fmt.Sprintf(`"%s" run`, exe)
		cfg.StartType = mgr.StartAutomatic
		s.UpdateConfig(cfg)
	} else {
		s, err = m.CreateService(ServiceName, exe, mgr.Config{
			DisplayName:      "Bastion Backup Agent",
			Description:      "Sichert diesen Computer auf den zentralen Bastion-Server.",
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
		}, "run")
		if err != nil {
			return err
		}
	}
	defer s.Close()
	s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)
	s.SetRecoveryActionsOnNonCrashFailures(true)
	registerProgram(exe)
	if st, err := s.Query(); err == nil && st.State == svc.Running {
		return nil
	}
	return s.Start()
}

const uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\BastionAgent`

// registerProgram adds the agent to "Installed apps" / "Programs and
// Features", including an uninstall button. It runs at install time and
// at every start, so version and size stay current after updates.
func registerProgram(exe string) {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKey, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	server := ""
	if cfg, err := LoadConfig(); err == nil {
		server = cfg.Server
	}
	size := uint32(0)
	if st, err := os.Stat(exe); err == nil {
		size = uint32(st.Size() / 1024)
	}
	k.SetStringValue("DisplayName", "Bastion Backup Agent")
	k.SetStringValue("DisplayVersion", proto.Version)
	k.SetStringValue("Publisher", "Bastion Backup")
	k.SetStringValue("InstallLocation", filepath.Dir(exe))
	k.SetStringValue("DisplayIcon", exe)
	k.SetStringValue("UninstallString", fmt.Sprintf(`"%s" uninstall`, exe))
	k.SetStringValue("QuietUninstallString", fmt.Sprintf(`"%s" uninstall`, exe))
	k.SetStringValue("Comments", "Sichert diesen Computer auf den zentralen Bastion-Server.")
	if server != "" {
		k.SetStringValue("URLInfoAbout", server)
		k.SetStringValue("HelpLink", server)
	}
	k.SetDWordValue("EstimatedSize", size)
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	if v, _, err := k.GetStringValue("InstallDate"); err != nil || v == "" {
		k.SetStringValue("InstallDate", time.Now().Format("20060102"))
	}
}

// refreshProgramEntry updates the entry when running as the installed
// service (not for a one-off run from somewhere else).
func refreshProgramEntry() {
	exe, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Dir(exe), InstallDir()) {
		return
	}
	registerProgram(exe)
}

func unregisterProgram() {
	registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKey)
}

func StopService() {
	m, err := mgr.Connect()
	if err != nil {
		return
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return
	}
	defer s.Close()
	s.Control(svc.Stop)
	for i := 0; i < 30; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Uninstall removes service, program and configuration. When called from
// the running service, a detached helper does the work after we exit.
func Uninstall(fromService bool) error {
	unregisterProgram()
	if fromService {
		script := fmt.Sprintf(`ping -n 4 127.0.0.1 >nul & sc.exe stop %[1]s & ping -n 4 127.0.0.1 >nul & sc.exe delete %[1]s & rmdir /s /q "%[2]s" & rmdir /s /q "%[3]s"`,
			ServiceName, InstallDir(), StateDir())
		cmd := exec.Command("cmd.exe", "/c", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
		return cmd.Start()
	}
	StopService()
	m, err := mgr.Connect()
	if err == nil {
		if s, err := m.OpenService(ServiceName); err == nil {
			s.Delete()
			s.Close()
		}
		m.Disconnect()
	}
	os.RemoveAll(StateDir())
	self, _ := os.Executable()
	if filepath.Dir(self) == InstallDir() {
		// a running exe can not delete itself; remove the folder afterwards
		cmd := exec.Command("cmd.exe", "/c", fmt.Sprintf(`ping -n 3 127.0.0.1 >nul & rmdir /s /q "%s"`, InstallDir()))
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS}
		cmd.Start()
	} else {
		os.RemoveAll(InstallDir())
	}
	return nil
}
