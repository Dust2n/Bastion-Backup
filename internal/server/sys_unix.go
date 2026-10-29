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
