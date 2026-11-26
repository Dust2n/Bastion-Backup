package repo

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openStore(t *testing.T, dir string, key []byte) *Store {
	t.Helper()
	s, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() }) // Windows deletes no open files
	return s
}

// chunk returns deterministic content that does not compress.
func chunk(i int) []byte {
	out := make([]byte, 0, 1024)
	for n := 0; len(out) < 300+i%700; n++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("chunk %d block %d", i, n)))
		out = append(out, h[:]...)
	}
	return out
}

func countFiles(dir, pattern string) int {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return len(m)
}

// writeLoose stores a chunk the way versions up to 0.3.2 did.
func writeLoose(t *testing.T, s *Store, data []byte) ID {
	t.Helper()
	id := Hash(data)
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	blob := append(append([]byte{}, magic...), nonce...)
	blob = s.aead.Seal(blob, nonce, Compress(data), id[:])
	if err := writeDurable(s.loosePath(id), blob); err != nil {
		t.Fatal(err)
	}
	s.hasLoose = true
	return id
}

func age(t *testing.T, s *Store, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	filepath.Walk(s.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			os.Chtimes(p, old, old)
		}
		return nil
	})
}

// Many chunks end up in few files, survive a restart, and packs roll over.
func TestPacksRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	s := openStore(t, dir, key)
	s.PackSize = 64 << 10
	const n = 500
	ids := make([]ID, n)
	for i := range ids {
		id, err := s.Put(chunk(i))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	// readable while the last pack is still open
	for i, id := range ids {
		if got, err := s.Get(id); err != nil || !bytes.Equal(got, chunk(i)) {
			t.Fatalf("chunk %d vor dem Abschluss: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	packs := countFiles(dir, "packs/*/*.pack")
	if packs < 2 || packs > 20 {
		t.Fatalf("%d Packs für %d Chunks", packs, n)
	}
	if idx := countFiles(dir, "packs/*/*.idx"); idx != packs {
		t.Fatalf("%d Packs, %d Indexdateien", packs, idx)
	}
	if loose := countFiles(dir, "chunks/*/*/*"); loose != 0 {
		t.Fatalf("%d Einzeldateien geschrieben", loose)
	}
	s2 := openStore(t, dir, key)
	seen := 0
	s2.Walk(func(ChunkInfo) error { seen++; return nil })
	if seen != n {
		t.Fatalf("nach Neustart %d von %d Chunks", seen, n)
	}
	for i, id := range ids {
		if !s2.Has(id) {
			t.Fatalf("chunk %d fehlt nach Neustart", i)
		}
		if err := s2.Verify(id); err != nil {
			t.Fatal(err)
		}
	}
	if s2.Has(Hash([]byte("gibt es nicht"))) {
		t.Fatal("unbekannter Chunk als vorhanden gemeldet")
	}
	if _, err := s2.Get(Hash([]byte("gibt es nicht"))); err != ErrNotFound {
		t.Fatalf("unbekannter Chunk: %v", err)
	}
}

// Writers from several goroutines (upload workers of several machines).
func TestPacksConcurrent(t *testing.T) {
	s := testStore(t)
	s.PackSize = 32 << 10
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				// half of the chunks are shared between the writers
				n := i
				if i%2 == 1 {
					n = w*1000 + i
				}
				id, err := s.Put(chunk(n))
				if err != nil {
					t.Error(err)
					return
				}
				if got, err := s.Get(id); err != nil || !bytes.Equal(got, chunk(n)) {
					t.Errorf("chunk %d: %v", n, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	want := 100 + 8*100
	seen := 0
	s.Walk(func(ChunkInfo) error { seen++; return nil })
	if seen != want {
		t.Fatalf("%d Chunks, erwartet %d", seen, want)
	}
}

// A crash while a pack is being written: what was complete is still there
// after the restart, the torn rest is cut off, and the store keeps working.
func TestPackRecoveryAfterCrash(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	s := openStore(t, dir, key)
	var ids []ID
	for i := 0; i < 20; i++ {
		id, _ := s.Put(chunk(i))
		ids = append(ids, id)
	}
	s.Flush()
	// no Close: the pack has no index. Simulate a half-written record.
	file, _, _, _ := s.Location(ids[0])
	fi, _ := os.Stat(file)
	f, _ := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
	torn := Hash([]byte("abgerissen"))
	f.Write(torn[:])
	f.Write([]byte{0xff, 0x00, 0x00, 0x00, 'B', 'S'})
	f.Close()

	s2 := openStore(t, dir, key)
	for i, id := range ids {
		if got, err := s2.Get(id); err != nil || !bytes.Equal(got, chunk(i)) {
			t.Fatalf("chunk %d nach Absturz: %v", i, err)
		}
	}
	if s2.Has(torn) {
		t.Fatal("abgerissener Chunk gilt als vorhanden")
	}
	if fi2, _ := os.Stat(file); fi2.Size() != fi.Size() {
		t.Fatalf("Pack nicht auf %d Bytes gekürzt (%d)", fi.Size(), fi2.Size())
	}
	if countFiles(dir, "packs/*/*.idx") != 1 {
		t.Fatal("kein Index nach der Wiederherstellung")
	}
	if _, err := s2.Put(chunk(999)); err != nil {
		t.Fatal(err)
	}

	// a record destroyed in the middle costs that chunk, not the ones after it
	flipByte(t, s2, ids[5])
	s2.Close()
	for _, p := range mustGlob(dir, "packs/*/*.idx") {
		os.Remove(p)
	}
	s3 := openStore(t, dir, key)
	if s3.Has(ids[5]) {
		t.Fatal("beschädigter Chunk nach Index-Neuaufbau vorhanden")
	}
	for i, id := range ids {
		if i != 5 && s3.Verify(id) != nil {
			t.Fatalf("chunk %d nach Index-Neuaufbau verloren", i)
		}
	}
	// a damaged index file is rebuilt as well
	s3.Close()
	for _, p := range mustGlob(dir, "packs/*/*.idx") {
		os.WriteFile(p, []byte("BPI1 kaputt"), 0o600)
	}
	s4 := openStore(t, dir, key)
	if s4.Verify(ids[0]) != nil || s4.Has(ids[5]) {
		t.Fatal("Neuaufbau nach beschädigtem Index fehlerhaft")
	}
}

func mustGlob(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return m
}

// Chunks of the old format are read, and Compact moves them into packs.
func TestMigrationFromSingleFiles(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	s := openStore(t, dir, key)
	s.PackSize = 16 << 10
	live := map[ID]struct{}{}
	var keep, drop []ID
	for i := 0; i < 120; i++ {
		id := writeLoose(t, s, chunk(i))
		if i%3 == 0 {
			drop = append(drop, id)
		} else {
			keep = append(keep, id)
			live[id] = struct{}{}
		}
	}
	// reading and deduplication work before any migration
	s = openStore(t, dir, key)
	s.PackSize = 16 << 10
	for _, id := range append(append([]ID{}, keep...), drop...) {
		if !s.Has(id) || s.Verify(id) != nil {
			t.Fatal("Einzeldatei nicht lesbar")
		}
	}
	if _, err := s.Put(chunk(1)); err != nil || countFiles(dir, "packs/*/*.pack") != 0 {
		t.Fatal("vorhandener Chunk wurde erneut geschrieben")
	}
	seen := 0
	s.Walk(func(ChunkInfo) error { seen++; return nil })
	if seen != 120 {
		t.Fatalf("Walk sieht %d von 120", seen)
	}

	age(t, s, 24*time.Hour)
	st, err := s.Compact(live, time.Now().Add(-6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Migrated != int64(len(keep)) || st.Chunks != int64(len(keep)) || st.Freed == 0 {
		t.Fatalf("Ergebnis: %+v", st)
	}
	if n := countFiles(dir, "chunks/*/*/*"); n != 0 {
		t.Fatalf("%d Einzeldateien übrig", n)
	}
	if n := countFiles(dir, "chunks/*"); n != 0 {
		t.Fatalf("%d leere Ordner übrig", n)
	}
	for _, id := range keep {
		if s.Verify(id) != nil {
			t.Fatal("Chunk nach dem Umpacken nicht lesbar")
		}
	}
	for _, id := range drop {
		if s.Has(id) {
			t.Fatal("nicht mehr benötigter Chunk noch vorhanden")
		}
	}
	// and after a restart
	s = openStore(t, dir, key)
	for _, id := range keep {
		if s.Verify(id) != nil {
			t.Fatal("Chunk nach Umpacken und Neustart nicht lesbar")
		}
	}
	// young unreferenced chunks stay: they may belong to a running backup
	young := writeLoose(t, s, []byte("gerade hochgeladen"))
	if _, err := s.Compact(live, time.Now().Add(-6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s.Verify(young) != nil {
		t.Fatal("junger Chunk wurde gelöscht")
	}
}

// Compact deletes packs nobody needs, rewrites packs that are mostly
// unused and leaves full and young ones alone.
func TestCompactPacks(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	s := openStore(t, dir, key)
	s.PackSize = 1 << 40 // sealed by hand below
	put := func(from, to int) []ID {
		var ids []ID
		for i := from; i < to; i++ {
			id, err := s.Put(chunk(i))
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		s.Seal()
		return ids
	}
	dead := put(0, 50)      // nothing referenced
	sparse := put(100, 150) // 10 % referenced
	full := put(200, 250)   // everything referenced
	live := map[ID]struct{}{}
	for _, id := range sparse[:5] {
		live[id] = struct{}{}
	}
	for _, id := range full {
		live[id] = struct{}{}
	}
	fullFile, _, _, _ := s.Location(full[0])
	age(t, s, 24*time.Hour)
	young := put(300, 320) // unreferenced, but new

	before := countFiles(dir, "packs/*/*.pack")
	st, err := s.Compact(live, time.Now().Add(-6*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Repacked != 5 || st.Freed == 0 {
		t.Fatalf("Ergebnis: %+v", st)
	}
	if after := countFiles(dir, "packs/*/*.pack"); before != 4 || after != 3 {
		t.Fatalf("Packs vorher %d, nachher %d", before, after)
	}
	for _, id := range dead {
		if s.Has(id) {
			t.Fatal("Chunk aus gelöschtem Pack noch vorhanden")
		}
	}
	for _, id := range sparse[5:] {
		if s.Has(id) {
			t.Fatal("nicht referenzierter Chunk nach dem Umpacken noch vorhanden")
		}
	}
	for _, id := range append(append(append([]ID{}, sparse[:5]...), full...), young...) {
		if err := s.Verify(id); err != nil {
			t.Fatalf("benötigter Chunk verloren: %v", err)
		}
	}
	if f, _, _, _ := s.Location(full[0]); f != fullFile {
		t.Fatal("volles Pack wurde unnötig umgeschrieben")
	}
	if st.Chunks != int64(5+len(full)+len(young)) {
		t.Fatalf("%d Chunks gezählt", st.Chunks)
	}
	// the result holds after a restart
	s = openStore(t, dir, key)
	for _, id := range append(append([]ID{}, sparse[:5]...), full...) {
		if err := s.Verify(id); err != nil {
			t.Fatalf("nach Neustart: %v", err)
		}
	}
	for _, id := range dead {
		if s.Has(id) {
			t.Fatal("gelöschter Chunk nach Neustart wieder da")
		}
	}
}

// Delete and Quarantine take a chunk out for good, also across a restart.
func TestDeleteFromPack(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	rand.Read(key)
	s := openStore(t, dir, key)
	a, _ := s.Put(chunk(1))
	b, _ := s.Put(chunk(2))
	c, _ := s.Put(chunk(3))
	if err := s.Delete(a); err != nil || s.Has(a) { // pack still open
		t.Fatal("Löschen aus offenem Pack", err)
	}
	s.Seal()
	if err := s.Quarantine(b); err != nil || s.Has(b) {
		t.Fatal("Quarantäne", err)
	}
	if countFiles(dir, "quarantine/*") != 1 {
		t.Fatal("Chunk nicht in Quarantäne abgelegt")
	}
	s = openStore(t, dir, key)
	if s.Has(a) || s.Has(b) || s.Verify(c) != nil {
		t.Fatal("Zustand nach Neustart falsch")
	}
	// the chunk can be stored again
	if _, err := s.Put(chunk(2)); err != nil || s.Verify(b) != nil {
		t.Fatal("erneutes Speichern", err)
	}
}

// CopyTo between stores, one of them still in the old format.
func TestCopyBetweenFormats(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	src := openStore(t, t.TempDir(), key)
	dst := openStore(t, t.TempDir(), key)
	loose := writeLoose(t, src, chunk(1))
	packed, _ := src.Put(chunk(2))
	for _, id := range []ID{loose, packed} {
		if n, err := src.CopyTo(dst, id); err != nil || n == 0 {
			t.Fatal(err)
		}
		if err := dst.Verify(id); err != nil {
			t.Fatal(err)
		}
	}
	chunks, bytes := dst.Stats()
	if chunks != 2 || bytes == 0 {
		t.Fatalf("Stats: %d Chunks, %d Bytes", chunks, bytes)
	}
}
