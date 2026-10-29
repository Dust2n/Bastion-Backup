//go:build linux

package rescue

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"bastion/internal/layout"
	"bastion/internal/platform"
	"bastion/internal/proto"
	"bastion/internal/repo"
)

const targetRoot = "/mnt/bastion-target"

func configPaths() []string { return []string{"/etc/bastion/rescue.json"} }

func sh(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func prepareEnvironment() {
	sh("udevadm", "settle")
	sh("vgchange", "-an") // old volume groups must not keep the disk busy
}

// reboot restarts immediately: the rescue system has nothing to shut down.
func reboot() {
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	sh("reboot", "-f")
	os.Exit(0)
}

func firmware() string {
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		return "uefi"
	}
	return "bios"
}

type blk struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	FSType   string `json:"fstype"`
	Size     any    `json:"size"`
	Serial   string `json:"serial"`
	Model    string `json:"model"`
	Tran     string `json:"tran"`
	RM       any    `json:"rm"`
	Children []blk  `json:"children"`
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "1" || x == "true"
	case float64:
		return x == 1
	}
	return false
}

func lsblk() ([]blk, error) {
	out, err := sh("lsblk", "-J", "-b", "-o", "PATH,TYPE,FSTYPE,SIZE,SERIAL,MODEL,TRAN,RM")
	if err != nil {
		return nil, err
	}
	var r struct {
		Blockdevices []blk `json:"blockdevices"`
	}
	return r.Blockdevices, json.Unmarshal([]byte(out), &r)
}

func findTicket() (*proto.RestoreTicket, error) {
	devs, err := lsblk()
	if err != nil {
		return nil, err
	}
	dir := "/run/bastion-probe"
	os.MkdirAll(dir, 0o700)
	var walk func([]blk) *proto.RestoreTicket
	walk = func(bs []blk) *proto.RestoreTicket {
		for _, b := range bs {
			if b.FSType == "ext4" || b.FSType == "ext3" || b.FSType == "ext2" || b.FSType == "xfs" || b.FSType == "btrfs" || b.FSType == "vfat" {
				opts := "ro"
				if b.FSType == "xfs" {
					opts += ",norecovery"
				}
				if _, err := sh("mount", "-o", opts, b.Path, dir); err == nil {
					var t *proto.RestoreTicket
					for _, p := range []string{"bastion/ticket.json", "boot/bastion/ticket.json"} {
						if raw, err := os.ReadFile(filepath.Join(dir, p)); err == nil {
							var tt proto.RestoreTicket
							if json.Unmarshal(raw, &tt) == nil {
								t = &tt
							}
						}
					}
					sh("umount", dir)
					if t != nil {
						return t
					}
				}
			}
			if t := walk(b.Children); t != nil {
				return t
			}
		}
		return nil
	}
	sh("vgchange", "-ay")
	t := walk(devs)
	sh("vgchange", "-an")
	if t == nil {
		return nil, fmt.Errorf("kein Ticket")
	}
	return t, nil
}

func listDisks() ([]TargetDisk, error) {
	devs, err := lsblk()
	if err != nil {
		return nil, err
	}
	var out []TargetDisk
	for i, d := range devs {
		if d.Type != "disk" || d.Tran == "usb" || asBool(d.RM) || strings.HasPrefix(d.Path, "/dev/loop") || strings.HasPrefix(d.Path, "/dev/zram") {
			continue
		}
		out = append(out, TargetDisk{Number: i, Path: d.Path, Size: asInt(d.Size), Serial: d.Serial, Model: d.Model})
	}
	return out, nil
}

