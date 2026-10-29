//go:build windows

package platform

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// VSS requester with writers (IVssBackupComponents via COM). Unlike a
// Win32_ShadowCopy "ClientAccessible" snapshot, the writers of SQL Server,
// Exchange, Hyper-V, Active Directory etc. flush and freeze their data, so
// the snapshot is application-consistent. The backup type is COPY: log
// chains of other backup software are not truncated.

var (
	modvssapi                     = windows.NewLazySystemDLL("vssapi.dll")
	procCreateVssBackupComponents = modvssapi.NewProc("CreateVssBackupComponentsInternal")
	procVssFreeSnapshotProperties = modvssapi.NewProc("VssFreeSnapshotPropertiesInternal")
	modole32                      = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx            = modole32.NewProc("CoInitializeEx")
	procCoInitializeSecurity      = modole32.NewProc("CoInitializeSecurity")
)

const (
	vssCtxBackup      = 0
	vssBtCopy         = 5
	vssSAsyncFinished = 0x0004230A

	// IVssBackupComponents vtable (vsbackup.h)
	vssRelease               = 2
	vssInitializeForBackup   = 5
	vssSetBackupState        = 6
	vssGatherWriterMetadata  = 9
	vssFreeWriterMetadata    = 12
	vssPrepareForBackup      = 14
	vssAbortBackup           = 15
	vssBackupComplete        = 27
	vssSetContext            = 35
	vssStartSnapshotSet      = 36
	vssAddToSnapshotSet      = 37
	vssDoSnapshotSet         = 38
	vssGetSnapshotProperties = 42

	// IVssAsync vtable
	asyncWait        = 4
	asyncQueryStatus = 5
)

var vssErrors = map[uint32]string{
	0x80070005: "Zugriff verweigert (Agent muss als SYSTEM/Administrator laufen)",
	0x80042301: "VSS in falschem Zustand",
	0x80042306: "Volume wird von keinem VSS-Anbieter unterstützt",
	0x8004230C: "Volume wird nicht unterstützt",
	0x8004230F: "VSS-Anbieter meldet einen unerwarteten Fehler",
	0x80042312: "maximale Anzahl Volumes im Snapshot erreicht",
	0x80042313: "Schreibvorgänge konnten nicht angehalten werden (hohe Last), bitte erneut versuchen",
	0x80042314: "Schreibvorgänge wurden zu lange angehalten",
	0x80042316: "es läuft bereits ein anderer Snapshot (anderes Backup-Programm?)",
	0x80042317: "maximale Anzahl Schattenkopien erreicht",
	0x80042318: "ein VSS-Writer hat einen Fehler gemeldet",
	0x8004231F: "zu wenig Speicher für die Schattenkopie (Schattenkopie-Speicher vergrößern)",
	0x80042326: "VSS-Writer-Zeitüberschreitung",
	0x8004232B: "ein VSS-Writer hat nicht rechtzeitig reagiert",
}

func hrErr(step string, hr uint32) error {
	if msg, ok := vssErrors[hr]; ok {
		return fmt.Errorf("%s: %s (0x%08X)", step, msg, hr)
	}
	return fmt.Errorf("%s: Fehler 0x%08X", step, hr)
}

type comObject struct{ vtbl *[64]uintptr }

func (c *comObject) call(idx int, args ...uintptr) uint32 {
	r, _, _ := syscall.SyscallN(c.vtbl[idx], append([]uintptr{uintptr(unsafe.Pointer(c))}, args...)...)
	return uint32(r)
}

func failed(hr uint32) bool { return int32(hr) < 0 }

// waitAsync waits for an IVssAsync operation and releases it.
func waitAsync(step string, a *comObject, hr uint32) error {
	if failed(hr) {
		return hrErr(step, hr)
	}
	defer a.call(vssRelease)
	if hr = a.call(asyncWait, 0xFFFFFFFF); failed(hr) {
		return hrErr(step, hr)
	}
	var res uint32
	if hr = a.call(asyncQueryStatus, uintptr(unsafe.Pointer(&res)), 0); failed(hr) {
		return hrErr(step, hr)
	}
	if res != vssSAsyncFinished {
		return hrErr(step, res)
	}
	return nil
}

type vssSnapshotProp struct {
	SnapshotID           windows.GUID
	SnapshotSetID        windows.GUID
	SnapshotsCount       int32
	SnapshotDeviceObject *uint16
	OriginalVolumeName   *uint16
	OriginatingMachine   *uint16
	ServiceMachine       *uint16
	ExposedName          *uint16
	ExposedPath          *uint16
	ProviderID           windows.GUID
	SnapshotAttributes   int32
	CreationTimestamp    int64
	Status               int32
}

