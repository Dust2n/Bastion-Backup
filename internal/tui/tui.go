// Package tui is a small full-screen text UI for the rescue environment:
// colours, frames, arrow-key menus, input fields, selectable tables,
// confirmation dialogs and a progress view. It uses VT escape sequences
// (Windows 10+ console, Linux terminals) and needs no other libraries.
// Only characters of code page 437 are used, so the WinPE raster font can
// show everything.
package tui

import (
	"bufio"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Key is a pressed key.
type Key struct {
	Code KeyCode
	Rune rune
}

type KeyCode int

const (
	KeyRune KeyCode = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyEnter
	KeyEsc
	KeyBackspace
	KeyTab
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDown
	KeyF10
)

// colours (ANSI)
const (
	reset  = "\x1b[0m"
	bgMain = "\x1b[48;5;17m"  // dark blue
	bgBar  = "\x1b[48;5;24m"  // header/status
	bgBox  = "\x1b[48;5;236m" // dialog
	bgSel  = "\x1b[48;5;33m"  // selection
	bgBad  = "\x1b[48;5;88m"  // danger dialog
	fgText = "\x1b[38;5;255m"
	fgDim  = "\x1b[38;5;249m"
	fgAcc  = "\x1b[38;5;117m"
	fgOK   = "\x1b[38;5;120m"
	fgBad  = "\x1b[38;5;210m"
	fgWarn = "\x1b[38;5;221m"
	bold   = "\x1b[1m"
)

// Screen is the terminal in full-screen mode.
type Screen struct {
	mu      sync.Mutex
	w, h    int
	out     *bufio.Writer
	keys    chan Key
	restore func()
	Title   string
	// Status returns the left and right text of the status bar.
	Status func() (string, string)
	Hint   string
}

// Open switches the console to full-screen mode. It fails when stdin or
// stdout is not a console (redirected), then callers use line mode.
func Open(title string) (*Screen, error) {
	s := &Screen{Title: title, keys: make(chan Key, 32)}
	restore, w, h, err := setup()
	if err != nil {
		return nil, err
	}
	s.restore, s.w, s.h = restore, w, h
	s.out = bufio.NewWriterSize(stdout(), 64<<10)
	go readKeys(s.keys)
	fmt.Fprint(s.out, "\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	s.out.Flush()
	return s, nil
}

// Close restores the console.
func (s *Screen) Close() {
	fmt.Fprint(s.out, reset+"\x1b[2J\x1b[H\x1b[?25h\x1b[?1049l")
	s.out.Flush()
	if s.restore != nil {
		s.restore()
	}
}

// Size returns columns and rows.
func (s *Screen) Size() (int, int) {
	if w, h, ok := size(); ok {
		s.w, s.h = w, h
	}
	return s.w, s.h
}

// ReadKey waits for a key; ok is false on timeout (d > 0).
func (s *Screen) ReadKey(d time.Duration) (Key, bool) {
	if d <= 0 {
		return <-s.keys, true
	}
	select {
	case k := <-s.keys:
		return k, true
	case <-time.After(d):
		return Key{}, false
	}
}

// ---------- drawing ----------

func (s *Screen) at(x, y int) string { return fmt.Sprintf("\x1b[%d;%dH", y+1, x+1) }

// fit cuts or pads text to exactly n visible cells; colour escape
// sequences inside t are kept and not counted.
func fit(t string, n int) string {
	if n <= 0 {
		return ""
	}
	total := Len(t)
	var b strings.Builder
	cnt, esc := 0, false
	for _, r := range t {
		switch {
		case r == 0x1b:
			esc = true
			b.WriteRune(r)
		case esc:
			b.WriteRune(r)
			if r == 'm' {
				esc = false
			}
		case cnt >= n:
		case total > n && cnt == n-1:
			b.WriteRune('~')
			cnt++
		default:
			b.WriteRune(r)
			cnt++
		}
	}
	if cnt < n {
		b.WriteString(strings.Repeat(" ", n-cnt))
	}
	return b.String()
}

// frame draws the background, header and status bar.
func (s *Screen) frame() {
	w, h := s.Size()
	var b strings.Builder
	b.WriteString(reset + bgMain + fgText + "\x1b[2J")
	for y := 1; y < h-1; y++ {
		b.WriteString(s.at(0, y) + strings.Repeat(" ", w))
	}
	// header
	b.WriteString(s.at(0, 0) + bgBar + fgText + fit(bold+"  ▓ BASTION  "+reset+bgBar+fgDim+"│ "+fgText+s.Title, w) + reset)
	// status bar
	left, right := "", ""
	if s.Status != nil {
		left, right = s.Status()
	}
	if s.Hint != "" {
		right = s.Hint + "   " + right
	}
	rw := Len(right)
	b.WriteString(s.at(0, h-1) + bgBar + fgText + " " + fit(left, w-rw-3) + " " + right + " " + reset)
	s.out.WriteString(b.String())
}

// box draws a framed dialog and returns its inner area.
func (s *Screen) box(bw, bh int, title string, bg string) (x0, y0, iw int) {
	w, h := s.Size()
	if bw > w-4 {
		bw = w - 4
	}
	if bh > h-4 {
		bh = h - 4
	}
	x := (w - bw) / 2
	y := (h - bh) / 2
	var b strings.Builder
	b.WriteString(bg + fgAcc)
	b.WriteString(s.at(x, y) + "┌" + strings.Repeat("─", bw-2) + "┐")
	if title != "" {
		t := " " + title + " "
		b.WriteString(s.at(x+3, y) + bold + fgText + t + reset + bg + fgAcc)
	}
	for i := 1; i < bh-1; i++ {
		b.WriteString(s.at(x, y+i) + "│" + bg + strings.Repeat(" ", bw-2) + fgAcc + "│")
	}
	b.WriteString(s.at(x, y+bh-1) + "└" + strings.Repeat("─", bw-2) + "┘")
	// shadow
	b.WriteString("\x1b[48;5;16m")
	for i := 1; i <= bh; i++ {
		b.WriteString(s.at(x+bw, y+i) + "  ")
	}
	b.WriteString(s.at(x+2, y+bh) + strings.Repeat(" ", bw))
	b.WriteString(reset)
	s.out.WriteString(b.String())
	return x + 2, y + 1, bw - 4
}

func (s *Screen) text(x, y int, style, t string, n int) {
	s.out.WriteString(s.at(x, y) + style + fit(t, n) + reset)
}

func (s *Screen) flush() { s.out.Flush() }

// ---------- components ----------

// Item is a menu entry.
type Item struct {
	Label string
	Desc  string
}

// Menu shows a list of choices; returns the index or -1 for Esc.
func (s *Screen) Menu(title string, lines []string, items []Item, sel int) int {
	s.Hint = "↑↓ wählen · Enter OK · Esc zurück"
	defer func() { s.Hint = "" }()
	for {
		s.mu.Lock()
		s.frame()
		bw := 64
		for _, l := range lines {
			if n := utf8.RuneCountInString(l) + 6; n > bw {
				bw = n
			}
		}
		bh := len(lines) + len(items)*2 + 5
		x, y, iw := s.box(bw, bh, title, bgBox)
		for i, l := range lines {
			s.text(x, y+i, bgBox+fgDim, l, iw)
		}
		y += len(lines) + 1
		for i, it := range items {
			style := bgBox + fgText
			mark := "  "
			if i == sel {
				style = bgSel + fgText + bold
				mark = "► "
			}
			s.text(x, y+i*2, style, fmt.Sprintf(" %s%d  %s", mark, i+1, it.Label), iw)
			if it.Desc != "" {
				s.text(x, y+i*2+1, bgBox+fgDim, "        "+it.Desc, iw)
			}
		}
		s.flush()
		s.mu.Unlock()
		k, ok := s.ReadKey(2 * time.Second) // redraw for status updates
		if !ok {
			continue
		}
		switch k.Code {
		case KeyUp:
			sel = (sel - 1 + len(items)) % len(items)
		case KeyDown, KeyTab:
			sel = (sel + 1) % len(items)
		case KeyEnter:
			return sel
		case KeyEsc:
			return -1
		case KeyRune:
			if k.Rune >= '1' && int(k.Rune-'0') <= len(items) {
				return int(k.Rune - '1')
			}
		}
	}
}

// Input asks for one line of text. hidden shows "*" (passwords).
func (s *Screen) Input(title string, lines []string, label, def string, hidden bool) (string, bool) {
	s.Hint = "Enter OK · Esc zurück"
	defer func() { s.Hint = "" }()
	val := []rune(def)
	for {
		s.mu.Lock()
		s.frame()
		x, y, iw := s.box(70, len(lines)+7, title, bgBox)
		for i, l := range lines {
			s.text(x, y+i, bgBox+fgDim, l, iw)
		}
		y += len(lines) + 1
		s.text(x, y, bgBox+fgText+bold, label, iw)
		shown := string(val)
		if hidden {
			shown = strings.Repeat("*", len(val))
		}
		fw := iw
		r := []rune(shown)
		if len(r) > fw-2 {
			r = r[len(r)-(fw-2):]
		}
		s.text(x, y+2, "\x1b[48;5;255m\x1b[38;5;16m", " "+string(r)+"█", fw)
		s.flush()
		s.mu.Unlock()
		k, ok := s.ReadKey(2 * time.Second)
		if !ok {
			continue
		}
		switch k.Code {
		case KeyEnter:
			return strings.TrimSpace(string(val)), true
		case KeyEsc:
			return "", false
		case KeyBackspace:
			if len(val) > 0 {
				val = val[:len(val)-1]
			}
		case KeyRune:
			if k.Rune >= 32 {
				val = append(val, k.Rune)
			}
		}
	}
}

// Select shows a table and returns the chosen row or -1.
func (s *Screen) Select(title string, lines []string, header []string, rows [][]string, widths []int) int {
	s.Hint = "↑↓ wählen · Enter OK · Esc zurück"
	defer func() { s.Hint = "" }()
	sel, top := 0, 0
	for {
		s.mu.Lock()
		s.frame()
		_, sh := s.Size()
		total := 0
		for _, w := range widths {
			total += w + 2
		}
		visible := len(rows)
		if max := sh - 10 - len(lines); visible > max {
			visible = max
		}
		if visible < 1 {
			visible = 1
		}
		if sel < top {
			top = sel
		}
		if sel >= top+visible {
			top = sel - visible + 1
		}
		x, y, iw := s.box(total+6, visible+len(lines)+6, title, bgBox)
		for i, l := range lines {
			s.text(x, y+i, bgBox+fgDim, l, iw)
		}
		y += len(lines) + 1
		row := func(cells []string) string {
			var b strings.Builder
			for i, c := range cells {
				if i < len(widths) {
					b.WriteString(fit(c, widths[i]) + "  ")
				}
			}
			return b.String()
		}
		s.text(x, y, bgBox+fgAcc+bold, " "+row(header), iw)
		for i := 0; i < visible && top+i < len(rows); i++ {
			style := bgBox + fgText
			if top+i == sel {
				style = bgSel + fgText + bold
			}
			s.text(x, y+1+i, style, " "+row(rows[top+i]), iw)
		}
		if len(rows) > visible {
			s.text(x, y+1+visible, bgBox+fgDim, fmt.Sprintf(" %d–%d von %d", top+1, top+visible, len(rows)), iw)
		}
		s.flush()
		s.mu.Unlock()
		k, ok := s.ReadKey(2 * time.Second)
		if !ok {
			continue
		}
		switch k.Code {
		case KeyUp:
			if sel > 0 {
				sel--
			}
		case KeyDown:
			if sel < len(rows)-1 {
				sel++
			}
		case KeyPgUp:
			sel = max(0, sel-visible)
		case KeyPgDown:
			sel = min(len(rows)-1, sel+visible)
		case KeyHome:
			sel = 0
		case KeyEnd:
			sel = len(rows) - 1
		case KeyEnter:
			return sel
		case KeyEsc:
			return -1
		}
	}
}

// Confirm shows a red warning and requires typing word.
func (s *Screen) Confirm(title string, lines []string, word string) bool {
	s.Hint = "Enter bestätigen · Esc abbrechen"
	defer func() { s.Hint = "" }()
	var val []rune
	for {
		s.mu.Lock()
		s.frame()
		x, y, iw := s.box(74, len(lines)+8, "! "+title+" !", bgBad)
		for i, l := range lines {
			s.text(x, y+i, bgBad+fgText, l, iw)
		}
		y += len(lines) + 1
		s.text(x, y, bgBad+fgText+bold, fmt.Sprintf("Zum Fortfahren %s eingeben:", word), iw)
		s.text(x, y+2, "\x1b[48;5;255m\x1b[38;5;16m", " "+string(val)+"█", 20)
		s.flush()
		s.mu.Unlock()
		k, ok := s.ReadKey(2 * time.Second)
		if !ok {
			continue
		}
		switch k.Code {
		case KeyEnter:
			return strings.TrimSpace(string(val)) == word
		case KeyEsc:
			return false
		case KeyBackspace:
			if len(val) > 0 {
				val = val[:len(val)-1]
			}
		case KeyRune:
			if len(val) < 16 {
				val = append(val, k.Rune)
			}
		}
	}
}

// Message shows text until a key is pressed (or d elapsed, if > 0).
func (s *Screen) Message(title string, lines []string, bad bool, d time.Duration) {
	s.Hint = "beliebige Taste"
	defer func() { s.Hint = "" }()
	bg := bgBox
	if bad {
		bg = bgBad
	}
	deadline := time.Now().Add(d)
	for {
		s.mu.Lock()
		s.frame()
		bw := 60
		for _, l := range lines {
			if n := utf8.RuneCountInString(l) + 6; n > bw {
				bw = n
			}
		}
		x, y, iw := s.box(bw, len(lines)+4, title, bg)
		for i, l := range lines {
			s.text(x, y+i+1, bg+fgText, l, iw)
		}
		s.flush()
		s.mu.Unlock()
		wait := 2 * time.Second
		if d > 0 {
			if left := time.Until(deadline); left <= 0 {
				return
			} else if left < wait {
				wait = left
			}
		}
		if _, ok := s.ReadKey(wait); ok {
			return
		}
	}
}

// Progress is a non-blocking progress view.
type Progress struct {
	s     *Screen
	title string
	mu    sync.Mutex
	pct   float64
	lines []string // info lines above the bar
	log   []string
	stop  chan struct{}
}

func (s *Screen) Progress(title string) *Progress {
	p := &Progress{s: s, title: title, stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			p.draw()
			select {
			case <-t.C:
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *Progress) Set(pct float64, lines ...string) {
	p.mu.Lock()
	p.pct, p.lines = pct, lines
	p.mu.Unlock()
}

func (p *Progress) Log(line string) {
	p.mu.Lock()
	p.log = append(p.log, line)
	if len(p.log) > 200 {
		p.log = p.log[len(p.log)-200:]
	}
	p.mu.Unlock()
}

func (p *Progress) Stop() { close(p.stop) }

func (p *Progress) draw() {
	p.mu.Lock()
	pct, lines, log := p.pct, append([]string(nil), p.lines...), append([]string(nil), p.log...)
	p.mu.Unlock()
	s := p.s
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frame()
	w, h := s.Size()
	bw := w - 8
	if bw > 100 {
		bw = 100
	}
	logLines := h - 16
	if logLines < 3 {
		logLines = 3
	}
	x, y, iw := s.box(bw, 10+logLines, p.title, bgBox)
	for i, l := range lines {
		if i < 3 {
			s.text(x, y+i, bgBox+fgText, l, iw)
		}
	}
	barW := iw - 8
	filled := int(float64(barW) * pct / 100)
	if filled > barW {
		filled = barW
	}
	s.out.WriteString(s.at(x, y+4) + bgBox + fgOK + strings.Repeat("█", filled) + fgDim + strings.Repeat("░", barW-filled) + fgText + bold + fmt.Sprintf(" %5.1f%%", pct) + reset)
	s.text(x, y+6, bgBox+fgAcc+bold, "Protokoll", iw)
	start := len(log) - logLines
	if start < 0 {
		start = 0
	}
	for i := 0; i < logLines; i++ {
		t := ""
		if start+i < len(log) {
			t = log[start+i]
		}
		style := bgBox + fgDim
		if strings.Contains(t, "Fehler") || strings.Contains(t, "fehlgeschlagen") {
			style = bgBox + fgBad
		}
		s.text(x, y+7+i, style, t, iw)
	}
	s.flush()
}

// Colour helpers for callers building status lines.
func OK(t string) string   { return fgOK + t + fgText }
func Bad(t string) string  { return fgBad + t + fgText }
func Warn(t string) string { return fgWarn + t + fgText }

// Len counts the visible characters (without escape sequences).
func Len(t string) int {
	n, esc := 0, false
	for _, r := range t {
		switch {
		case r == 0x1b:
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			n++
		}
	}
	return n
}