func partDev(disk string, n int) string {
	last := disk[len(disk)-1]
	if last >= '0' && last <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

func defaultType(p layout.Partition, gpt bool) string {
	if p.Type != "" && (gpt == strings.Contains(p.Type, "-")) {
		return p.Type
	}
	switch p.Role {
	case layout.RoleESP:
		if gpt {
			return "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
		}
		return "ef"
	case layout.RoleBIOSBoot:
		return "21686148-6449-6e6f-744e-656564454649"
	case layout.RoleSwap:
		if gpt {
			return "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f"
		}
		return "82"
	case layout.RoleLVM:
		if gpt {
			return "e6d6d379-f507-44c2-a23c-238f2a3df928"
		}
		return "8e"
	}
	if gpt {
		return "0fc63daf-8483-4772-8e79-3d69d8477de4"
	}
	return "83"
}

type mountEntry struct {
	dev, fs, mount, uuid, label string
}

var mountsDone []string

func partition(t *TargetDisk, src layout.Disk, plan []layout.Planned, osName string) (Mounted, error) {
	gpt := src.Style == "gpt"
	if !gpt && len(plan) > 4 {
		return nil, fmt.Errorf("MBR mit mehr als 4 Partitionen wird nicht unterstützt")
	}
	sh("wipefs", "-a", "-f", t.Path)
	var b strings.Builder
	if gpt {
		b.WriteString("label: gpt\n")
	} else {
		b.WriteString("label: dos\n")
	}
	start := int64(2048)
	for _, p := range plan {
		sectors := p.Size / 512
		line := fmt.Sprintf("start=%d, size=%d, type=%s", start, sectors, defaultType(p.Source, gpt))
		if !gpt && p.Source.Active {
			line += ", bootable"
		}
		b.WriteString(line + "\n")
		start += sectors
	}
	cmd := exec.Command("sfdisk", "--no-reread", "-f", t.Path)
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sfdisk: %v: %s", err, out)
	}
	sh("partprobe", t.Path)
	sh("udevadm", "settle")
	time.Sleep(2 * time.Second)

	var entries []mountEntry
	for i, p := range plan {
		dev := partDev(t.Path, i+1)
		switch p.Source.Role {
		case layout.RoleBIOSBoot:
			continue
		case layout.RoleLVM:
			lvs, err := layout.ScaleLVM(p.Source, p.Size)
			if err != nil {
				return nil, err
			}
			vg := p.Source.VG
			if _, err := sh("pvcreate", "-ff", "-y", dev); err != nil {
				return nil, err
			}
			if _, err := sh("vgcreate", "-y", vg, dev); err != nil {
				return nil, err
			}
			for _, lv := range lvs {
				if _, err := sh("lvcreate", "-y", "-W", "y", "-n", lv.Source.Label, "-L", fmt.Sprintf("%dm", lv.Size/layout.MiB), vg); err != nil {
					return nil, err
				}
				// for logical volumes Label holds the LV name, not a file system label
				entries = append(entries, mountEntry{dev: "/dev/" + vg + "/" + lv.Source.Label, fs: lv.Source.FS,
					mount: lv.Source.Volume, uuid: lv.Source.UUID})
			}
		default:
			entries = append(entries, mountEntry{dev: dev, fs: p.Source.FS, mount: p.Source.Volume, uuid: p.Source.UUID, label: p.Source.Label})
		}
	}
	for _, e := range entries {
		if e.fs == "" {
			continue
		}
		if err := platform.Mkfs(e.dev, e.fs, e.uuid, e.label); err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return len(entries[i].mount) < len(entries[j].mount) })
	mounted := Mounted{}
	os.MkdirAll(targetRoot, 0o755)
	for _, e := range entries {
		if e.mount == "" || e.fs == "swap" {
			continue
		}
		dir := filepath.Join(targetRoot, e.mount)
		os.MkdirAll(dir, 0o755)
		if _, err := sh("mount", e.dev, dir); err != nil {
			return nil, err
		}
		mountsDone = append(mountsDone, dir)
		mounted[e.mount] = dir
	}
	return mounted, nil
}

func unmountAll(Mounted) {
	for i := len(mountsDone) - 1; i >= 0; i-- {
		sh("umount", "-l", mountsDone[i])
	}
	mountsDone = nil
}

func chroot(args ...string) error {
	_, err := sh("chroot", append([]string{targetRoot}, args...)...)
	return err
}

func have(name string) bool {
	for _, d := range []string{"usr/sbin", "usr/bin", "sbin", "bin"} {
		if _, err := os.Stat(filepath.Join(targetRoot, d, name)); err == nil {
			return true
		}
	}
	return false
}

func makeBootable(t *TargetDisk, src layout.Disk, plan []layout.Planned, mounted Mounted, snap *repo.Snapshot) error {
	if _, ok := mounted["/"]; !ok {
		return fmt.Errorf("Root-Dateisystem nicht eingehängt")
	}
	for _, m := range []string{"dev", "proc", "sys", "run"} {
		dir := filepath.Join(targetRoot, m)
		os.MkdirAll(dir, 0o755)
		if _, err := sh("mount", "--rbind", "/"+m, dir); err == nil {
			mountsDone = append(mountsDone, dir)
		}
	}
	var errs []string
	if firmware() == "uefi" {
		efiDir := "/boot/efi"
		for k := range mounted {
			if k == "/efi" {
				efiDir = "/efi"
			}
		}
		shim, _ := filepath.Glob(filepath.Join(targetRoot, efiDir, "EFI/*/shimx64.efi"))
		switch {
		case have("grub-install"):
			if err := chroot("grub-install", "--target=x86_64-efi", "--efi-directory="+efiDir, "--recheck"); err != nil {
				errs = append(errs, err.Error())
			}
		case len(shim) > 0:
			espNum := 1
			for i, p := range plan {
				if p.Source.Role == layout.RoleESP {
					espNum = i + 1
				}
			}
			loader := `\EFI\` + filepath.Base(filepath.Dir(shim[0])) + `\shimx64.efi`
			if _, err := sh("efibootmgr", "-c", "-d", t.Path, "-p", strconv.Itoa(espNum), "-L", snap.Hostname, "-l", loader); err != nil {
				errs = append(errs, err.Error())
			}
		}
	} else {
		var err error
		if have("grub-install") {
			err = chroot("grub-install", "--target=i386-pc", "--recheck", t.Path)
		} else if have("grub2-install") {
			err = chroot("grub2-install", "--target=i386-pc", "--recheck", t.Path)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	switch {
	case have("update-initramfs"):
		if err := chroot("update-initramfs", "-u", "-k", "all"); err != nil {
			errs = append(errs, err.Error())
		}
	case have("dracut"):
		if err := chroot("dracut", "-f", "--regenerate-all"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	switch {
	case have("update-grub"):
		if err := chroot("update-grub"); err != nil {
			errs = append(errs, err.Error())
		}
	case have("grub2-mkconfig"):
		if err := chroot("grub2-mkconfig", "-o", "/boot/grub2/grub.cfg"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
