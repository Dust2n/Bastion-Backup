package server

import (
	"os"
	"path/filepath"
	"time"

	"bastion/internal/repo"
)

// osChtimes makes a chunk look old, so the cleanup may delete it.
func osChtimes(st *repo.Store, id repo.ID, t time.Time) error {
	h := id.String()
	return os.Chtimes(filepath.Join(st.Dir(), "chunks", h[:2], h[2:4], h), t, t)
}
