//go:build linux

package fsx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"bastion/internal/repo"
)

// EnablePrivileges is a no-op on Linux (the agent runs as root).
func EnablePrivileges() {}

func nodeFromStat(st *unix.Stat_t, n *repo.Node) {
	n.Mode = st.Mode & 07777
	n.UID = st.Uid
	n.GID = st.Gid
	n.Size = st.Size
	n.Mtime = st.Mtim.Nano()
	n.Atime = st.Atim.Nano()
	n.Ctime = st.Ctim.Nano()
	n.FileID = fmt.Sprintf("%x:%x", st.Dev, st.Ino)
}

func readXattrs(p string) map[string][]byte {
	sz, err := unix.Llistxattr(p, nil)
	if err != nil || sz <= 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = unix.Llistxattr(p, buf)
	if err != nil {
		return nil
	}
	out := map[string][]byte{}
	for _, name := range bytes.Split(buf[:sz], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		vs, err := unix.Lgetxattr(p, string(name), nil)
		if err != nil || vs < 0 {
			continue
		}
		v := make([]byte, vs)
		vs, err = unix.Lgetxattr(p, string(name), v)
		if err != nil {
			continue
		}
		out[string(name)] = v[:vs]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Walk reads the tree below root without crossing into other file systems.
func Walk(root string, opt WalkOptions, fn func(*Entry) error) error {
	if opt.Error == nil {
		opt.Error = func(string, error) {}
	}
	var st unix.Stat_t
	if err := unix.Lstat(root, &st); err != nil {
		return fmt.Errorf("Quelle %s nicht lesbar: %w", root, err)
	}
	n := &repo.Node{Path: "", Type: repo.TypeDir}
	nodeFromStat(&st, n)
	n.Size = 0
	n.Xattrs = readXattrs(root)
	if err := fn(&Entry{Node: n}); err != nil {
		return err
	}
	links := map[string]string{}
	return walkDir(root, "", st.Dev, links, opt, fn)
}

func walkDir(abs, rel string, dev uint64, links map[string]string, opt WalkOptions, fn func(*Entry) error) error {
	entries, err := os.ReadDir(abs)
	if err != nil {
		opt.Error(rel, err)
		return nil
	}
	for _, e := range entries {
		childRel := e.Name()
		if rel != "" {
			childRel = rel + "/" + e.Name()
		}
		if Excluded(childRel, opt.Excludes, false) {
			continue
		}
		p := filepath.Join(abs, e.Name())
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			opt.Error(childRel, err)
			continue
		}
		n := &repo.Node{Path: childRel}
		nodeFromStat(&st, n)
		n.Xattrs = readXattrs(p)
		var open func() (io.ReadCloser, error)
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			n.Type = repo.TypeDir
			n.Size = 0
		case unix.S_IFREG:
			n.Type = repo.TypeFile
			if st.Nlink > 1 {
				if first, ok := links[n.FileID]; ok {
					n.Type = repo.TypeHardlink
					n.Link = first
					n.Size = 0
					if err := fn(&Entry{Node: n}); err != nil {
						return err
					}
					continue
				}
				links[n.FileID] = childRel
			}
			open = func() (io.ReadCloser, error) {
				return os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NOATIME, 0)
			}
		case unix.S_IFLNK:
			n.Type = repo.TypeSymlink
			n.Link, err = os.Readlink(p)
			if err != nil {
				opt.Error(childRel, err)
				continue
			}
			n.Size = 0
		case unix.S_IFCHR:
			n.Type, n.Rdev, n.Size = repo.TypeChar, st.Rdev, 0
		case unix.S_IFBLK:
			n.Type, n.Rdev, n.Size = repo.TypeBlock, st.Rdev, 0
		case unix.S_IFIFO:
			n.Type, n.Size = repo.TypeFifo, 0
		case unix.S_IFSOCK:
			continue // sockets are recreated by their programs
		}
		if err := fn(&Entry{Node: n, Open: open}); err != nil {
			return err
		}
		if n.Type == repo.TypeDir && st.Dev == dev {
			if err := walkDir(p, childRel, dev, links, opt, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Writer restores nodes below Root.
type Writer struct {
	Root          string
	Overwrite     string // always (default) | never | rename
	DelayLocked   bool   // unused on Linux: files are replaced atomically
	PendingReboot bool
	dirs          []*repo.Node
}

func NewWriter(root string) *Writer { return &Writer{Root: root} }

func (w *Writer) Write(n *repo.Node, content io.Reader) error {
	p := filepath.Join(w.Root, filepath.FromSlash(n.Path))
	switch n.Type {
	case repo.TypeDir:
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		}
		w.meta(p, n, false)
		c := *n
		c.Path = p
		w.dirs = append(w.dirs, &c)
		return nil
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
		tmp := p + ".bastion-tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if content != nil {
			if _, err := io.Copy(f, content); err != nil {
				f.Close()
				os.Remove(tmp)
				return fmt.Errorf("%s: %w", n.Path, err)
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
		w.meta(p, n, true)
	case repo.TypeSymlink:
		os.Remove(p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(n.Link, p); err != nil {
			return err
		}
		w.meta(p, n, true)
	case repo.TypeHardlink:
		os.Remove(p)
		return os.Link(filepath.Join(w.Root, filepath.FromSlash(n.Link)), p)
	case repo.TypeChar, repo.TypeBlock, repo.TypeFifo:
		os.Remove(p)
		mode := n.Mode & 07777
		switch n.Type {
		case repo.TypeChar:
			mode |= unix.S_IFCHR
		case repo.TypeBlock:
			mode |= unix.S_IFBLK
		default:
			mode |= unix.S_IFIFO
		}
		if err := unix.Mknod(p, mode, int(n.Rdev)); err != nil {
			return err
		}
		w.meta(p, n, true)
	}
	return nil
}

func (w *Writer) meta(p string, n *repo.Node, times bool) {
	unix.Lchown(p, int(n.UID), int(n.GID))
	if n.Type != repo.TypeSymlink {
		unix.Chmod(p, n.Mode&07777) // after chown, which clears setuid bits
	}
	for k, v := range n.Xattrs {
		unix.Lsetxattr(p, k, v, 0)
	}
	if times {
		setTimes(p, n)
	}
}

func setTimes(p string, n *repo.Node) {
	ts := []unix.Timespec{unix.NsecToTimespec(n.Atime), unix.NsecToTimespec(n.Mtime)}
	unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func (w *Writer) Finish() error {
	for i := len(w.dirs) - 1; i >= 0; i-- {
		setTimes(w.dirs[i].Path, w.dirs[i])
	}
	w.dirs = nil
	return nil
}
