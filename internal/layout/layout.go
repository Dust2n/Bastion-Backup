// Package layout describes disks and partitions of a machine and computes
// a new partition plan for a (possibly smaller or larger) target disk.
package layout

import (
	"fmt"
	"sort"
	"strings"
)

const (
	MiB = int64(1) << 20
	GiB = int64(1) << 30
)

// Partition roles.
const (
	RoleESP      = "esp"      // EFI system partition
	RoleMSR      = "msr"      // Microsoft reserved
	RoleRecovery = "recovery" // Windows recovery (WinRE)
	RoleSystem   = "system"   // Windows C: or Linux /
	RoleBoot     = "boot"     // Linux /boot or MBR "System Reserved"
	RoleData     = "data"     // other file systems
	RoleSwap     = "swap"
	RoleBIOSBoot = "biosboot" // GRUB BIOS boot partition on GPT
	RoleLVM      = "lvm"      // LVM physical volume, logical volumes in Children
	RoleOther    = "other"
)

type Machine struct {
	OS       string `json:"os"`       // windows | linux
	Firmware string `json:"firmware"` // uefi | bios
	Disks    []Disk `json:"disks"`
}

type Disk struct {
	Number     int         `json:"number"`
	Path       string      `json:"path"` // \\.\PhysicalDrive0 or /dev/sda
	Size       int64       `json:"size"`
	Style      string      `json:"style"` // gpt | mbr
	Model      string      `json:"model,omitempty"`
	Serial     string      `json:"serial,omitempty"`
	System     bool        `json:"system"` // holds the running OS
	Partitions []Partition `json:"partitions"`
}

type Partition struct {
	Number int    `json:"number"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
	Type   string `json:"type,omitempty"` // GPT type GUID or MBR type id
	Role   string `json:"role"`
	FS     string `json:"fs,omitempty"` // ntfs, fat32, ext4, xfs, btrfs, vfat, swap
	Label  string `json:"label,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Mount  string `json:"mount,omitempty"` // C: or /boot
	Used   int64  `json:"used,omitempty"`
	Active bool   `json:"active,omitempty"`
	// Volume is the key of the backed-up volume in the snapshot, empty if
	// the content of this partition is not part of the backup.
	Volume string `json:"volume,omitempty"`
	// LVM: volume group name and logical volumes (Label = LV name).
	VG       string      `json:"vg,omitempty"`
	Children []Partition `json:"children,omitempty"`
}

// Planned is a partition to create on the target disk.
type Planned struct {
	Source Partition `json:"source"`
	Size   int64     `json:"size"`
}

// fixedSize returns the size a non-scalable partition gets, or -1 when the
// partition is scalable, or 0 when it is dropped.
func fixedSize(p Partition) int64 {
	switch p.Role {
	case RoleESP:
		if p.Size < 100*MiB {
			return 100 * MiB
		}
		return p.Size
	case RoleMSR:
		return 16 * MiB
	case RoleBIOSBoot:
		return 1 * MiB
	case RoleBoot:
		if p.Volume == "" {
			return p.Size
		}
		return -1
	case RoleRecovery:
		if p.Volume != "" {
			return -1
		}
		return 0 // recreated by Windows (reagentc) if needed, content not backed up
	case RoleSwap:
		return -1
	case RoleLVM:
		for _, c := range p.Children {
			if c.Volume != "" {
				return -1
			}
		}
		return 0
	case RoleSystem, RoleData:
		if p.Volume == "" {
			return 0
		}
		return -1
	default:
		return 0
	}
}

// MinSize is the smallest size a scalable partition can be restored into.
func MinSize(p Partition) int64 {
	if p.Role == RoleSwap {
		return 512 * MiB
	}
	if p.Role == RoleLVM {
		m := 64 * MiB // LVM metadata and extent rounding
		for _, c := range p.Children {
			if f := fixedSize(c); f > 0 {
				m += align(f)
			} else if f < 0 {
				m += MinSize(c) + 4*MiB
			}
		}
		return m
	}
	slack := p.Used / 10
	if slack < 1*GiB {
		slack = 1 * GiB
	}
	if p.Role == RoleBoot && slack > 256*MiB {
		slack = 256 * MiB
	}
	m := align(p.Used + slack)
	if p.Used == 0 && p.Volume == "" {
		m = align(p.Size) // content unknown (not backed up): keep size
	}
	if m > p.Size && p.Size > 0 {
		// never require more than the original partition had
		m = align(p.Size)
	}
	return m
}

