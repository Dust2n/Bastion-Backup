//go:build !windows

package server

import (
	"errors"

	"golang.org/x/sys/unix"
)

// connectShare: on Linux SMB shares must be mounted (CIFS) and added as a
// local path.
func connectShare(path, user, pass string) error {
	return errors.New("unter Linux die Freigabe per CIFS einhängen und als lokalen Pfad eintragen")
}

func diskFree(path string) (free, total int64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize)
}

func listDrives() []dirEntry {
	free, total := diskFree("/")
	return []dirEntry{{Name: "/", Path: "/", Free: free, Total: total}}
}

// sameDisk reports whether both paths lie on the same file system. Unknown
// (a path is missing) counts as not the same.
func sameDisk(a, b string) bool {
	var sa, sb unix.Stat_t
	if unix.Stat(a, &sa) != nil || unix.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
