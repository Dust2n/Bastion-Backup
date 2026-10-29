//go:build windows

package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procReadConsoleInputW = kernel32.NewProc("ReadConsoleInputW")
	procSetConsoleCP      = kernel32.NewProc("SetConsoleCP")
)

func stdout() io.Writer { return os.Stdout }

func setup() (func(), int, int, error) {
	in, out := windows.Handle(os.Stdin.Fd()), windows.Handle(os.Stdout.Fd())
	var inMode, outMode uint32
	if windows.GetConsoleMode(in, &inMode) != nil || windows.GetConsoleMode(out, &outMode) != nil {
		return nil, 0, 0, errors.New("keine Konsole")
	}
	if err := windows.SetConsoleMode(out, outMode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.ENABLE_PROCESSED_OUTPUT); err != nil {
		return nil, 0, 0, errors.New("Konsole unterstützt keine Farben (VT)")
	}
	windows.SetConsoleMode(in, windows.ENABLE_WINDOW_INPUT|windows.ENABLE_EXTENDED_FLAGS)
	oldOutCP, _ := windows.GetConsoleOutputCP()
	oldCP, _ := windows.GetConsoleCP()
	windows.SetConsoleOutputCP(65001)
	procSetConsoleCP.Call(65001)
	w, h, _ := size()
	return func() {
		windows.SetConsoleMode(in, inMode)
		windows.SetConsoleMode(out, outMode)
		windows.SetConsoleOutputCP(oldOutCP)
		procSetConsoleCP.Call(uintptr(oldCP))
	}, w, h, nil
}

func size() (int, int, bool) {
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info) != nil {
		return 80, 25, false
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	h := int(info.Window.Bottom-info.Window.Top) + 1
	if w < 40 || h < 15 {
		return 80, 25, true
	}
	return w, h, true
}

// inputRecord is INPUT_RECORD with a KEY_EVENT_RECORD.
type inputRecord struct {
	EventType       uint16
	_               uint16
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

var vkMap = map[uint16]KeyCode{
	0x26: KeyUp, 0x28: KeyDown, 0x25: KeyLeft, 0x27: KeyRight, 0x0D: KeyEnter, 0x1B: KeyEsc,
	0x08: KeyBackspace, 0x09: KeyTab, 0x24: KeyHome, 0x23: KeyEnd, 0x21: KeyPgUp, 0x22: KeyPgDown, 0x79: KeyF10,
}

// readKeys reads key events directly, so characters come out exactly as
// the active keyboard layout (German in the rescue system) produces them.
var debugKeys *os.File

func readKeys(ch chan<- Key) {
	if p := os.Getenv("BASTION_TUI_DEBUG"); p != "" {
		debugKeys, _ = os.Create(p)
	}
	in := windows.Handle(os.Stdin.Fd())
	recs := make([]inputRecord, 16)
	for {
		var n uint32
		r, _, _ := procReadConsoleInputW.Call(uintptr(in), uintptr(unsafe.Pointer(&recs[0])), uintptr(len(recs)), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return
		}
		for _, rec := range recs[:n] {
			if debugKeys != nil {
				fmt.Fprintf(debugKeys, "type=%d down=%d vk=%#x scan=%#x char=%#x ctrl=%#x\n", rec.EventType, rec.KeyDown, rec.VirtualKeyCode, rec.VirtualScanCode, rec.UnicodeChar, rec.ControlKeyState)
			}
			if rec.EventType != 1 || rec.KeyDown == 0 {
				continue
			}
			if code, ok := vkMap[rec.VirtualKeyCode]; ok {
				ch <- Key{Code: code}
				continue
			}
			if c := rec.UnicodeChar; c >= 32 && (c < 0xD800 || c > 0xDFFF) {
				ch <- Key{Code: KeyRune, Rune: rune(c)}
			}
		}
	}
}
