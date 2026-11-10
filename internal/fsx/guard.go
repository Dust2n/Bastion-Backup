package fsx

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// pathGuard keeps a restore inside its root. Paths come from the backup's
// directory tree, which a compromised machine could have crafted: "../"
// components, absolute paths, or a symlink/junction followed by entries
// "below" it would otherwise write anywhere on the target.
type pathGuard struct {
	links map[string]bool // restored links/reparse points ("/" paths)
}

func guardKey(rel string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(rel)
	}
	return rel
}

// check validates a node path (or hard link target) relative to the root.
func (g *pathGuard) check(rel string) error {
	if rel == "" {
		return nil // the volume root itself
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) || strings.Contains(rel, "\x00") {
		return fmt.Errorf("%q: unzulässiger Pfad im Backup, übersprungen", rel)
	}
	if len(g.links) > 0 {
		key := guardKey(rel)
		for i := 0; i < len(key); i++ {
			if key[i] == '/' && g.links[key[:i]] {
				return fmt.Errorf("%q: liegt unterhalb einer Verknüpfung (%s), übersprungen", rel, rel[:i])
			}
		}
	}
	return nil
}

// addLink remembers a restored link; nothing may be written through it.
func (g *pathGuard) addLink(rel string) {
	if g.links == nil {
		g.links = map[string]bool{}
	}
	g.links[guardKey(rel)] = true
}
