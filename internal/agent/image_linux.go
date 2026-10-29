//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"bastion/internal/proto"
)

// Image restore while Linux is running: the rescue kernel and initramfs
// are put into /boot/bastion and booted once via grub-reboot. The rescue
// system finds the ticket on the disk, restores the full image and reboots.

const (
	stageDir  = "/boot/bastion"
	grubEntry = "/etc/grub.d/42_bastion"
	entryName = "Bastion Restore"
)

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func firstOK(cmds ...[]string) (string, error) {
	var err error
	for _, c := range cmds {
		if _, lerr := exec.LookPath(c[0]); lerr != nil {
			continue
		}
		var out string
		if out, err = run(c[0], c[1:]...); err == nil {
			return out, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("keines der Programme gefunden: %v", cmds)
	}
	return "", err
}

func (a *Agent) download(ctx context.Context, path, dst string) error {
	resp, err := a.c.Do(ctx, "GET", path, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func updateGrub() error {
	_, err := firstOK([]string{"update-grub"}, []string{"grub2-mkconfig", "-o", "/boot/grub2/grub.cfg"}, []string{"grub-mkconfig", "-o", "/boot/grub/grub.cfg"})
	return err
}

func (a *Agent) stageImageRestore(ctx context.Context, rep *reporter, jobID int64, p proto.RestoreImageParams) proto.JobResult {
	a.removeBootEntry()
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	fail := func(err error) proto.JobResult {
		a.removeBootEntry()
		return proto.JobResult{Message: err.Error()}
	}
	rep.Log("Lade Rettungssystem vom Server")
	for _, f := range [][2]string{{"rescue/linux-vmlinuz", "vmlinuz"}, {"rescue/linux-initrd.img", "initrd.img"}} {
		if err := a.download(ctx, "/api/agent/download/"+f[0], filepath.Join(stageDir, f[1])); err != nil {
			return fail(fmt.Errorf("Rettungssystem nicht verfügbar (auf dem Server mit rescue/linux/build.sh erstellen): %w", err))
		}
	}
	t := proto.RestoreTicket{Server: a.cfg.Server, Fingerprint: a.cfg.Fingerprint, Token: p.Token, JobID: jobID,
		SnapshotID: p.SnapshotID, DiskNumber: p.DiskNumber, Network: netConfigs()}
	if a.layout != nil {
		for _, d := range a.layout.Disks {
			if (p.DiskNumber < 0 && d.System) || d.Number == p.DiskNumber {
				t.DiskSerial, t.DiskSize = d.Serial, d.Size
			}
		}
	}
	tb, _ := json.Marshal(t)
	if err := os.WriteFile(filepath.Join(stageDir, "ticket.json"), tb, 0o600); err != nil {
		return fail(err)
	}

	rel, err := firstOK([]string{"grub-mkrelpath", stageDir}, []string{"grub2-mkrelpath", stageDir})
	if err != nil {
		return fail(fmt.Errorf("GRUB nicht gefunden: %w", err))
	}
	script := fmt.Sprintf(`#!/bin/sh
cat <<'EOF'
menuentry '%[1]s' --id bastion-restore {
	insmod part_gpt
	insmod part_msdos
	insmod ext2
	insmod xfs
	insmod btrfs
	search --no-floppy --file --set=root %[2]s/vmlinuz
	linux %[2]s/vmlinuz bastion.auto=1 quiet
	initrd %[2]s/initrd.img
}
EOF
`, entryName, rel)
	if err := os.WriteFile(grubEntry, []byte(script), 0o755); err != nil {
		return fail(err)
	}
	if err := updateGrub(); err != nil {
		return fail(err)
	}
	if _, err := firstOK([]string{"grub-reboot", entryName}, []string{"grub2-reboot", entryName}); err != nil {
		return fail(err)
	}
	os.WriteFile(filepath.Join(stageDir, "job"), []byte(fmt.Sprint(jobID)), 0o600)
	return proto.JobResult{Success: true, Waiting: true,
		Message: fmt.Sprintf("Neustart in das Rettungssystem in %d Sekunden", p.DelaySec)}
}

func (a *Agent) removeBootEntry() {
	if _, err := os.Stat(grubEntry); err == nil {
		os.Remove(grubEntry)
		updateGrub()
	}
	os.RemoveAll(stageDir)
}

func (a *Agent) afterImageRestore(ctx context.Context) {
	b, err := os.ReadFile(filepath.Join(stageDir, "job"))
	if err != nil {
		if _, err := os.Stat(grubEntry); err == nil {
			a.removeBootEntry()
		}
		return
	}
	var job int64
	fmt.Sscan(string(b), &job)
	a.removeBootEntry()
	if job != 0 {
		a.c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/stale", job),
			proto.JobResult{Message: "Rettungssystem wurde nicht ausgeführt oder vor dem Löschen der Festplatte abgebrochen; das alte System läuft weiter"}, nil)
	}
}
