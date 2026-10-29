//go:build windows

package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"bastion/internal/repo"
)

var (
	modkernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procBackupRead        = modkernel32.NewProc("BackupRead")
	procBackupWrite       = modkernel32.NewProc("BackupWrite")
	procSetFileShortNameW = modkernel32.NewProc("SetFileShortNameW")
)

const (
	attrReadonly    = 0x1
	attrHidden      = 0x2
	attrSystem      = 0x4
	attrDirectory   = 0x10
	attrArchive     = 0x20
	attrNormal      = 0x80
	attrTemporary   = 0x100
	attrSparse      = 0x200
	attrReparse     = 0x400
	attrOffline     = 0x1000
	attrNotIndexed  = 0x2000
	attrEncrypted   = 0x4000
	settableAttrs   = attrReadonly | attrHidden | attrSystem | attrArchive | attrTemporary | attrOffline | attrNotIndexed
	tagWOF          = 0x80000017
	tagDedup        = 0x80000013
	accessSysSec    = 0x01000000 // ACCESS_SYSTEM_SECURITY
	fsctlSetSparse  = 0x000900c4
	errSharingViol  = windows.Errno(32)
	errAlreadyExist = windows.Errno(183)
)

type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

func b2u(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

func backupRead(h windows.Handle, b []byte, abort bool, ctx *uintptr) (uint32, error) {
	var n uint32
	var p uintptr
	if len(b) > 0 {
		p = uintptr(unsafe.Pointer(&b[0]))
	}
	r, _, err := procBackupRead.Call(uintptr(h), p, uintptr(len(b)), uintptr(unsafe.Pointer(&n)), b2u(abort), 1, uintptr(unsafe.Pointer(ctx)))
	if r == 0 {
		return n, err
	}
	return n, nil
}

func backupWrite(h windows.Handle, b []byte, abort bool, ctx *uintptr) (uint32, error) {
	var n uint32
	var p uintptr
	if len(b) > 0 {
		p = uintptr(unsafe.Pointer(&b[0]))
	}
	r, _, err := procBackupWrite.Call(uintptr(h), p, uintptr(len(b)), uintptr(unsafe.Pointer(&n)), b2u(abort), 1, uintptr(unsafe.Pointer(ctx)))
	if r == 0 {
		return n, err
	}
	return n, nil
}

var privOnce sync.Once

// EnablePrivileges enables the privileges needed to read and write every
// file including security descriptors, regardless of ACLs.
func EnablePrivileges() {
	privOnce.Do(func() {
		var tok windows.Token
		if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
			return
		}
		defer tok.Close()
		for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeSecurityPrivilege",
			"SeTakeOwnershipPrivilege", "SeManageVolumePrivilege", "SeCreateSymbolicLinkPrivilege"} {
			var luid windows.LUID
			if windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid) != nil {
				continue
			}
			tp := windows.Tokenprivileges{PrivilegeCount: 1}
			tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
			windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
		}
	})
}

func ft2ns(ft int64) int64 {
	if ft == 0 {
		return 0
	}
	return (ft - 116444736000000000) * 100
}

func ns2ft(ns int64) int64 {
	if ns == 0 {
		return 0
	}
	return ns/100 + 116444736000000000
}

