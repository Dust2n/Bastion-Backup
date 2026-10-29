//go:build windows

package rescue

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procLoadKeyboardLayoutW      = user32.NewProc("LoadKeyboardLayoutW")
	procGetKeyboardLayout        = user32.NewProc("GetKeyboardLayout")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procGetConsoleWindow         = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
)

const (
	klfActivate      = 0x1
	klfSetForProcess = 0x100
	wmInputLangReq   = 0x50
)

// setKeyboard switches the console to the German ("de") or US layout. In
// WinPE wpeutil sets the system default; the console window additionally
// gets a language change request so the switch takes effect immediately.
func setKeyboard(layout string) {
	klid, wpe := "00000407", "0407:00000407"
	if layout == "us" {
		klid, wpe = "00000409", "0409:00000409"
	}
	if path, err := exec.LookPath("wpeutil.exe"); err == nil {
		cmd := exec.Command(path, "SetKeyboardLayout", wpe)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		cmd.Run()
	}
	hkl, _, _ := procLoadKeyboardLayoutW.Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(klid))), klfActivate|klfSetForProcess)
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 && hkl != 0 {
		procPostMessageW.Call(hwnd, wmInputLangReq, 0, hkl)
	}
}

// keyboardName returns the layout of the console window (DE, US, ...).
func keyboardName() string {
	var hkl uintptr
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		tid, _, _ := procGetWindowThreadProcessId.Call(hwnd, 0)
		hkl, _, _ = procGetKeyboardLayout.Call(tid)
	}
	if hkl == 0 {
		hkl, _, _ = procGetKeyboardLayout.Call(0)
	}
	switch lang := hkl & 0xffff; lang {
	case 0x0407:
		return "DE"
	case 0x0807:
		return "CH"
	case 0x0c07:
		return "AT"
	case 0x0409:
		return "US"
	case 0x0809:
		return "UK"
	default:
		return fmt.Sprintf("%04x", lang)
	}
}