func align(n int64) int64 { return (n + MiB - 1) / MiB * MiB }

// Scale plans the partitions of src onto a disk of targetSize bytes.
// Partitions keep their order. When the target is at least as large as the
// original layout needs, all partitions keep their size and the system
// partition grows. Otherwise scalable partitions shrink proportionally, but
// never below their used size plus a safety margin.
func Scale(src Disk, targetSize int64) ([]Planned, error) {
	parts := append([]Partition(nil), src.Partitions...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].Offset < parts[j].Offset })

	usable := (targetSize - 2*MiB) / MiB * MiB // 1 MiB alignment at start, GPT backup at end
	if src.Style == "gpt" {
		usable -= MiB
	}

	var plan []Planned
	var fixed, sumOrig, sumMin int64
	var scal []int
	for _, p := range parts {
		f := fixedSize(p)
		if f == 0 {
			continue
		}
		plan = append(plan, Planned{Source: p})
		i := len(plan) - 1
		if f > 0 {
			plan[i].Size = align(f)
			fixed += plan[i].Size
			continue
		}
		scal = append(scal, i)
		sumOrig += align(p.Size)
		sumMin += MinSize(p)
	}
	if len(scal) == 0 {
		return nil, fmt.Errorf("keine gesicherten Partitionen auf Datenträger %d", src.Number)
	}

	need := fixed + sumMin
	if need > usable {
		return nil, fmt.Errorf("Zieldatenträger zu klein: benötigt mindestens %s, verfügbar %s", Human(need+2*MiB), Human(targetSize))
	}

	if fixed+sumOrig <= usable {
		// Enough room: keep original sizes, give the rest to the main partition.
		for _, i := range scal {
			plan[i].Size = align(plan[i].Source.Size)
		}
		plan[mainIndex(plan, scal)].Size += usable - fixed - sumOrig
	} else {
		// Shrink: each partition gets its minimum plus a share of the extra
		// space proportional to how much it could originally shrink.
		extra := usable - fixed - sumMin
		var weight int64
		for _, i := range scal {
			weight += align(plan[i].Source.Size) - MinSize(plan[i].Source)
		}
		var given int64
		for _, i := range scal {
			p := plan[i].Source
			share := int64(0)
			if weight > 0 {
				share = int64(float64(extra) * float64(align(p.Size)-MinSize(p)) / float64(weight))
			}
			share = share / MiB * MiB
			plan[i].Size = MinSize(p) + share
			given += share
		}
		plan[mainIndex(plan, scal)].Size += (extra - given) / MiB * MiB
	}
	return plan, nil
}

func mainIndex(plan []Planned, scal []int) int {
	best := scal[0]
	for _, i := range scal {
		if plan[i].Source.Role == RoleSystem {
			return i
		}
		if plan[i].Source.Size > plan[best].Source.Size {
			best = i
		}
	}
	return best
}

// ScaleLVM plans the logical volumes of an LVM partition into pvSize bytes.
func ScaleLVM(pv Partition, pvSize int64) ([]Planned, error) {
	children := append([]Partition(nil), pv.Children...)
	for i := range children {
		children[i].Offset = int64(i)
	}
	return Scale(Disk{Number: pv.Number, Style: "lvm", Partitions: children}, pvSize-60*MiB)
}

// WithBackupSizes raises the used size of each backed-up partition to the
// amount of data in the backup (files compressed by NTFS/CompactOS are
// stored and restored uncompressed, so they need more space than before).
func WithBackupSizes(d Disk, bytes map[string]int64) Disk {
	adj := func(p *Partition) {
		if b, ok := bytes[p.Volume]; ok && p.Volume != "" {
			b += b / 50 // file system metadata
			if b > p.Used {
				p.Used = b
			}
		}
	}
	parts := make([]Partition, len(d.Partitions))
	for i, p := range d.Partitions {
		adj(&p)
		if len(p.Children) > 0 {
			p.Children = append([]Partition(nil), p.Children...)
			for j := range p.Children {
				adj(&p.Children[j])
			}
		}
		parts[i] = p
	}
	d.Partitions = parts
	return d
}

// Required returns the minimum target disk size for src.
func Required(src Disk) int64 {
	var n int64 = 3 * MiB
	for _, p := range src.Partitions {
		f := fixedSize(p)
		switch {
		case f > 0:
			n += align(f)
		case f < 0:
			n += MinSize(p)
		}
	}
	return n
}

func Human(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	s := fmt.Sprintf("%.1f %s", f, units[i])
	return strings.Replace(s, ".0 ", " ", 1)
}