// vssCreate creates an application-consistent snapshot of one volume
// ("C:\"). The snapshot lives until release is called.
func vssCreate(vol string) (device string, release func(), err error) {
	if err := procCreateVssBackupComponents.Find(); err != nil {
		return "", nil, fmt.Errorf("vssapi.dll: %v", err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// the multi-threaded apartment stays initialized for the process, so
	// the object can be released later from another thread
	procCoInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
	// RPC_C_AUTHN_LEVEL_PKT_PRIVACY, RPC_C_IMP_LEVEL_IDENTIFY, EOAC_DYNAMIC_CLOAKING;
	// fails harmlessly (RPC_E_TOO_LATE) when already set
	procCoInitializeSecurity.Call(0, ^uintptr(0), 0, 0, 6, 2, 0, 0x40, 0)

	var c *comObject
	if hr, _, _ := procCreateVssBackupComponents.Call(uintptr(unsafe.Pointer(&c))); failed(uint32(hr)) || c == nil {
		return "", nil, hrErr("VSS starten", uint32(hr))
	}
	prepared := false
	fail := func(e error) (string, func(), error) {
		if prepared {
			c.call(vssAbortBackup)
		}
		c.call(vssRelease)
		return "", nil, e
	}
	if hr := c.call(vssInitializeForBackup, 0); failed(hr) {
		return fail(hrErr("InitializeForBackup", hr))
	}
	if hr := c.call(vssSetContext, vssCtxBackup); failed(hr) {
		return fail(hrErr("SetContext", hr))
	}
	if hr := c.call(vssSetBackupState, 0, 0, vssBtCopy, 0); failed(hr) {
		return fail(hrErr("SetBackupState", hr))
	}
	var a *comObject
	if err := waitAsync("Writer abfragen", a, c.call(vssGatherWriterMetadata, uintptr(unsafe.Pointer(&a)))); err != nil {
		return fail(err)
	}
	c.call(vssFreeWriterMetadata)

	var setID, snapID, provider windows.GUID
	if hr := c.call(vssStartSnapshotSet, uintptr(unsafe.Pointer(&setID))); failed(hr) {
		return fail(hrErr("StartSnapshotSet", hr))
	}
	volp, _ := windows.UTF16PtrFromString(vol)
	// VSS_ID is passed by value; on amd64 a 16-byte struct goes by reference
	if hr := c.call(vssAddToSnapshotSet, uintptr(unsafe.Pointer(volp)), uintptr(unsafe.Pointer(&provider)), uintptr(unsafe.Pointer(&snapID))); failed(hr) {
		return fail(hrErr("Volume "+vol+" hinzufügen", hr))
	}
	prepared = true
	if err := waitAsync("Writer vorbereiten", a, c.call(vssPrepareForBackup, uintptr(unsafe.Pointer(&a)))); err != nil {
		return fail(err)
	}
	if err := waitAsync("Schattenkopie erstellen", a, c.call(vssDoSnapshotSet, uintptr(unsafe.Pointer(&a)))); err != nil {
		return fail(err)
	}
	var prop vssSnapshotProp
	if hr := c.call(vssGetSnapshotProperties, uintptr(unsafe.Pointer(&snapID)), uintptr(unsafe.Pointer(&prop))); !failed(hr) && prop.SnapshotDeviceObject != nil {
		device = windows.UTF16PtrToString(prop.SnapshotDeviceObject)
		procVssFreeSnapshotProperties.Call(uintptr(unsafe.Pointer(&prop)))
	}
	if device == "" {
		// some systems leave the properties empty; WMI knows the device
		out, _ := PowerShell(fmt.Sprintf(`(Get-CimInstance Win32_ShadowCopy -Filter "ID='%s'").DeviceObject`, snapID.String()))
		device = strings.TrimSpace(out)
	}
	if device == "" {
		return fail(fmt.Errorf("Gerätepfad der Schattenkopie %s nicht ermittelbar", snapID.String()))
	}
	release = func() {
		var a *comObject
		// tells the writers the backup is done; releasing the object
		// deletes the (non-persistent) snapshot
		waitAsync("BackupComplete", a, c.call(vssBackupComplete, uintptr(unsafe.Pointer(&a))))
		c.call(vssRelease)
	}
	return strings.TrimRight(device, `\`), release, nil
}