// LongPath adds the \\?\ prefix so paths longer than MAX_PATH work.
func LongPath(p string) string {
	if strings.HasPrefix(p, `\\`) {
		return p
	}
	return `\\?\` + p
}

type winWalker struct {
	opt   WalkOptions
	links map[string]string
}

// Walk reads the tree below root (a directory or a VSS shadow device path).
func Walk(root string, opt WalkOptions, fn func(*Entry) error) error {
	EnablePrivileges()
	if opt.Error == nil {
		opt.Error = func(string, error) {}
	}
	root = LongPath(strings.TrimRight(root, `\`) + `\`)
	w := &winWalker{opt: opt, links: map[string]string{}}
	n := &repo.Node{Path: "", Type: repo.TypeDir}
	if _, err := w.stat(root, n, false); err != nil {
		return fmt.Errorf("Quelle %s nicht lesbar: %w", root, err)
	}
	if err := fn(&Entry{Node: n, Open: w.opener(root, false)}); err != nil {
		return err
	}
	return w.dir(root, "", fn)
}

func (w *winWalker) dir(abs, rel string, fn func(*Entry) error) error {
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(windows.StringToUTF16Ptr(abs+"*"), &fd)
	if err != nil {
		w.opt.Error(rel, err)
		return nil
	}
	var list []windows.Win32finddata
	for {
		name := windows.UTF16ToString(fd.FileName[:])
		if name != "." && name != ".." {
			list = append(list, fd)
		}
		if windows.FindNextFile(h, &fd) != nil {
			break
		}
	}
	windows.FindClose(h)
	sort.Slice(list, func(i, j int) bool {
		return windows.UTF16ToString(list[i].FileName[:]) < windows.UTF16ToString(list[j].FileName[:])
	})

	for i := range list {
		fd := &list[i]
		name := windows.UTF16ToString(fd.FileName[:])
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		if Excluded(childRel, w.opt.Excludes, true) {
			continue
		}
		attrs := fd.FileAttributes
		isDir := attrs&attrDirectory != 0
		isReparse := attrs&attrReparse != 0
		// WOF (CompactOS) and dedup files are read through the filter, so
		// they are restored as normal files that work on any volume.
		follow := isReparse && !isDir && (fd.Reserved0 == tagWOF || fd.Reserved0 == tagDedup)
		absChild := abs + name
		if attrs&attrEncrypted != 0 && !isDir {
			w.opt.Error(childRel, errors.New("EFS-verschlüsselte Datei wird nicht gesichert"))
			continue
		}
		n := &repo.Node{Path: childRel, Short: windows.UTF16ToString(fd.AlternateFileName[:])}
		nlink, err := w.stat(absChild, n, follow)
		if err != nil {
			w.opt.Error(childRel, err)
			continue
		}
		if follow {
			n.Mode &^= attrReparse | attrSparse
		}
		if isDir {
			n.Type = repo.TypeDir
			n.Size = 0
		} else {
			n.Type = repo.TypeFile
			if nlink > 1 {
				if first, ok := w.links[n.FileID]; ok {
					n.Type = repo.TypeHardlink
					n.Link = first
					if err := fn(&Entry{Node: n}); err != nil {
						return err
					}
					continue
				}
				w.links[n.FileID] = childRel
			}
		}
		if err := fn(&Entry{Node: n, Open: w.opener(absChild, follow)}); err != nil {
			return err
		}
		if isDir && !isReparse {
			if err := w.dir(absChild+`\`, childRel, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *winWalker) stat(p string, n *repo.Node, follow bool) (uint32, error) {
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if !follow {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(p), windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bi); err != nil {
		return 0, err
	}
	var b fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		return 0, err
	}
	n.FileID = fmt.Sprintf("%08x-%08x%08x", bi.VolumeSerialNumber, bi.FileIndexHigh, bi.FileIndexLow)
	n.Size = int64(bi.FileSizeHigh)<<32 | int64(bi.FileSizeLow)
	n.Mode = b.FileAttributes
	n.Btime = ft2ns(b.CreationTime)
	n.Atime = ft2ns(b.LastAccessTime)
	n.Mtime = ft2ns(b.LastWriteTime)
	n.Ctime = ft2ns(b.ChangeTime)
	return bi.NumberOfLinks, nil
}

func (w *winWalker) opener(p string, follow bool) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_SEQUENTIAL_SCAN)
		if !follow {
			flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
		}
		share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
		name := windows.StringToUTF16Ptr(p)
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.READ_CONTROL|accessSysSec, share, nil, windows.OPEN_EXISTING, flags, 0)
		if err != nil {
			// without SeSecurityPrivilege the SACL can not be read
			h, err = windows.CreateFile(name, windows.GENERIC_READ|windows.READ_CONTROL, share, nil, windows.OPEN_EXISTING, flags, 0)
		}
		if err != nil {
			return nil, err
		}
		return &backupReader{h: h}, nil
	}
}

type backupReader struct {
	h    windows.Handle
	ctx  uintptr
	done bool
}

func (r *backupReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if len(p) > 4<<20 {
		p = p[:4<<20]
	}
	n, err := backupRead(r.h, p, false, &r.ctx)
	if err != nil {
		return int(n), err
	}
	if n == 0 {
		r.done = true
		return 0, io.EOF
	}
	return int(n), nil
}

func (r *backupReader) Close() error {
	if r.ctx != 0 {
		backupRead(r.h, nil, true, &r.ctx)
	}
	return windows.CloseHandle(r.h)
}

// Writer restores nodes below Root.
type Writer struct {
	Root          string
	Overwrite     string // always (default) | never | rename
	DelayLocked   bool   // replace files in use at the next reboot
	PendingReboot bool
	dirs          []*repo.Node
}

func NewWriter(root string) *Writer {
	EnablePrivileges()
	return &Writer{Root: root}
}

func (w *Writer) abs(rel string) string {
	return LongPath(filepath.Join(w.Root, filepath.FromSlash(rel)))
}

// Write restores one node. content is the stored stream (nil for none).
func (w *Writer) Write(n *repo.Node, content io.Reader) error {
	p := w.abs(n.Path)
	switch n.Type {
	case repo.TypeDir:
		if err := windows.CreateDirectory(windows.StringToUTF16Ptr(p), nil); err != nil && !errors.Is(err, errAlreadyExist) {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		}
		if content != nil {
			h, err := openForRestore(p, windows.OPEN_EXISTING)
			if err != nil {
				return err
			}
			err = writeStream(h, content)
			windows.CloseHandle(h)
			if err != nil {
				return fmt.Errorf("%s: %w", n.Path, err)
			}
		}
		c := *n
		c.Path = p
		w.dirs = append(w.dirs, &c)
		return nil

	case repo.TypeHardlink:
		target := w.abs(n.Link)
		clearAttrs(p)
		os.Remove(p)
		return windows.CreateHardLink(windows.StringToUTF16Ptr(p), windows.StringToUTF16Ptr(target), 0)

	case repo.TypeFile:
		if _, err := os.Lstat(p); err == nil {
			switch w.Overwrite {
			case "never":
				return nil
			case "rename":
				ext := filepath.Ext(p)
				p = strings.TrimSuffix(p, ext) + ".restored-" + time.Now().Format("20060102-150405") + ext
			}
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		clearAttrs(p)
		h, err := openForRestore(p, windows.CREATE_ALWAYS)
		delayed := ""
		if errors.Is(err, errSharingViol) && w.DelayLocked {
			delayed = p
			p = p + ".bastion-new"
			clearAttrs(p)
			h, err = openForRestore(p, windows.CREATE_ALWAYS)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", n.Path, err)
		}
		if n.Mode&attrSparse != 0 {
			var ret uint32
			windows.DeviceIoControl(h, fsctlSetSparse, nil, 0, nil, 0, &ret, nil)
		}
		if content != nil {
			if err := writeStream(h, content); err != nil {
				windows.CloseHandle(h)
				return fmt.Errorf("%s: %w", n.Path, err)
			}
		}
		if n.Short != "" && delayed == "" {
			procSetFileShortNameW.Call(uintptr(h), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(n.Short))))
		}
		setBasic(h, n)
		windows.CloseHandle(h)
		if delayed != "" {
			err := windows.MoveFileEx(windows.StringToUTF16Ptr(p), windows.StringToUTF16Ptr(delayed),
				windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_DELAY_UNTIL_REBOOT)
			if err != nil {
				return fmt.Errorf("%s: Ersetzen beim Neustart nicht möglich: %w", n.Path, err)
			}
			w.PendingReboot = true
		}
		return nil
	}
	return nil // unix-only types do not occur in Windows backups
}

// Finish sets directory timestamps and attributes (after their content).
func (w *Writer) Finish() error {
	for i := len(w.dirs) - 1; i >= 0; i-- {
		d := w.dirs[i]
		h, err := windows.CreateFile(windows.StringToUTF16Ptr(d.Path), windows.FILE_WRITE_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			continue
		}
		setBasic(h, d)
		windows.CloseHandle(h)
	}
	w.dirs = nil
	return nil
}

func clearAttrs(p string) {
	windows.SetFileAttributes(windows.StringToUTF16Ptr(p), attrNormal)
}

func openForRestore(p string, disposition uint32) (windows.Handle, error) {
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE | accessSysSec)
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	name := windows.StringToUTF16Ptr(p)
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ, nil, disposition, flags, 0)
	if err != nil && !errors.Is(err, errSharingViol) {
		h, err = windows.CreateFile(name, access&^accessSysSec, windows.FILE_SHARE_READ, nil, disposition, flags, 0)
	}
	return h, err
}

func writeStream(h windows.Handle, r io.Reader) error {
	buf := make([]byte, 1<<20)
	var ctx uintptr
	defer func() {
		if ctx != 0 {
			backupWrite(h, nil, true, &ctx)
		}
	}()
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			written, err := backupWrite(h, buf[:n], false, &ctx)
			if err != nil {
				return err
			}
			if int(written) != n {
				return errors.New("BackupWrite: unvollständig geschrieben")
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

func setBasic(h windows.Handle, n *repo.Node) {
	b := fileBasicInfo{
		CreationTime:   ns2ft(n.Btime),
		LastAccessTime: ns2ft(n.Atime),
		LastWriteTime:  ns2ft(n.Mtime),
		ChangeTime:     ns2ft(n.Ctime),
		FileAttributes: n.Mode & settableAttrs,
	}
	if b.FileAttributes == 0 && n.Type != repo.TypeDir {
		b.FileAttributes = attrNormal
	}
	windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b)))
}
