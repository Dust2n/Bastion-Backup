// Package rescue runs inside the rescue environment (WinPE or the Linux
// rescue system, booted from the local disk, USB/ISO or PXE). It restores
// a full image onto a disk, scaling partitions to the disk size, and makes
// the system bootable again.
package rescue

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"bastion/internal/client"
	"bastion/internal/fsx"
	"bastion/internal/layout"
	"bastion/internal/proto"
	"bastion/internal/repo"
	"bastion/internal/transfer"
)

// Config is baked into the rescue media (server address for USB/PXE).
type Config struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
}

// TargetDisk is a disk visible in the rescue environment.
type TargetDisk struct {
	Number int
	Path   string
	Size   int64
	Serial string
	Model  string
}

// Mounted maps snapshot volume keys to paths to restore into.
type Mounted map[string]string

type session struct {
	c      *client.Client
	ticket proto.RestoreTicket
	info   proto.RestoreInfo
	auto   bool
	in     *bufio.Reader
	start  time.Time
	ui     ui
	prog   progressView
	srv    serverState
	// transfer speed
	lastBytes int64
	lastAt    time.Time
	rate      float64
}

// progress updates the screen and reports to the server.
func (s *session) progress(pct float64, msg string, log ...string) {
	say("%s", msg)
	if s.prog != nil {
		s.prog.set(pct, msg)
	}
	s.report(pct, msg, log...)
}

func (s *session) report(pct float64, msg string, log ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/progress", s.ticket.JobID),
		proto.Progress{Percent: pct, Message: msg, Log: append(log, time.Now().Format("15:04:05 ")+msg)}, nil)
}

// speed returns bytes/s (moving average) for the bytes restored so far.
func (s *session) speed(bytes int64) float64 {
	now := time.Now()
	if !s.lastAt.IsZero() {
		if dt := now.Sub(s.lastAt).Seconds(); dt > 0.5 {
			cur := float64(bytes-s.lastBytes) / dt
			if s.rate == 0 {
				s.rate = cur
			} else {
				s.rate = 0.7*s.rate + 0.3*cur
			}
		}
	}
	s.lastBytes, s.lastAt = bytes, now
	return s.rate
}

