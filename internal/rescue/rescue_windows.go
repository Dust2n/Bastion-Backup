//go:build windows

package rescue

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"bastion/internal/fsx"
	"bastion/internal/layout"
	"bastion/internal/proto"
	"bastion/internal/repo"
)

func configPaths() []string {
	self, _ := os.Executable()
	return []string{filepath.Join(filepath.Dir(self), "rescue.json"), `X:\bastion\rescue.json`}
}

func prepareEnvironment() {
	// network is initialised by wpeinit in startnet.cmd
}

func reboot() {
	exec.Command("wpeutil", "reboot").Run()
	os.Exit(0)
}

func firmware() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if v, _, err := k.GetIntegerValue("PEFirmwareType"); err == nil && v == 2 {
			return "uefi"
		}
	}
	if strings.EqualFold(os.Getenv("firmware_type"), "UEFI") {
		return "uefi"
	}
	return "bios"
}

func findTicket() (*proto.RestoreTicket, error) {
	for l := 'C'; l <= 'Z'; l++ {
		if l == 'X' {
			continue
		}
		p := fmt.Sprintf(`%c:\BastionRecovery\ticket.json`, l)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var t proto.RestoreTicket
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, err
		}
		return &t, nil
	}
	return nil, fmt.Errorf("kein Ticket")
}

