//go:build windows

package rescue

import (
	"bytes"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// Runs against the machine's own disks (read-only): the value computed for
// C: must equal the one Windows itself stored in MountedDevices.
func TestNativeDiskQueries(t *testing.T) {
	disks, err := listDisks()
	if err != nil {
		t.Skip("keine Datenträger lesbar (keine Administratorrechte?):", err)
	}
	for _, d := range disks {
		t.Logf("Disk %d: %d Bytes, %q, Seriennr. %q", d.Number, d.Size, d.Model, d.Serial)
	}
	want, err := func() ([]byte, error) {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\MountedDevices`, registry.QUERY_VALUE)
		if err != nil {
			return nil, err
		}
		defer k.Close()
		v, _, err := k.GetBinaryValue(`\DosDevices\C:`)
		return v, err
	}()
	if err != nil {
		t.Skip("MountedDevices nicht lesbar:", err)
	}
	got, err := mountedDevicesValue('C')
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MountedDevices-Wert für C: falsch:\n got %x\nwant %x", got, want)
	}
	disk, _, _ := volumeLocation('C')
	t.Logf("C: liegt auf Disk %d, MSR vorhanden: %v", disk, hasMSR(int(disk)))
}
