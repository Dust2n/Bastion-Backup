//go:build windows

package server

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modmpr                    = windows.NewLazySystemDLL("mpr.dll")
	procWNetAddConnection2W   = modmpr.NewProc("WNetAddConnection2W")
	procWNetCancelConnection2 = modmpr.NewProc("WNetCancelConnection2W")
)

type netResource struct {
	Scope, Type, DisplayType, Usage uint32
	LocalName, RemoteName           *uint16
	Comment, Provider               *uint16
}

func wnetConnect(remote, user, pass string) error {
	nr := netResource{Type: 1 /* RESOURCETYPE_DISK */, RemoteName: windows.StringToUTF16Ptr(remote)}
	var u, p uintptr
	if user != "" {
		u = uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(user)))
		p = uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(pass)))
	}
	r, _, _ := procWNetAddConnection2W.Call(uintptr(unsafe.Pointer(&nr)), p, u, 0)
	if r == 1219 { // ERROR_SESSION_CREDENTIAL_CONFLICT: drop the old connection and retry
		wnetDisconnect(remote)
		r, _, _ = procWNetAddConnection2W.Call(uintptr(unsafe.Pointer(&nr)), p, u, 0)
	}
	if r != 0 {
		return windows.Errno(r)
	}
	return nil
}

func wnetDisconnect(remote string) {
	procWNetCancelConnection2.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(remote))), 0, 1)
}

// connectShare logs on to the share of a UNC path (\\server\share\...).
func connectShare(path, user, pass string) error {
	parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
	if len(parts) < 2 {
		return fmt.Errorf(`Netzwerkpfad %q ungültig (\\server\freigabe\ordner)`, path)
	}
	root := `\\` + parts[0] + `\` + parts[1]
	if err := wnetConnect(root, user, pass); err != nil && err != windows.Errno(85) /* already connected */ {
		return fmt.Errorf("%s: %v", root, err)
	}
	return nil
}

func diskFree(path string) (free, total int64) {
	var avail, tot, totFree uint64
	if windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(path), &avail, &tot, &totFree) == nil {
		return int64(avail), int64(tot)
	}
	return 0, 0
}

func listDrives() []dirEntry {
	var out []dirEntry
	mask, _ := windows.GetLogicalDrives()
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		t := windows.GetDriveType(windows.StringToUTF16Ptr(root))
		if t != windows.DRIVE_FIXED && t != windows.DRIVE_REMOTE && t != windows.DRIVE_REMOVABLE {
			continue
		}
		var label [261]uint16
		windows.GetVolumeInformation(windows.StringToUTF16Ptr(root), &label[0], uint32(len(label)), nil, nil, nil, nil, 0)
		free, total := diskFree(root)
		if total == 0 {
			continue
		}
		out = append(out, dirEntry{Name: root, Path: root, Free: free, Total: total, Label: windows.UTF16ToString(label[:])})
	}
	return out
}
