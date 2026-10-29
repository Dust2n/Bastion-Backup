//go:build linux

package tui

import (
	"bufio"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func stdout() io.Writer { return os.Stdout }

func setup() (func(), int, int, error) {
	old, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return nil, 0, 0, errors.New("kein Terminal")
	}
	if _, err := unix.IoctlGetTermios(1, unix.TCGETS); err != nil {
		return nil, 0, 0, errors.New("kein Terminal")
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(0, unix.TCSETS, &raw); err != nil {
		return nil, 0, 0, err
	}
	w, h, _ := size()
	return func() { unix.IoctlSetTermios(0, unix.TCSETS, old) }, w, h, nil
}

func size() (int, int, bool) {
	ws, err := unix.IoctlGetWinsize(1, unix.TIOCGWINSZ)
	if err != nil || ws.Col < 40 || ws.Row < 15 {
		return 80, 25, err == nil
	}
	return int(ws.Col), int(ws.Row), true
}

func readKeys(ch chan<- Key) {
	in := bufio.NewReader(os.Stdin)
	bytesCh := make(chan byte, 64)
	go func() {
		for {
			b, err := in.ReadByte()
			if err != nil {
				close(bytesCh)
				return
			}
			bytesCh <- b
		}
	}()
	next := func(d time.Duration) (byte, bool) {
		select {
		case b, ok := <-bytesCh:
			return b, ok
		case <-time.After(d):
			return 0, false
		}
	}
	for b := range bytesCh {
		switch {
		case b == 0x1b:
			b2, ok := next(50 * time.Millisecond)
			if !ok || (b2 != '[' && b2 != 'O') {
				ch <- Key{Code: KeyEsc}
				continue
			}
			b3, _ := next(50 * time.Millisecond)
			switch b3 {
			case 'A':
				ch <- Key{Code: KeyUp}
			case 'B':
				ch <- Key{Code: KeyDown}
			case 'C':
				ch <- Key{Code: KeyRight}
			case 'D':
				ch <- Key{Code: KeyLeft}
			case 'H':
				ch <- Key{Code: KeyHome}
			case 'F':
				ch <- Key{Code: KeyEnd}
			case '5', '6', '1', '4':
				if t, _ := next(50 * time.Millisecond); t == '~' {
					ch <- Key{Code: map[byte]KeyCode{'5': KeyPgUp, '6': KeyPgDown, '1': KeyHome, '4': KeyEnd}[b3]}
				}
			}
		case b == '\r' || b == '\n':
			ch <- Key{Code: KeyEnter}
		case b == 0x7f || b == 0x08:
			ch <- Key{Code: KeyBackspace}
		case b == '\t':
			ch <- Key{Code: KeyTab}
		case b >= 0x80:
			buf := []byte{b}
			for !utf8.FullRune(buf) && len(buf) < 4 {
				c, ok := next(50 * time.Millisecond)
				if !ok {
					break
				}
				buf = append(buf, c)
			}
			r, _ := utf8.DecodeRune(buf)
			ch <- Key{Code: KeyRune, Rune: r}
		case b >= 32:
			ch <- Key{Code: KeyRune, Rune: rune(b)}
		}
	}
}
