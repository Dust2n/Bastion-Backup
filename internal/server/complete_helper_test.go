package server

import (
	"os"
	"testing"
	"time"

	"bastion/internal/repo"
)

// osChtimes makes a chunk look old, so the cleanup may delete it. The
// cleanup goes by the age of the pack, so the pack is finished first.
func osChtimes(st *repo.Store, id repo.ID, t time.Time) error {
	st.Seal()
	file, _, _, ok := st.Location(id)
	if !ok {
		return nil
	}
	return os.Chtimes(file, t, t)
}

// damageChunk flips the last byte of a stored chunk.
func damageChunk(t *testing.T, st *repo.Store, id repo.ID) {
	t.Helper()
	file, off, n, ok := st.Location(id)
	if !ok {
		t.Fatal("Chunk nicht gespeichert")
	}
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	f.ReadAt(b, off+int64(n)-1)
	b[0] ^= 0xff
	f.WriteAt(b, off+int64(n)-1)
}

// chunkCount counts the chunks of a store.
func chunkCount(st *repo.Store) (n int) {
	st.Walk(func(repo.ChunkInfo) error { n++; return nil })
	return
}
