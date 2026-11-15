// Package fsx reads and writes file system trees with all metadata needed
// for a bootable system restore (Windows: BackupRead/BackupWrite streams,
// Linux: ownership, modes, xattrs/ACLs, devices, hard links).
package fsx

import (
	"io"
	"path"
	"strings"

	"bastion/internal/repo"
)

// Entry is one item found while walking a tree.
type Entry struct {
	Node *repo.Node
	// Open returns the content stream. Nil for entries without content.
	Open func() (io.ReadCloser, error)
}

// WalkOptions configure Walk.
type WalkOptions struct {
	Excludes []string // glob patterns on the relative "/" path, case-insensitive on Windows
	// Error is called for entries that can not be read; the walk continues.
	Error func(path string, err error)
}

// DefaultWindowsExcludes are never needed for a restore.
var DefaultWindowsExcludes = []string{
	"pagefile.sys", "hiberfil.sys", "swapfile.sys", "dumpstack.log.tmp",
	"System Volume Information", "$WINDOWS.~BT", "$WINDOWS.~WS",
	"Windows/SoftwareDistribution/Download/*",
	"Windows/Temp/*", "Users/*/AppData/Local/Temp/*",
	"ProgramData/Bastion/cache/*", "BastionRecovery",
}

var DefaultLinuxExcludes = []string{
	"tmp/*", "var/tmp/*", "var/cache/apt/archives/*.deb", "swapfile", "swap.img",
	".bastion-snap*", "lost+found/*",
	"boot/bastion", "bastion", "etc/grub.d/42_bastion", "run/bastion",
}

// Excluded reports whether rel matches one of the patterns. A pattern
// without "/" matches the name at any level only when it is anchored at
// the root ("pagefile.sys" only excludes /pagefile.sys), patterns with "/"
// are matched against the whole relative path.
func Excluded(rel string, patterns []string, fold bool) bool {
	if fold {
		rel = strings.ToLower(rel)
	}
	for _, p := range patterns {
		p = strings.Trim(strings.ReplaceAll(p, "\\", "/"), "/")
		if fold {
			p = strings.ToLower(p)
		}
		if p == "" {
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		// "*" in patterns also covers deeper levels: "Windows/Temp/*"
		// excludes everything below Windows/Temp.
		if strings.HasSuffix(p, "/*") && strings.HasPrefix(rel, strings.TrimSuffix(p, "*")) {
			return true
		}
	}
	return false
}

// ExcludeBelow turns an absolute path into an exclude pattern for a backup
// that reads from root: everything below abs is left out, the directory
// itself stays (empty), so the path exists again after a restore. It
// returns false when abs does not lie inside root. fold compares without
// case (Windows).
func ExcludeBelow(root, abs string, fold bool) (string, bool) {
	norm := func(p string) string {
		p = strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/")
		if fold {
			p = strings.ToLower(p)
		}
		return p
	}
	r, a := norm(root), norm(abs)
	if a == r {
		return "*", true
	}
	if !strings.HasPrefix(a, r+"/") {
		return "", false
	}
	// keep the original spelling of the part below root
	orig := strings.TrimRight(strings.ReplaceAll(abs, "\\", "/"), "/")
	return orig[len(r)+1:] + "/*", true
}

// InlineLimit: streams up to this size are stored in the tree itself.
const InlineLimit = 4096