func usedLetters() map[byte]bool {
	used := map[byte]bool{'X': true, 'A': true, 'B': true}
	for l := byte('C'); l <= 'Z'; l++ {
		if _, err := os.Stat(string(l) + `:\`); err == nil {
			used[l] = true
		}
	}
	return used
}

type winPart struct {
	plan   layout.Planned
	letter byte
}

var lastParts []winPart

func diskpart(script string) error {
	f, err := os.CreateTemp("", "bastion-dp-*.txt")
	if err != nil {
		return err
	}
	f.WriteString(script)
	f.Close()
	defer os.Remove(f.Name())
	cmd := exec.Command("diskpart.exe", "/s", f.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("diskpart: %v\n%s", err, out)
	}
	return nil
}

func partition(t *TargetDisk, src layout.Disk, plan []layout.Planned, osName string) (Mounted, error) {
	if src.Style == "mbr" && len(plan) > 4 {
		return nil, fmt.Errorf("MBR erlaubt höchstens 4 primäre Partitionen, Plan hat %d", len(plan))
	}
	used := usedLetters()
	next := func(pref byte) byte {
		if !used[pref] {
			used[pref] = true
			return pref
		}
		for l := byte('Z'); l >= 'D'; l-- {
			if !used[l] {
				used[l] = true
				return l
			}
		}
		return 0
	}
	hasBoot := false
	for _, p := range plan {
		if p.Source.Role == layout.RoleBoot {
			hasBoot = true
		}
	}

	// Step 1: wipe and initialise. On some Windows versions "convert gpt"
	// already creates an MSR partition, which must not be created twice.
	style := "mbr"
	if src.Style == "gpt" {
		style = "gpt"
	}
	if err := diskpart(fmt.Sprintf("select disk %d\nonline disk noerr\nattributes disk clear readonly noerr\nclean\nconvert %s\nexit\n", t.Number, style)); err != nil {
		return nil, err
	}
	autoMSR := style == "gpt" && hasMSR(t.Number)

	// Step 2: create the planned partitions. The last one takes all
	// remaining space, which absorbs alignment and rounding differences.
	var b strings.Builder
	fmt.Fprintf(&b, "select disk %d\n", t.Number)
	lastParts = nil
	mounted := Mounted{}
	for i, p := range plan {
		mb := p.Size / layout.MiB
		size := fmt.Sprintf(" size=%d", mb)
		if i == len(plan)-1 {
			size = ""
		}
		wp := winPart{plan: p}
		label := strings.ReplaceAll(p.Source.Label, `"`, "")
		switch p.Source.Role {
		case layout.RoleESP:
			wp.letter = next('S')
			fmt.Fprintf(&b, "create partition efi size=%d\nformat quick fs=fat32 label=\"System\"\nassign letter=%c\n", mb, wp.letter)
		case layout.RoleMSR:
			if !autoMSR {
				fmt.Fprintf(&b, "create partition msr size=%d\n", mb)
			}
		case layout.RoleBoot:
			wp.letter = next('S')
			if label == "" {
				label = "System-reserviert"
			}
			fmt.Fprintf(&b, "create partition primary%s\nformat quick fs=ntfs label=\"%s\"\nactive\nassign letter=%c\n", size, label, wp.letter)
		default:
			pref := byte('W')
			if p.Source.Role != layout.RoleSystem {
				pref = 'R'
			}
			wp.letter = next(pref)
			fs := p.Source.FS
			if fs == "" {
				fs = "ntfs"
			}
			fmt.Fprintf(&b, "create partition primary%s\nformat quick fs=%s label=\"%s\"\n", size, fs, label)
			if src.Style == "mbr" && !hasBoot && p.Source.Role == layout.RoleSystem {
				b.WriteString("active\n")
			}
			if src.Style == "gpt" && p.Source.Role == layout.RoleRecovery {
				b.WriteString("set id=\"de94bba4-06d1-4d40-a16a-bfd50179d6ac\"\ngpt attributes=0x8000000000000001\n")
			}
			fmt.Fprintf(&b, "assign letter=%c\n", wp.letter)
			if p.Source.Volume != "" {
				mounted[p.Source.Volume] = string(wp.letter) + `:\`
			}
		}
		lastParts = append(lastParts, wp)
	}
	b.WriteString("exit\n")
	if err := diskpart(b.String()); err != nil {
		return nil, err
	}
	return mounted, nil
}

func unmountAll(Mounted) {}

func makeBootable(t *TargetDisk, src layout.Disk, plan []layout.Planned, mounted Mounted, snap *repo.Snapshot) error {
	var sys, boot byte
	for _, wp := range lastParts {
		switch wp.plan.Source.Role {
		case layout.RoleSystem:
			sys = wp.letter
		case layout.RoleESP, layout.RoleBoot:
			boot = wp.letter
		}
	}
	if sys == 0 {
		return fmt.Errorf("keine Systempartition im Plan")
	}
	if boot == 0 {
		boot = sys
	}
	fw := "BIOS"
	if src.Style == "gpt" {
		fw = "UEFI"
	}
	// Test hook: when restoring onto a virtual disk from a running Windows,
	// bcdboot would also change that machine's UEFI boot entries.
	if os.Getenv("BASTION_RESCUE_SKIP_BCDBOOT") != "1" {
		cmd := exec.Command("bcdboot.exe", fmt.Sprintf(`%c:\Windows`, sys), "/s", fmt.Sprintf("%c:", boot), "/f", fw)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("bcdboot: %v: %s", err, out)
		}
	} else {
		say("bcdboot übersprungen (BASTION_RESCUE_SKIP_BCDBOOT=1)")
	}

	// Point the drive letters in the restored registry at the new
	// partitions, so C: stays C: (and D: stays D:).
	letters := map[byte]string{}
	for _, wp := range lastParts {
		v := wp.plan.Source.Volume
		if len(v) == 2 && v[1] == ':' && wp.letter != 0 {
			letters[wp.letter] = strings.ToUpper(v)
		}
	}
	if err := FixDriveLetters(t.Number, fmt.Sprintf(`%c:\Windows\System32\config\SYSTEM`, sys), letters); err != nil {
		// Not fatal: Windows usually assigns C: to the boot volume anyway.
		say("Warnung: Laufwerksbuchstaben konnten nicht übernommen werden (%v). Falls Windows nach dem Start nicht C: ist, MountedDevices prüfen.", err)
	}
	return nil
}

var (
	modadvapi          = windows.NewLazySystemDLL("advapi32.dll")
	procRegLoadAppKeyW = modadvapi.NewProc("RegLoadAppKeyW")
	procRegLoadKeyW    = modadvapi.NewProc("RegLoadKeyW")
	procRegUnLoadKeyW  = modadvapi.NewProc("RegUnLoadKeyW")
)

// loadHive mounts an offline hive. RegLoadKey replays the hive's
// transaction logs (hives from a live snapshot are crash-consistent);
// RegLoadAppKey is the fallback where HKLM can not be used.
func loadHive(path string) (registry.Key, func(), error) {
	fsx.EnablePrivileges()
	const mount = "BastionOffline"
	hklm := uintptr(registry.LOCAL_MACHINE)
	sub := windows.StringToUTF16Ptr(mount)
	r, _, _ := procRegLoadKeyW.Call(hklm, uintptr(unsafe.Pointer(sub)), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(path))))
	if r == 0 {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, mount, registry.ALL_ACCESS)
		if err == nil {
			return k, func() {
				k.Close()
				procRegUnLoadKeyW.Call(hklm, uintptr(unsafe.Pointer(sub)))
			}, nil
		}
		procRegUnLoadKeyW.Call(hklm, uintptr(unsafe.Pointer(sub)))
	}
	first := windows.Errno(r)
	var h windows.Handle
	r, _, _ = procRegLoadAppKeyW.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(path))), uintptr(unsafe.Pointer(&h)),
		uintptr(registry.ALL_ACCESS), 0, 0)
	if r != 0 {
		return 0, nil, fmt.Errorf("SYSTEM-Hive %s nicht ladbar: %v / %v", path, first, windows.Errno(r))
	}
	k := registry.Key(h)
	return k, func() { k.Close() }, nil
}

// FixDriveLetters rewrites MountedDevices in an offline SYSTEM hive:
// current letter on the target disk → original letter.
func FixDriveLetters(diskNumber int, hive string, letters map[byte]string) error {
	values := map[string][]byte{}
	for cur, orig := range letters {
		v, err := mountedDevicesValue(cur)
		if err != nil {
			say("  %c: %v", cur, err)
			continue
		}
		values[`\DosDevices\`+orig] = v
	}
	if len(values) == 0 {
		return fmt.Errorf("keine Partitionen mit Laufwerksbuchstaben auf Datenträger %d gefunden", diskNumber)
	}

	root, unload, err := loadHive(hive)
	if err != nil {
		return err
	}
	defer unload()
	md, _, err := registry.CreateKey(root, "MountedDevices", registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer md.Close()
	names, _ := md.ReadValueNames(-1)
	for _, n := range names {
		if strings.HasPrefix(n, `\DosDevices\`) {
			md.DeleteValue(n)
		}
	}
	for n, v := range values {
		if err := md.SetBinaryValue(n, v); err != nil {
			return err
		}
		say("  %s → neue Partition", n)
	}
	return nil
}
