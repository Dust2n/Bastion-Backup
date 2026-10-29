// Package platform wraps OS specifics: snapshots (VSS, LVM, btrfs), disk
// layout discovery, system information and boot configuration.
package platform

import (
	"net"
	"os/exec"
	"strings"

	"bastion/internal/layout"
)

// Snap is a point-in-time view of a volume.
type Snap struct {
	Root    string // path to read from
	Kind    string // vss | lvm | btrfs | none
	Warning string
	Info    string // shown in the job log
	release func()
}

func (s *Snap) Release() {
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

// VolumeSpec is a volume that belongs into an image backup.
type VolumeSpec struct {
	Key    string // C: or /home
	Source string // path to snapshot/read
	FS     string
	Label  string
	UUID   string
	Size   int64
	Used   int64
}

// ImageVolumes lists the volumes referenced by a layout.
func ImageVolumes(m *layout.Machine) []VolumeSpec {
	var out []VolumeSpec
	add := func(p layout.Partition) {
		if p.Volume == "" {
			return
		}
		src := p.Mount
		if m.OS == "windows" && len(src) == 2 {
			src += `\`
		}
		out = append(out, VolumeSpec{Key: p.Volume, Source: src, FS: p.FS, Label: p.Label, UUID: p.UUID, Size: p.Size, Used: p.Used})
	}
	for _, d := range m.Disks {
		for _, p := range d.Partitions {
			add(p)
			for _, c := range p.Children {
				add(c)
			}
		}
	}
	return out
}

// NetInfo returns non-loopback IPv4/IPv6 addresses and MAC addresses.
func NetInfo() (ips, macs []string) {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 {
			continue
		}
		if len(i.HardwareAddr) == 6 {
			macs = append(macs, i.HardwareAddr.String())
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP.String())
			}
		}
	}
	return
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const (
	gptESP      = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	gptMSR      = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
	gptRecovery = "de94bba4-06d1-4d40-a16a-bfd50179d6ac"
	gptBIOSBoot = "21686148-6449-6e6f-744e-656564454649"
	gptSwap     = "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f"
	gptLVM      = "e6d6d379-f507-44c2-a23c-238f2a3df928"
	gptLinuxFS  = "0fc63daf-8483-4772-8e79-3d69d8477de4"
	gptBasic    = "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"
)
