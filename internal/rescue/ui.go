package rescue

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"bastion/internal/proto"
	"bastion/internal/tui"
)

// ui is what the rescue program shows: a full-screen text UI on a real
// console, or plain lines when input is redirected (tests, serial
// consoles).
type ui interface {
	menu(title string, lines []string, items []tui.Item) int
	input(title string, lines []string, label, def string) (string, bool)
	password(title, label string) (string, bool)
	selectRow(title string, lines []string, header []string, rows [][]string, widths []int) int
	confirm(title string, lines []string, word string) bool
	message(title string, lines []string, bad bool, d time.Duration)
	progress(title string) progressView
	close()
}

type progressView interface {
	set(pct float64, lines ...string)
	log(line string)
	stop()
}

// logging: say() writes to the active view
var (
	logMu   sync.Mutex
	logHook func(string)
	recent  []string
)

func say(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	logMu.Lock()
	recent = append(recent, line)
	if len(recent) > 50 {
		recent = recent[len(recent)-50:]
	}
	hook := logHook
	logMu.Unlock()
	if hook != nil {
		hook(line)
	} else {
		fmt.Println(line)
	}
}

func lastLog() string {
	logMu.Lock()
	defer logMu.Unlock()
	if len(recent) == 0 {
		return ""
	}
	return recent[len(recent)-1]
}

func setLogHook(f func(string)) {
	logMu.Lock()
	logHook = f
	logMu.Unlock()
}

// ---------- full screen ----------

type screenUI struct {
	sc *tui.Screen
}

func newUI(s *session) ui {
	sc, err := tui.Open("Wiederherstellung " + proto.Version)
	if err != nil {
		return &lineUI{s: s}
	}
	sc.Status = s.statusBar
	setLogHook(func(string) {}) // keep the screen clean; lines go to recent and progress views
	return &screenUI{sc: sc}
}

func (u *screenUI) menu(title string, lines []string, items []tui.Item) int {
	return u.sc.Menu(title, lines, items, 0)
}
func (u *screenUI) input(title string, lines []string, label, def string) (string, bool) {
	return u.sc.Input(title, lines, label, def, false)
}
func (u *screenUI) password(title, label string) (string, bool) {
	return u.sc.Input(title, nil, label, "", true)
}
func (u *screenUI) selectRow(title string, lines []string, header []string, rows [][]string, widths []int) int {
	return u.sc.Select(title, lines, header, rows, widths)
}
func (u *screenUI) confirm(title string, lines []string, word string) bool {
	return u.sc.Confirm(title, lines, word)
}
func (u *screenUI) message(title string, lines []string, bad bool, d time.Duration) {
	u.sc.Message(title, lines, bad, d)
}
func (u *screenUI) progress(title string) progressView {
	p := u.sc.Progress(title)
	setLogHook(p.Log)
	return &screenProgress{p: p}
}
func (u *screenUI) close() { u.sc.Close(); setLogHook(nil) }

type screenProgress struct{ p *tui.Progress }

func (p *screenProgress) set(pct float64, lines ...string) { p.p.Set(pct, lines...) }
func (p *screenProgress) log(line string)                  { p.p.Log(line) }
func (p *screenProgress) stop() {
	p.p.Stop()
	setLogHook(func(string) {})
}

// ---------- plain lines ----------

type lineUI struct{ s *session }

func (u *lineUI) header(title string, lines []string) {
	fmt.Printf("\n== %s ==\n", title)
	for _, l := range lines {
		fmt.Println("  " + tuiPlain(l))
	}
}

func (u *lineUI) menu(title string, lines []string, items []tui.Item) int {
	u.header(title, lines)
	for i, it := range items {
		fmt.Printf("  %d) %s\n", i+1, it.Label)
	}
	return u.number("Auswahl:", len(items))
}

func (u *lineUI) number(q string, max int) int {
	for {
		v := u.s.ask(q)
		if v == "" {
			return -1
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= max {
			return n - 1
		}
		fmt.Printf("  Bitte eine Zahl von 1 bis %d eingeben (Enter = zurück).\n", max)
	}
}

func (u *lineUI) input(title string, lines []string, label, def string) (string, bool) {
	if len(lines) > 0 {
		u.header(title, lines)
	}
	q := label
	if def != "" {
		q += " [" + def + "]"
	}
	v := u.s.ask(q)
	if v == "" {
		v = def
	}
	return v, v != ""
}

func (u *lineUI) password(title, label string) (string, bool) {
	v := u.s.askHidden(label)
	return v, v != ""
}

func (u *lineUI) selectRow(title string, lines []string, header []string, rows [][]string, widths []int) int {
	u.header(title, lines)
	for i, r := range rows {
		fmt.Printf("  [%d] %s\n", i+1, strings.Join(r, "  "))
	}
	return u.number("Auswahl (Enter = zurück):", len(rows))
}

func (u *lineUI) confirm(title string, lines []string, word string) bool {
	u.header(title, lines)
	return u.s.ask(fmt.Sprintf("Zum Fortfahren %s eingeben:", word)) == word
}

func (u *lineUI) message(title string, lines []string, bad bool, d time.Duration) {
	u.header(title, lines)
	if d > 0 {
		time.Sleep(d)
	}
}

func (u *lineUI) progress(title string) progressView {
	fmt.Printf("\n== %s ==\n", title)
	return &lineProgress{}
}

func (u *lineUI) close() {}

type lineProgress struct {
	last time.Time
}

func (p *lineProgress) set(pct float64, lines ...string) {
	if time.Since(p.last) < 5*time.Second && pct < 100 {
		return
	}
	p.last = time.Now()
	fmt.Printf("%s %5.1f %% %s\n", time.Now().Format("15:04:05"), pct, strings.Join(lines, " · "))
}
func (p *lineProgress) log(line string) { fmt.Println(line) }
func (p *lineProgress) stop()           {}

// tuiPlain removes colour sequences for line mode.
func tuiPlain(t string) string {
	var b strings.Builder
	esc := false
	for _, r := range t {
		switch {
		case r == 0x1b:
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			b.WriteRune(r)
		}
	}
	return b.String()
}

const byteOrderMark = "\xef\xbb\xbf"

func (s *session) ask(q string) string {
	fmt.Print(q + " ")
	if s.in == nil {
		s.in = bufio.NewReader(os.Stdin)
	}
	l, err := s.in.ReadString('\n')
	if err != nil && l == "" {
		fmt.Println()
		say("Keine Eingabe mehr möglich (%v), Abbruch.", err)
		os.Exit(1)
	}
	return strings.TrimSpace(strings.TrimPrefix(l, byteOrderMark))
}

// askHidden reads a line without echoing it (passwords).
func (s *session) askHidden(q string) string {
	restore := hideInput()
	v := s.ask(q)
	restore()
	fmt.Println()
	return v
}
