//go:build linux

package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bastion/internal/layout"
)

type lsblkDev struct {
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Type       string     `json:"type"`
	Size       jsonInt    `json:"size"`
	FSType     string     `json:"fstype"`
	UUID       string     `json:"uuid"`
	Label      string     `json:"label"`
	Mountpoint string     `json:"mountpoint"`
	PartType   string     `json:"parttype"`
	PTType     string     `json:"pttype"`
	Model      string     `json:"model"`
	Serial     string     `json:"serial"`
	FSUsed     jsonInt    `json:"fsused"`
	RM         jsonBool   `json:"rm"`
	Tran       string     `json:"tran"`
	Children   []lsblkDev `json:"children"`
}

// lsblk prints numbers as numbers or strings depending on its version.
type jsonInt int64

func (j *jsonInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*j = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*j = jsonInt(n)
	return err
}

type jsonBool bool

func (j *jsonBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*j = jsonBool(s == "true" || s == "1")
	return nil
}

func sysInt(name, attr string) int64 {
	b, err := os.ReadFile(filepath.Join("/sys/class/block", name, attr))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

var supportedFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "vfat": true}

func role(dev lsblkDev) string {
	pt := strings.ToLower(dev.PartType)
	switch {
	case pt == gptESP || pt == "0xef" || dev.Mountpoint == "/boot/efi" || dev.Mountpoint == "/efi":
		return layout.RoleESP
	case pt == gptBIOSBoot:
		return layout.RoleBIOSBoot
	case pt == gptSwap || pt == "0x82" || dev.FSType == "swap":
		return layout.RoleSwap
	case pt == gptLVM || pt == "0x8e" || dev.FSType == "LVM2_member":
		return layout.RoleLVM
	case dev.Mountpoint == "/":
		return layout.RoleSystem
	case dev.Mountpoint == "/boot":
		return layout.RoleBoot
	case dev.Mountpoint != "":
		return layout.RoleData
	}
	return layout.RoleOther
}

func toPartition(dev lsblkDev) layout.Partition {
	p := layout.Partition{
		Number: int(sysInt(dev.Name, "partition")),
		Offset: sysInt(dev.Name, "start") * 512,
		Size:   int64(dev.Size),
		Type:   strings.ToLower(dev.PartType),
		Role:   role(dev),
		FS:     dev.FSType,
		Label:  dev.Label,
		UUID:   dev.UUID,
		Mount:  dev.Mountpoint,
		Used:   int64(dev.FSUsed),
	}
	if p.Mount != "" && supportedFS[p.FS] {
		p.Volume = p.Mount
	}
	return p
}

// CaptureLayout reads disks, partitions and LVM volumes via lsblk.
func CaptureLayout() (*layout.Machine, error) {
	out, err := run("lsblk", "-J", "-b", "-o", "NAME,PATH,TYPE,SIZE,FSTYPE,UUID,LABEL,MOUNTPOINT,PARTTYPE,PTTYPE,MODEL,SERIAL,FSUSED,RM,TRAN")
	if err != nil {
		return nil, fmt.Errorf("lsblk: %v: %s", err, out)
	}
	var res struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	lvNames := lvmNames()
	m := &layout.Machine{OS: "linux", Firmware: "bios"}
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		m.Firmware = "uefi"
	}
	for i, d := range res.Blockdevices {
		if d.Type != "disk" || bool(d.RM) || d.Tran == "usb" {
			continue
		}
		ld := layout.Disk{Number: i, Path: d.Path, Size: int64(d.Size), Style: d.PTType, Model: strings.TrimSpace(d.Model), Serial: strings.TrimSpace(d.Serial)}
		if ld.Style == "dos" {
			ld.Style = "mbr"
		}
		for _, c := range d.Children {
			if c.Type != "part" {
				continue
			}
			p := toPartition(c)
			if p.Role == layout.RoleLVM {
				for _, lv := range c.Children {
					if lv.Type != "lvm" {
						continue
					}
					cp := toPartition(lv)
					cp.Number, cp.Offset = 0, 0
					if n, ok := lvNames[resolve(lv.Path)]; ok {
						p.VG, cp.Label = n[0], n[1]
					}
					if cp.Role == layout.RoleLVM || cp.Role == layout.RoleOther {
						cp.Role = layout.RoleData
					}
					p.Children = append(p.Children, cp)
				}
			}
			if p.Mount == "/" || hasMount(p, "/") {
				ld.System = true
			}
			ld.Partitions = append(ld.Partitions, p)
		}
		m.Disks = append(m.Disks, ld)
	}
	return m, nil
}

func hasMount(p layout.Partition, mnt string) bool {
	for _, c := range p.Children {
		if c.Mount == mnt {
			return true
		}
	}
	return false
}

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// lvmNames maps /dev/dm-N to [vg, lv].
func lvmNames() map[string][2]string {
	out, err := run("lvs", "--noheadings", "--separator", "|", "-o", "vg_name,lv_name,lv_path")
	res := map[string][2]string{}
	if err != nil {
		return res
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) == 3 {
			res[resolve(f[2])] = [2]string{f[0], f[1]}
		}
	}
	return res
}

