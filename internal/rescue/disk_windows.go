//go:build windows

package rescue

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native disk queries via DeviceIoControl, so the rescue program works in
// a bare WinPE without PowerShell or the Storage WMI provider.

const (
	ioctlDiskGetDriveGeometryEx = 0x000700a0
	ioctlDiskGetDriveLayoutEx   = 0x00070050
	ioctlStorageQueryProperty   = 0x002d1400
	ioctlStorageGetDeviceNumber = 0x002d1080
	busTypeUSB                  = 7
	partitionStyleMBR           = 0
	partitionStyleGPT           = 1
	gptTypeMSR                  = "{E3C9E316-0B5C-4DB8-817D-F92DF00215AE}"
)

func openDevice(path string) (windows.Handle, error) {
	return windows.CreateFile(windows.StringToUTF16Ptr(path), 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
}

func ioctl(h windows.Handle, code uint32, in []byte, outSize int) ([]byte, error) {
	out := make([]byte, outSize)
	var n uint32
	var inPtr *byte
	if len(in) > 0 {
		inPtr = &in[0]
	}
	if err := windows.DeviceIoControl(h, code, inPtr, uint32(len(in)), &out[0], uint32(len(out)), &n, nil); err != nil {
		return nil, err
	}
	return out[:n], nil
}

func cstr(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	end := int(off)
	for end < len(b) && b[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(b[off:end]))
}

// listDisks returns all non-USB physical disks.
func listDisks() ([]TargetDisk, error) {
	var disks []TargetDisk
	for n := 0; n < 64; n++ {
		h, err := openDevice(fmt.Sprintf(`\\.\PhysicalDrive%d`, n))
		if err != nil {
			continue
		}
		geo, err := ioctl(h, ioctlDiskGetDriveGeometryEx, nil, 256)
		if err != nil || len(geo) < 32 {
			windows.CloseHandle(h)
			continue
		}
		d := TargetDisk{Number: n, Path: fmt.Sprintf(`\\.\PhysicalDrive%d`, n), Size: int64(binary.LittleEndian.Uint64(geo[24:32]))}
		// STORAGE_PROPERTY_QUERY{StorageDeviceProperty, PropertyStandardQuery}
		if desc, err := ioctl(h, ioctlStorageQueryProperty, make([]byte, 12), 1024); err == nil && len(desc) >= 32 {
			vendor := cstr(desc, binary.LittleEndian.Uint32(desc[12:]))
			product := cstr(desc, binary.LittleEndian.Uint32(desc[16:]))
			d.Model = strings.TrimSpace(vendor + " " + product)
			d.Serial = cstr(desc, binary.LittleEndian.Uint32(desc[24:]))
			if binary.LittleEndian.Uint32(desc[28:]) == busTypeUSB {
				windows.CloseHandle(h)
				continue // the rescue stick itself or external drives
			}
		}
		windows.CloseHandle(h)
		if d.Size > 0 {
			disks = append(disks, d)
		}
	}
	if len(disks) == 0 {
		return nil, fmt.Errorf("keine Festplatte gefunden (fehlender Speicher-Treiber? Bei VMs SATA/AHCI statt virtio wählen)")
	}
	return disks, nil
}

type partInfo struct {
	Number uint32
	Offset uint64
	Type   string // GPT type GUID, or MBR type as 0x..
	ID     [16]byte
	IsGPT  bool
	MBRSig uint32
}

// diskLayout reads the partition table of a physical disk.
func diskLayout(disk int) ([]partInfo, error) {
	h, err := openDevice(fmt.Sprintf(`\\.\PhysicalDrive%d`, disk))
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	b, err := ioctl(h, ioctlDiskGetDriveLayoutEx, nil, 48+128*144)
	if err != nil {
		return nil, err
	}
	style := binary.LittleEndian.Uint32(b[0:])
	count := binary.LittleEndian.Uint32(b[4:])
	sig := binary.LittleEndian.Uint32(b[8:])
	var parts []partInfo
	for i := uint32(0); i < count && int(48+(i+1)*144) <= len(b); i++ {
		e := b[48+i*144:]
		p := partInfo{
			Offset: binary.LittleEndian.Uint64(e[8:]),
			Number: binary.LittleEndian.Uint32(e[24:]),
			IsGPT:  style == partitionStyleGPT,
			MBRSig: sig,
		}
		length := binary.LittleEndian.Uint64(e[16:])
		if length == 0 {
			continue // unused MBR slot
		}
		if p.IsGPT {
			g := (*windows.GUID)(unsafe.Pointer(&e[32]))
			p.Type = g.String()
			copy(p.ID[:], e[48:64])
		} else {
			p.Type = fmt.Sprintf("0x%02x", e[32])
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// volumeLocation returns disk and partition number of a drive letter.
func volumeLocation(letter byte) (disk, part uint32, err error) {
	h, err := openDevice(fmt.Sprintf(`\\.\%c:`, letter))
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	b, err := ioctl(h, ioctlStorageGetDeviceNumber, nil, 12)
	if err != nil || len(b) < 12 {
		return 0, 0, fmt.Errorf("%c: Gerätenummer nicht lesbar: %v", letter, err)
	}
	return binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:]), nil
}

// hasMSR reports whether "convert gpt" already created an MSR partition.
func hasMSR(disk int) bool {
	parts, err := diskLayout(disk)
	if err != nil {
		return false
	}
	for _, p := range parts {
		if strings.EqualFold(p.Type, gptTypeMSR) {
			return true
		}
	}
	return false
}

// mountedDevicesValue builds the MountedDevices entry that maps a drive
// letter to the partition currently mounted at letter.
func mountedDevicesValue(letter byte) ([]byte, error) {
	disk, num, err := volumeLocation(letter)
	if err != nil {
		return nil, err
	}
	parts, err := diskLayout(int(disk))
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if p.Number != num {
			continue
		}
		if p.IsGPT {
			return append([]byte("DMIO:ID:"), p.ID[:]...), nil
		}
		v := binary.LittleEndian.AppendUint32(nil, p.MBRSig)
		return binary.LittleEndian.AppendUint64(v, p.Offset), nil
	}
	return nil, fmt.Errorf("Partition %d auf Datenträger %d nicht gefunden", num, disk)
}
