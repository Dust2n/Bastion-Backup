//go:build windows

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"bastion/internal/platform"
	"bastion/internal/proto"
)

// Image restore while Windows is running: the WinPE rescue system is put
// on the local disk as a RAM disk boot entry that is used for the next
// boot only. WinPE then restores the full image and reboots.

func recoveryDir() string { return filepath.Join(os.Getenv("SystemDrive")+`\`, "BastionRecovery") }

type bcdState struct {
	Loader  string `json:"loader"`
	Ramdisk string `json:"ramdisk"`
	JobID   int64  `json:"job_id"`
}

var guidRe = regexp.MustCompile(`\{[0-9a-fA-F-]{36}\}`)

func bcdedit(args ...string) (string, error) {
	cmd := exec.Command("bcdedit.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("bcdedit %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (a *Agent) stageImageRestore(ctx context.Context, rep *reporter, jobID int64, p proto.RestoreImageParams) proto.JobResult {
	dir := recoveryDir()
	a.removeBootEntry()
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	secureDir(dir)
	fail := func(err error) proto.JobResult {
		a.removeBootEntry()
		os.RemoveAll(dir)
		return proto.JobResult{Message: err.Error()}
	}

	rep.Log("Lade Wiederherstellungsumgebung (WinPE) vom Server")
	rep.Set(func(pr *proto.Progress) { pr.Message = "Lade WinPE"; pr.Percent = 5 })
	resp, err := a.c.Do(ctx, "GET", "/api/agent/download/rescue/winpe.wim", nil, "")
	if err != nil {
		return fail(fmt.Errorf("WinPE-Image nicht verfügbar (auf dem Server mit rescue/winpe/build.ps1 erstellen): %w", err))
	}
	wim := filepath.Join(dir, "boot.wim")
	f, err := os.Create(wim)
	if err == nil {
		_, err = io.Copy(f, resp.Body)
		f.Close()
	}
	resp.Body.Close()
	if err != nil {
		return fail(err)
	}

	sdiSrc := ""
	for _, c := range []string{`Boot\DVD\EFI\boot.sdi`, `Boot\DVD\PCAT\boot.sdi`} {
		if _, err := os.Stat(filepath.Join(os.Getenv("SystemRoot"), c)); err == nil {
			sdiSrc = filepath.Join(os.Getenv("SystemRoot"), c)
			break
		}
	}
	if sdiSrc == "" {
		return fail(fmt.Errorf("boot.sdi nicht gefunden"))
	}
	if err := copyFile(sdiSrc, filepath.Join(dir, "boot.sdi")); err != nil {
		return fail(err)
	}

	rep.Log("Binde Netzwerk- und Speichertreiber dieses Computers in WinPE ein")
	rep.Set(func(pr *proto.Progress) { pr.Message = "Treiber einbinden"; pr.Percent = 30 })
	if out, err := platform.PowerShell(driverScript(dir)); err != nil {
		rep.Log("Treiber konnten nicht eingebunden werden (WinPE nutzt seine eigenen): %v", err)
	} else if out != "" {
		rep.Log("%s", out)
	}

	m := a.layout
	if m == nil {
		m, _ = platform.CaptureLayout()
	}
	t := proto.RestoreTicket{Server: a.cfg.Server, Fingerprint: a.cfg.Fingerprint, Token: p.Token, JobID: jobID,
		SnapshotID: p.SnapshotID, DiskNumber: p.DiskNumber, Network: netConfigs()}
	if m != nil {
		for _, d := range m.Disks {
			if (p.DiskNumber < 0 && d.System) || d.Number == p.DiskNumber {
				t.DiskNumber, t.DiskSerial, t.DiskSize = d.Number, d.Serial, d.Size
				break
			}
		}
	}
	tb, _ := json.MarshalIndent(t, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "ticket.json"), tb, 0o600); err != nil {
		return fail(err)
	}

	rep.Log("Richte einmaligen Start der Wiederherstellungsumgebung ein")
	rep.Set(func(pr *proto.Progress) { pr.Message = "Bootmenü einrichten"; pr.Percent = 80 })
	st, err := addBootEntry(dir)
	if err != nil {
		return fail(err)
	}
	st.JobID = jobID
	sb, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dir, "bcd.json"), sb, 0o600)
	rep.Set(func(pr *proto.Progress) { pr.Message = "Neustart"; pr.Percent = 90 })
	return proto.JobResult{Success: true, Waiting: true,
		Message: fmt.Sprintf("Neustart in die Wiederherstellungsumgebung in %d Sekunden", p.DelaySec)}
}

func addBootEntry(dir string) (*bcdState, error) {
	drive := filepath.VolumeName(dir)
	rel := strings.TrimPrefix(dir, drive)
	out, err := bcdedit("/create", "/d", "Bastion Restore", "/device")
	if err != nil {
		return nil, err
	}
	rd := guidRe.FindString(out)
	if rd == "" {
		return nil, fmt.Errorf("bcdedit: keine GUID in %q", out)
	}
	st := &bcdState{Ramdisk: rd}
	steps := [][]string{
		{"/set", rd, "ramdisksdidevice", "partition=" + drive},
		{"/set", rd, "ramdisksdipath", rel + `\boot.sdi`},
	}
	for _, s := range steps {
		if _, err := bcdedit(s...); err != nil {
			bcdedit("/delete", rd)
			return nil, err
		}
	}
	out, err = bcdedit("/create", "/d", "Bastion Restore", "/application", "osloader")
	if err != nil {
		bcdedit("/delete", rd)
		return nil, err
	}
	st.Loader = guidRe.FindString(out)
	winload := `\windows\system32\boot\winload.exe`
	if strings.EqualFold(os.Getenv("firmware_type"), "UEFI") {
		winload = `\windows\system32\boot\winload.efi`
	}
	dev := fmt.Sprintf("ramdisk=[%s]%s\\boot.wim,%s", drive, rel, rd)
	steps = [][]string{
		{"/set", st.Loader, "device", dev},
		{"/set", st.Loader, "osdevice", dev},
		{"/set", st.Loader, "path", winload},
		{"/set", st.Loader, "systemroot", `\windows`},
		{"/set", st.Loader, "winpe", "yes"},
		{"/set", st.Loader, "detecthal", "yes"},
		{"/bootsequence", st.Loader},
	}
	for _, s := range steps {
		if _, err := bcdedit(s...); err != nil {
			bcdedit("/delete", st.Loader, "/cleanup")
			bcdedit("/delete", rd)
			return nil, err
		}
	}
	return st, nil
}

func (a *Agent) removeBootEntry() {
	b, err := os.ReadFile(filepath.Join(recoveryDir(), "bcd.json"))
	if err != nil {
		return
	}
	var st bcdState
	if json.Unmarshal(b, &st) == nil {
		if st.Loader != "" {
			bcdedit("/delete", st.Loader, "/cleanup")
		}
		if st.Ramdisk != "" {
			bcdedit("/delete", st.Ramdisk)
		}
	}
}

// afterImageRestore runs at agent start. If the recovery folder still
// exists, this is the old system: the rescue environment did not restore
// (aborted before touching the disk, or never started).
func (a *Agent) afterImageRestore(ctx context.Context) {
	dir := recoveryDir()
	b, err := os.ReadFile(filepath.Join(dir, "bcd.json"))
	if err != nil {
		return
	}
	var st bcdState
	json.Unmarshal(b, &st)
	a.removeBootEntry()
	os.RemoveAll(dir)
	if st.JobID != 0 {
		a.c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/stale", st.JobID),
			proto.JobResult{Message: "Wiederherstellungsumgebung wurde nicht ausgeführt oder vor dem Löschen der Festplatte abgebrochen; das alte System läuft weiter"}, nil)
	}
}

func driverScript(dir string) string {
	return fmt.Sprintf(`
$dir = '%[1]s'
$drv = Join-Path $dir 'drivers'; $mnt = Join-Path $dir 'mount'
New-Item -ItemType Directory -Force $drv, $mnt | Out-Null
$infs = Get-CimInstance Win32_PnPSignedDriver | Where-Object { $_.DeviceClass -in 'NET','SCSIADAPTER','HDC' -and $_.InfName -like 'oem*.inf' } | Select-Object -ExpandProperty InfName -Unique
foreach ($i in $infs) { pnputil.exe /export-driver $i $drv | Out-Null }
if (@(Get-ChildItem $drv -Recurse -Filter *.inf).Count -gt 0) {
  dism.exe /Mount-Image /ImageFile:"$dir\boot.wim" /Index:1 /MountDir:"$mnt" /Quiet | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "DISM Mount fehlgeschlagen ($LASTEXITCODE)" }
  dism.exe /Image:"$mnt" /Add-Driver /Driver:"$drv" /Recurse /ForceUnsigned /Quiet | Out-Null
  $add = $LASTEXITCODE
  dism.exe /Unmount-Image /MountDir:"$mnt" /Commit /Quiet | Out-Null
  "$(@($infs).Count) Treiberpakete eingebunden (DISM $add)"
}
Remove-Item -Recurse -Force $drv, $mnt -ErrorAction SilentlyContinue`, strings.ReplaceAll(dir, "'", "''"))
}
