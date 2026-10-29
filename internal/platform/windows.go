//go:build windows

package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"

	"bastion/internal/layout"
)

// PowerShell runs a script and returns its output. The script is passed
// encoded so quoting never breaks.
func PowerShell(script string) (string, error) {
	full := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $ProgressPreference='SilentlyContinue'; $ErrorActionPreference='Stop'; " + script
	u := utf16.Encode([]rune(full))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.Index(msg, "\n"); i > 0 {
			msg = msg[:i]
		}
		return string(out), fmt.Errorf("%v: %s", err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// CreateSnapshot creates a VSS shadow copy of a volume ("C:" or "C:\"),
// application-consistent with the VSS writers where possible.
func CreateSnapshot(volume string) (*Snap, error) {
	vol := strings.TrimRight(volume, `\`) + `\`
	dev, release, err := vssCreate(vol)
	if err == nil {
		return &Snap{Root: dev + `\`, Kind: "vss", Info: "VSS-Snapshot mit Writern (anwendungskonsistent)", release: release}, nil
	}
	s, err2 := wmiSnapshot(vol)
	if err2 != nil {
		return nil, fmt.Errorf("%v; ohne Writer: %v", err, err2)
	}
	s.Warning = fmt.Sprintf("VSS-Writer nicht nutzbar (%v) – Snapshot ohne Writer, Datenbanken nur absturzkonsistent", err)
	return s, nil
}

// wmiSnapshot creates a shadow copy without writers (crash-consistent).
func wmiSnapshot(vol string) (*Snap, error) {
	out, err := PowerShell(fmt.Sprintf(
		`$r = Invoke-CimMethod -ClassName Win32_ShadowCopy -MethodName Create -Arguments @{Volume='%s'; Context='ClientAccessible'}
if ($r.ReturnValue -ne 0) { throw "VSS-Fehler $($r.ReturnValue)" }
$s = Get-CimInstance Win32_ShadowCopy -Filter "ID='$($r.ShadowID)'"
Write-Output "$($r.ShadowID)|$($s.DeviceObject)"`, strings.ReplaceAll(vol, "'", "''")))
	if err != nil {
		return nil, fmt.Errorf("VSS-Snapshot von %s fehlgeschlagen: %w", vol, err)
	}
	parts := strings.SplitN(strings.TrimSpace(lastLine(out)), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, fmt.Errorf("VSS-Snapshot: unerwartete Ausgabe %q", out)
	}
	id, dev := parts[0], parts[1]
	return &Snap{
		Root: dev + `\`,
		Kind: "vss",
		release: func() {
			DeleteSnapshot(id)
		},
	}, nil
}

// DeleteSnapshot removes a shadow copy by id.
func DeleteSnapshot(id string) {
	PowerShell(fmt.Sprintf(`Get-CimInstance Win32_ShadowCopy -Filter "ID='%s'" | Remove-CimInstance`, strings.ReplaceAll(id, "'", "")))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

const layoutScript = `
$disks = @(Get-Disk | Where-Object { $_.BusType -ne 'USB' -and $_.OperationalStatus -eq 'Online' } | ForEach-Object {
  $d = $_
  [pscustomobject]@{
    Number = $d.Number; Size = $d.Size; Style = "$($d.PartitionStyle)"; Model = "$($d.FriendlyName)";
    Serial = "$($d.SerialNumber)".Trim(); System = ($d.IsSystem -or $d.IsBoot);
    Partitions = @(Get-Partition -DiskNumber $d.Number -ErrorAction SilentlyContinue | ForEach-Object {
      $v = $null; try { $v = $_ | Get-Volume -ErrorAction Stop } catch {}
      [pscustomobject]@{
        Number = $_.PartitionNumber; Offset = $_.Offset; Size = $_.Size; GptType = "$($_.GptType)";
        MbrType = [int]$_.MbrType; Active = [bool]$_.IsActive; Letter = "$($_.DriveLetter)".Trim([char]0).Trim();
        IsSystem = [bool]$_.IsSystem; IsBoot = [bool]$_.IsBoot;
        FS = "$($v.FileSystem)"; Label = "$($v.FileSystemLabel)"; Free = [int64]$v.SizeRemaining; VSize = [int64]$v.Size
      }
    })
  }
})
ConvertTo-Json -InputObject $disks -Depth 6 -Compress`

type psPart struct {
	Number   int
	Offset   int64
	Size     int64
	GptType  string
	MbrType  int
	Active   bool
	Letter   string
	IsSystem bool
	IsBoot   bool
	FS       string
	Label    string
	Free     int64
	VSize    int64
}

type psDisk struct {
	Number     int
	Size       int64
	Style      string
	Model      string
	Serial     string
	System     bool
	Partitions []psPart
}

// CaptureLayout reads disks and partitions via the Storage module.
func CaptureLayout() (*layout.Machine, error) {
	out, err := PowerShell(layoutScript)
	if err != nil {
		return nil, fmt.Errorf("Datenträger-Layout: %w", err)
	}
	var disks []psDisk
	if err := json.Unmarshal([]byte(out), &disks); err != nil {
		return nil, fmt.Errorf("Datenträger-Layout: %w", err)
	}
	m := &layout.Machine{OS: "windows", Firmware: "bios"}
	if strings.EqualFold(os.Getenv("firmware_type"), "UEFI") {
		m.Firmware = "uefi"
	}
	windir := strings.ToUpper(os.Getenv("SystemDrive"))
	for _, d := range disks {
		ld := layout.Disk{Number: d.Number, Path: fmt.Sprintf(`\\.\PhysicalDrive%d`, d.Number), Size: d.Size,
			Style: strings.ToLower(d.Style), Model: d.Model, Serial: d.Serial, System: d.System}
		for _, p := range d.Partitions {
			lp := layout.Partition{Number: p.Number, Offset: p.Offset, Size: p.Size, Active: p.Active,
				FS: strings.ToLower(p.FS), Label: p.Label}
			if p.Letter != "" {
				lp.Mount = strings.ToUpper(p.Letter) + ":"
			}
			if p.VSize > 0 {
				lp.Used = p.VSize - p.Free
			}
			gt := strings.ToLower(strings.Trim(p.GptType, "{}"))
			if ld.Style == "gpt" {
				lp.Type = gt
			} else {
				lp.Type = fmt.Sprintf("0x%02x", p.MbrType)
			}
			switch {
			case gt == gptESP || p.MbrType == 0xef:
				lp.Role = layout.RoleESP
			case gt == gptMSR:
				lp.Role = layout.RoleMSR
			case gt == gptRecovery || p.MbrType == 0x27:
				lp.Role = layout.RoleRecovery
			case lp.Mount != "" && lp.Mount == windir:
				lp.Role = layout.RoleSystem
			case ld.Style == "mbr" && p.Active && lp.Mount == "":
				lp.Role = layout.RoleBoot // "System Reserved"
			case lp.Mount != "":
				lp.Role = layout.RoleData
			default:
				lp.Role = layout.RoleOther
			}
			if lp.Mount != "" && (lp.FS == "ntfs" || lp.FS == "refs") &&
				(lp.Role == layout.RoleSystem || lp.Role == layout.RoleData) {
				lp.Volume = lp.Mount
			}
			ld.Partitions = append(ld.Partitions, lp)
		}
		m.Disks = append(m.Disks, ld)
	}
	return m, nil
}

// OSVersion returns e.g. "Windows 11 Pro 23H2 (22631)".
func OSVersion() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "Windows"
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	disp, _, _ := k.GetStringValue("DisplayVersion")
	build, _, _ := k.GetStringValue("CurrentBuild")
	if b := build; b != "" {
		var n int
		fmt.Sscan(b, &n)
		if n >= 22000 {
			name = strings.Replace(name, "Windows 10", "Windows 11", 1)
		}
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s (%s)", name, disp, build))
}

// VolumeOf splits a path into its volume ("C:") and the rest.
func VolumeOf(p string) (vol, rel string) {
	vol = filepath.VolumeName(p)
	return vol, strings.TrimLeft(strings.TrimPrefix(p, vol), `\/`)
}

// JoinSnap joins a snapshot root and a relative path.
func JoinSnap(root, rel string) string {
	if rel == "" {
		return root
	}
	return strings.TrimRight(root, `\`) + `\` + rel
}

// FormatVolume quickly reformats a data volume (it is dismounted by force).
func FormatVolume(key, fs, label string) error {
	letter := strings.TrimSuffix(strings.ToUpper(key), ":")
	if len(letter) != 1 {
		return fmt.Errorf("Volume %s kann nicht formatiert werden", key)
	}
	if strings.EqualFold(key, os.Getenv("SystemDrive")) {
		return fmt.Errorf("das System-Volume %s kann nicht im laufenden Betrieb formatiert werden (Image-Wiederherstellung verwenden)", key)
	}
	if fs == "" {
		fs = "NTFS"
	}
	_, err := PowerShell(fmt.Sprintf(`Format-Volume -DriveLetter %s -FileSystem %s -NewFileSystemLabel '%s' -Force -Confirm:$false | Out-Null`,
		letter, strings.ToUpper(fs), strings.ReplaceAll(label, "'", "''")))
	return err
}

// ScheduleReboot reboots after delay seconds with a message for the user.
func ScheduleReboot(delay int, msg string) error {
	if delay < 0 {
		delay = 0
	}
	_, err := run("shutdown.exe", "/r", "/f", "/t", fmt.Sprint(delay), "/c", msg, "/d", "p:4:1")
	return err
}