func (s *session) finish(ok bool, msg string) {
	say("%s", msg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s.c.Result(ctx, s.ticket.JobID, proto.JobResult{Success: ok, Message: msg})
}

// Run is the rescue entry point. auto: started by a ticket on the local
// disk, no questions are asked.
func Run(auto bool) error {
	fsx.EnablePrivileges()
	setKeyboard("de")
	s := &session{auto: auto, start: time.Now()}
	s.ui = newUI(s)
	defer s.ui.close()
	say("Bastion Wiederherstellung %s", proto.Version)
	prepareEnvironment()

	t, err := findTicket()
	if err != nil {
		say("Kein Wiederherstellungsauftrag auf den Festplatten gefunden (%v)", err)
	}
	if t == nil {
		s.auto = false
		if t = s.menu(); t == nil {
			return nil
		}
	} else {
		s.srv.cfg = Config{Server: t.Server, Fingerprint: t.Fingerprint}
		s.startStatusChecker()
	}
	s.ticket = *t
	s.c = client.New(t.Server, t.Fingerprint, "restore:"+t.Token)
	if s.auto {
		s.autoNetwork(t.Network)
	}

	ctx := context.Background()
	for {
		p := s.ui.progress("Verbinde mit dem Server")
		for attempt := 0; attempt < 10; attempt++ {
			if _, err = s.c.JSON(ctx, "GET", "/api/rescue/info", nil, &s.info); err == nil {
				break
			}
			say("Server %s nicht erreichbar: %v", t.Server, err)
			p.set(float64(attempt)*10, fmt.Sprintf("Versuch %d von 10 · Netz: %s", attempt+1, ipSummary()))
			time.Sleep(6 * time.Second)
		}
		p.stop()
		if err == nil {
			break
		}
		if !s.unreachable() {
			if s.auto {
				s.ui.close()
				reboot()
			}
			return err
		}
	}
	err = s.restore(ctx)
	if s.prog != nil {
		s.prog.stop()
		s.prog = nil
	}
	if err != nil {
		s.finish(false, "Wiederherstellung fehlgeschlagen: "+err.Error())
		if errors.Is(err, errBeforeWipe) {
			if s.auto {
				s.ui.message("Wiederherstellung abgebrochen", []string{err.Error(), "", "Die Festplatte wurde nicht verändert.", "Neustart in das bisherige System in 30 Sekunden."}, true, 30*time.Second)
				s.ui.close()
				reboot()
			}
			s.ui.message("Wiederherstellung abgebrochen", []string{err.Error(), "", "Die Festplatte wurde nicht verändert."}, true, 0)
			return err
		}
		s.ui.message("Wiederherstellung fehlgeschlagen", []string{err.Error(), "",
			"Die Festplatte ist unvollständig. Bitte den Auftrag erneut starten", "(Weboberfläche oder dieses Rettungsmedium)."}, true, 0)
		if s.auto {
			s.ui.close()
			reboot()
		}
		return err
	}
	took := time.Since(s.start).Round(time.Second)
	s.finish(true, fmt.Sprintf("Image erfolgreich wiederhergestellt (%s)", took))
	lines := []string{fmt.Sprintf("%s ist wiederhergestellt (Dauer %s).", s.info.Snapshot.Hostname, took), ""}
	if s.auto {
		lines = append(lines, "Neustart in 15 Sekunden …")
		s.ui.message("Fertig", lines, false, 15*time.Second)
	} else {
		lines = append(lines, "Tipp: Ist der Rechner ein Klon eines laufenden Systems,", "vor dem Neustart die Netzwerkverbindung trennen (gleiche IP/Name).", "", "Taste drücken für Neustart.")
		s.ui.message("Fertig", lines, false, 0)
	}
	s.ui.close()
	reboot()
	return nil
}

var errBeforeWipe = errors.New("vor dem Löschen der Festplatte abgebrochen")

type beforeWipe struct{ err error }

func (b beforeWipe) Error() string        { return b.err.Error() }
func (b beforeWipe) Is(target error) bool { return target == errBeforeWipe }
func (b beforeWipe) Unwrap() error        { return b.err }

// sourceDisk returns the disk of the snapshot that holds the system volume.
func sourceDisk(m *layout.Machine) (*layout.Disk, error) {
	if m == nil {
		return nil, errors.New("das Backup enthält kein Datenträger-Layout (kein Image-Backup)")
	}
	for i := range m.Disks {
		d := &m.Disks[i]
		for _, p := range d.Partitions {
			if p.Role == layout.RoleSystem && p.Volume != "" {
				return d, nil
			}
			for _, c := range p.Children {
				if c.Role == layout.RoleSystem && c.Volume != "" {
					return d, nil
				}
			}
		}
	}
	return nil, errors.New("System-Volume nicht im Backup gefunden")
}

func (s *session) chooseDisk(disks []TargetDisk, need int64) (*TargetDisk, error) {
	t := s.ticket
	for i := range disks {
		if t.DiskSerial != "" && strings.EqualFold(strings.TrimSpace(disks[i].Serial), strings.TrimSpace(t.DiskSerial)) {
			return &disks[i], nil
		}
	}
	if s.auto {
		var fit []TargetDisk
		for _, d := range disks {
			if d.Size >= need {
				fit = append(fit, d)
			}
		}
		if len(fit) == 1 {
			return &fit[0], nil
		}
		return nil, fmt.Errorf("Zieldatenträger nicht eindeutig (Seriennummer %q nicht gefunden, %d passende Datenträger)", t.DiskSerial, len(fit))
	}
	rows := make([][]string, len(disks))
	for i, d := range disks {
		ok := "passt"
		if d.Size < need {
			ok = "zu klein"
		}
		serial := d.Serial
		if serial == "" {
			serial = "–"
		}
		rows[i] = []string{fmt.Sprintf("%d", d.Number), layout.Human(d.Size), d.Model, serial, ok}
	}
	for {
		n := s.ui.selectRow("Zieldatenträger wählen", []string{
			"Auf diesen Datenträger wird das Image geschrieben.",
			fmt.Sprintf("Benötigt mindestens %s.", layout.Human(need)),
		}, []string{"Nr.", "Größe", "Modell", "Seriennummer", "Status"}, rows, []int{4, 10, 28, 18, 8})
		if n < 0 {
			return nil, errors.New("vom Benutzer abgebrochen")
		}
		if disks[n].Size < need {
			s.ui.message("Zu klein", []string{fmt.Sprintf("Datenträger %d hat %s, benötigt werden mindestens %s.", disks[n].Number, layout.Human(disks[n].Size), layout.Human(need))}, true, 0)
			continue
		}
		return &disks[n], nil
	}
}

// planLines draws the partition plan for the confirmation dialog.
func planLines(plan []layout.Planned, diskSize int64) []string {
	var out []string
	for _, p := range plan {
		name := p.Source.Volume
		if name == "" {
			name = map[string]string{layout.RoleESP: "EFI", layout.RoleMSR: "MSR", layout.RoleBoot: "Boot", layout.RoleSwap: "Swap", layout.RoleBIOSBoot: "BIOS-Boot", layout.RoleLVM: "LVM"}[p.Source.Role]
		}
		n := int(float64(p.Size) / float64(diskSize) * 30)
		if n < 1 {
			n = 1
		}
		used := ""
		if p.Source.Used > 0 {
			used = "belegt " + layout.Human(p.Source.Used)
		}
		out = append(out, fmt.Sprintf("  %-9s %-6s %10s  %s %s", name, p.Source.FS, layout.Human(p.Size), strings.Repeat("█", n), used))
	}
	return out
}

func (s *session) restore(ctx context.Context) error {
	snap := s.info.Snapshot
	if snap == nil {
		return beforeWipe{errors.New("kein Backup im Auftrag")}
	}
	say("Backup %s von %s (%s)", snap.ID, snap.Hostname, snap.Time.Local().Format("02.01.2006 15:04"))
	src, err := sourceDisk(snap.Layout)
	if err != nil {
		return beforeWipe{err}
	}
	adapted := adaptToFirmware(layout.WithBackupSizes(*src, volumeBytes(snap)), firmware(), snap.OS)
	disks, err := listDisks()
	if err != nil {
		return beforeWipe{fmt.Errorf("Datenträger auflisten: %w", err)}
	}
	target, err := s.chooseDisk(disks, layout.Required(adapted))
	if err != nil {
		return beforeWipe{err}
	}
	plan, err := layout.Scale(adapted, target.Size)
	if err != nil {
		return beforeWipe{err}
	}
	pl := planLines(plan, target.Size)
	say("Partitionsplan für Datenträger %d (%s):", target.Number, layout.Human(target.Size))
	for _, l := range pl {
		say("%s", l)
	}

	// Make sure the server delivers data before the disk is touched.
	for _, v := range snap.Volumes {
		if len(v.Tree) > 0 {
			if _, err := s.c.GetChunk(ctx, v.Tree[0]); err != nil {
				return beforeWipe{fmt.Errorf("Backup-Daten nicht abrufbar: %w", err)}
			}
		}
	}
	var total int64
	for _, v := range snap.Volumes {
		total += v.Bytes
	}
	if !s.auto {
		lines := []string{
			fmt.Sprintf("Backup:  %s vom %s (%s)", snap.Hostname, snap.Time.Local().Format("02.01.2006 15:04"), layout.Human(total)),
			fmt.Sprintf("Ziel:    Datenträger %d · %s · %s", target.Number, target.Model, layout.Human(target.Size)),
			"",
			"Neue Aufteilung:",
		}
		lines = append(lines, pl...)
		lines = append(lines, "", fmt.Sprintf("ALLE DATEN auf Datenträger %d werden gelöscht!", target.Number))
		if !s.ui.confirm("Wiederherstellung starten", lines, "JA") {
			return beforeWipe{errors.New("vom Benutzer abgebrochen")}
		}
	}

	s.prog = s.ui.progress(fmt.Sprintf("Wiederherstellung von %s", snap.Hostname))
	s.progress(2, fmt.Sprintf("Partitioniere Datenträger %d", target.Number))
	mounted, err := partition(target, adapted, plan, snap.OS)
	if err != nil {
		return fmt.Errorf("Partitionieren: %w", err)
	}
	defer unmountAll(mounted)

	var done int64
	vols := append([]repo.Volume(nil), snap.Volumes...)
	sort.Slice(vols, func(i, j int) bool { return len(vols[i].Key) < len(vols[j].Key) }) // "/" before "/boot"
	for _, v := range vols {
		dst, ok := mounted[v.Key]
		if !ok {
			say("Volume %s wird übersprungen (nicht auf dem Systemdatenträger)", v.Key)
			continue
		}
		s.progress(pct(done, total), fmt.Sprintf("Stelle %s wieder her (%s)", v.Key, layout.Human(v.Bytes)))
		w := fsx.NewWriter(dst)
		base := done
		last := time.Now()
		lastScreen := time.Now()
		st, err := transfer.RestoreVolume(ctx, func(ctx context.Context, id repo.ID) ([]byte, error) { return s.c.GetChunk(ctx, id) },
			v, w, nil, func(st transfer.RestoreStats) {
				if time.Since(lastScreen) < time.Second {
					return
				}
				lastScreen = time.Now()
				all := base + st.Bytes
				rate := s.speed(all)
				eta := "–"
				if rate > 0 && total > all {
					eta = (time.Duration(float64(total-all)/rate) * time.Second).Round(time.Second).String()
				}
				info := fmt.Sprintf("Volume %s · %d Dateien · %s von %s", v.Key, st.Files, layout.Human(all), layout.Human(total))
				speed := fmt.Sprintf("Tempo %s/s · Restzeit %s · läuft seit %s", layout.Human(int64(rate)), eta, time.Since(s.start).Round(time.Second))
				if s.prog != nil {
					s.prog.set(pct(all, total), info, speed)
				}
				if time.Since(last) > 5*time.Second {
					last = time.Now()
					s.report(pct(all, total), fmt.Sprintf("%s: %d Dateien, %s · %s/s", v.Key, st.Files, layout.Human(st.Bytes), layout.Human(int64(rate))))
				}
			})
		if err != nil {
			return fmt.Errorf("%s: %w", v.Key, err)
		}
		for i, e := range st.Errors {
			if i < 20 {
				say("  Fehler: %s", e)
			}
		}
		if len(st.Errors) > 0 {
			s.progress(pct(base+st.Bytes, total), fmt.Sprintf("%s: %d Dateien konnten nicht geschrieben werden", v.Key, len(st.Errors)), st.Errors...)
		}
		done += v.Bytes
	}

	s.progress(97, "Mache das System startfähig")
	if err := makeBootable(target, adapted, plan, mounted, snap); err != nil {
		return fmt.Errorf("Bootloader: %w", err)
	}
	return nil
}

func volumeBytes(snap *repo.Snapshot) map[string]int64 {
	m := map[string]int64{}
	for _, v := range snap.Volumes {
		m[v.Key] = v.Bytes
	}
	return m
}

func pct(done, total int64) float64 {
	if total <= 0 {
		return 50
	}
	return 3 + 93*float64(done)/float64(total)
}

// adaptToFirmware makes a Windows layout bootable on the firmware of the
// machine we run on (e.g. old BIOS/MBR backup onto a UEFI machine).
func adaptToFirmware(d layout.Disk, fw, osName string) layout.Disk {
	if osName != "windows" {
		return d
	}
	var parts []layout.Partition
	hasESP := false
	for _, p := range d.Partitions {
		if p.Role == layout.RoleESP {
			hasESP = true
		}
	}
	if fw == "uefi" {
		d.Style = "gpt"
		if !hasESP {
			parts = append(parts,
				layout.Partition{Offset: -2, Size: 260 * layout.MiB, Role: layout.RoleESP, FS: "fat32"},
				layout.Partition{Offset: -1, Size: 16 * layout.MiB, Role: layout.RoleMSR})
		}
		for _, p := range d.Partitions {
			if p.Role == layout.RoleBoot && p.Volume == "" {
				continue // MBR "System Reserved" is not needed with UEFI
			}
			parts = append(parts, p)
		}
	} else {
		d.Style = "mbr"
		for _, p := range d.Partitions {
			if p.Role == layout.RoleESP || p.Role == layout.RoleMSR {
				continue
			}
			parts = append(parts, p)
		}
	}
	d.Partitions = parts
	return d
}

func loadConfig() Config {
	var c Config
	for _, p := range configPaths() {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &c)
			break
		}
	}
	return c
}