// CreateSnapshot snapshots the file system mounted at mount.
func CreateSnapshot(mount string) (*Snap, error) {
	out, err := run("findmnt", "-n", "-o", "SOURCE,FSTYPE", "-M", mount)
	if err != nil {
		return nil, fmt.Errorf("findmnt %s: %s", mount, out)
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return nil, fmt.Errorf("findmnt %s: %q", mount, out)
	}
	src, fstype := f[0], f[1]
	if i := strings.Index(src, "["); i > 0 {
		src = src[:i]
	}

	if fstype == "btrfs" {
		dir := filepath.Join(mount, ".bastion-snap")
		run("btrfs", "subvolume", "delete", dir)
		if out, err := run("btrfs", "subvolume", "snapshot", "-r", mount, dir); err == nil {
			return &Snap{Root: dir, Kind: "btrfs", release: func() { run("btrfs", "subvolume", "delete", dir) }}, nil
		} else {
			return &Snap{Root: mount, Kind: "none", Warning: "btrfs-Snapshot fehlgeschlagen: " + out}, nil
		}
	}

	if names, ok := lvmNames()[resolve(src)]; ok {
		vg, lv := names[0], names[1]
		snapName := "bastion_snap_" + lv
		run("lvremove", "-f", vg+"/"+snapName)
		if out, err := run("lvcreate", "-s", "-n", snapName, "-l", "20%ORIGIN", vg+"/"+lv); err != nil {
			return &Snap{Root: mount, Kind: "none", Warning: "LVM-Snapshot nicht möglich (freier Platz in VG " + vg + "?): " + out}, nil
		}
		dir := "/run/bastion/snap-" + lv
		os.MkdirAll(dir, 0o700)
		opts := "ro"
		if fstype == "xfs" {
			opts += ",nouuid"
		}
		if out, err := run("mount", "-o", opts, "/dev/"+vg+"/"+snapName, dir); err != nil {
			run("lvremove", "-f", vg+"/"+snapName)
			return &Snap{Root: mount, Kind: "none", Warning: "LVM-Snapshot nicht einhängbar: " + out}, nil
		}
		return &Snap{Root: dir, Kind: "lvm", release: func() {
			run("umount", dir)
			run("lvremove", "-f", vg+"/"+snapName)
		}}, nil
	}

	return &Snap{Root: mount, Kind: "none",
		Warning: fmt.Sprintf("%s (%s) liegt weder auf LVM noch btrfs: Sicherung ohne Snapshot, geöffnete Dateien können inkonsistent sein", mount, fstype)}, nil
}

// DeleteSnapshot is only used for VSS on Windows.
func DeleteSnapshot(string) {}

func OSVersion() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
		}
	}
	return "Linux"
}

// VolumeOf returns the mount point containing p and the path below it.
func VolumeOf(p string) (vol, rel string) {
	out, err := run("findmnt", "-n", "-o", "TARGET", "--target", p)
	if err != nil || out == "" {
		return "/", strings.TrimLeft(p, "/")
	}
	vol = strings.TrimSpace(strings.Split(out, "\n")[0])
	rel = strings.TrimLeft(strings.TrimPrefix(p, vol), "/")
	return vol, rel
}

func JoinSnap(root, rel string) string { return filepath.Join(root, rel) }

// FormatVolume unmounts, recreates (same UUID and label) and remounts a
// data file system listed in /etc/fstab.
func FormatVolume(mount, fs, label string) error {
	if mount == "/" || mount == "/boot" || mount == "/usr" {
		return fmt.Errorf("%s kann nicht im laufenden Betrieb formatiert werden (Image-Wiederherstellung verwenden)", mount)
	}
	out, err := run("findmnt", "-n", "-o", "SOURCE,UUID", "-M", mount)
	if err != nil {
		return fmt.Errorf("findmnt %s: %s", mount, out)
	}
	f := strings.Fields(out)
	if len(f) < 1 {
		return fmt.Errorf("Quelle von %s unbekannt", mount)
	}
	dev, uuid := f[0], ""
	if len(f) > 1 {
		uuid = f[1]
	}
	if out, err := run("umount", mount); err != nil {
		users, _ := run("fuser", "-vm", mount)
		return fmt.Errorf("%s ist in Benutzung: %s %s", mount, out, users)
	}
	if err := Mkfs(dev, fs, uuid, label); err != nil {
		run("mount", mount)
		return err
	}
	if out, err := run("mount", mount); err != nil {
		return fmt.Errorf("mount %s: %s", mount, out)
	}
	return nil
}

// Mkfs creates a file system with the given UUID and label.
func Mkfs(dev, fs, uuid, label string) error {
	var args []string
	switch fs {
	case "ext2", "ext3", "ext4":
		args = []string{"mkfs." + fs, "-F", "-q"}
		if uuid != "" {
			args = append(args, "-U", uuid)
		}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "xfs":
		args = []string{"mkfs.xfs", "-f", "-q"}
		if uuid != "" {
			args = append(args, "-m", "uuid="+uuid)
		}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "btrfs":
		args = []string{"mkfs.btrfs", "-f", "-q"}
		if uuid != "" {
			args = append(args, "-U", uuid)
		}
		if label != "" {
			args = append(args, "-L", label)
		}
	case "vfat":
		args = []string{"mkfs.vfat", "-F", "32"}
		if uuid != "" {
			args = append(args, "-i", strings.ReplaceAll(uuid, "-", ""))
		}
		if label != "" {
			args = append(args, "-n", label)
		}
	case "swap":
		args = []string{"mkswap"}
		if uuid != "" {
			args = append(args, "-U", uuid)
		}
		if label != "" {
			args = append(args, "-L", label)
		}
	default:
		return fmt.Errorf("Dateisystem %s wird nicht unterstützt", fs)
	}
	args = append(args, dev)
	if out, err := run(args[0], args[1:]...); err != nil {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), out)
	}
	return nil
}

func ScheduleReboot(delay int, msg string) error {
	if delay > 0 {
		run("wall", msg)
		_, err := run("shutdown", "-r", fmt.Sprintf("+%d", (delay+59)/60), msg)
		return err
	}
	_, err := run("systemctl", "reboot")
	return err
}
