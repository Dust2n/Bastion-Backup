// Package repo is the deduplicating, compressed and encrypted chunk store
// plus the snapshot/tree formats that reference its chunks.
package repo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

var magic = []byte("BSC1")

var ErrNotFound = errors.New("chunk not found")

var (
	// EncodeAll may be called from several goroutines; the default
	// concurrency (number of CPUs) lets the upload workers compress in
	// parallel instead of queueing behind one encoder.
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(64<<20))
)

// Compress zstd-compresses a chunk (used by agents before upload).
func Compress(b []byte) []byte { return zenc.EncodeAll(b, make([]byte, 0, len(b)/2)) }

// Decompress reverses Compress.
func Decompress(b []byte) ([]byte, error) { return zdec.DecodeAll(b, nil) }

// Store keeps chunks in pack files.
//
// A chunk is stored as a blob: magic | nonce | AES-256-GCM(zstd(plaintext))
// with the chunk ID as additional data, so a chunk can not be swapped for
// another one.
//
//	packs/ab/<pack>.pack   "BPK1", then records: chunk ID | blob length | blob
//	packs/ab/<pack>.idx    where each chunk of a finished pack lies
//	chunks/ab/cd/<id>      format of versions up to 0.3.2: one file per
//	                       chunk, holding just the blob. Still read; Compact
//	                       moves these into packs.
//
// One file per chunk meant one create, one fsync and one rename per chunk
// and millions of small files on big repositories. Packs are appended to
// and synced when a backup is committed (Flush), and which chunks exist is
// answered from memory instead of from the file system.
//
// A pack is self-describing: without its index it is read record by record
// (recoverPack), so a lost index or a crash while writing costs nothing
// that had been committed.
type Store struct {
	dir  string
	aead cipher.AEAD

	// PackSize is the size at which a pack is finished and a new one begun.
	PackSize int64

	mu       sync.RWMutex
	idx      map[ID]loc
	packs    map[packID]int64 // finished packs and their size
	open     *openPack
	hasLoose bool // loose chunk files may exist
}

type packID [16]byte

func (p packID) String() string { return hex.EncodeToString(p[:]) }

// loc is where the blob of a chunk lies.
type loc struct {
	pack packID
	off  int64
	n    uint32
}

type idxEntry struct {
	id  ID
	off int64
	n   uint32
}

type openPack struct {
	id      packID
	f       *os.File
	size    int64
	entries []idxEntry
	dirty   bool // written to since the last sync
}

const (
	packMagic      = "BPK1"
	idxMagic       = "BPI1"
	recHeader      = 32 + 4
	maxBlob        = 32 << 20
	defaultPackLen = 64 << 20
)

// Open opens (and creates) a store. key must be 32 bytes.
func Open(dir string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("repository key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"chunks", "packs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, aead: aead, PackSize: defaultPackLen, idx: map[ID]loc{}, packs: map[packID]int64{}}
	if err := s.loadPacks(); err != nil {
		return nil, err
	}
	s.hasLoose = dirHasEntries(filepath.Join(dir, "chunks"))
	return s, nil
}

func dirHasEntries(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// ---------- paths ----------

func (s *Store) loosePath(id ID) string {
	h := id.String()
	return filepath.Join(s.dir, "chunks", h[:2], h[2:4], h)
}

func (s *Store) packPath(p packID) string {
	h := p.String()
	return filepath.Join(s.dir, "packs", h[:2], h+".pack")
}

func (s *Store) idxPath(p packID) string {
	h := p.String()
	return filepath.Join(s.dir, "packs", h[:2], h+".idx")
}

// ---------- loading ----------

// loadPacks reads the index of every pack. A pack without a valid index
// (crash before it was finished, index lost) is read record by record.
func (s *Store) loadPacks() error {
	root := filepath.Join(s.dir, "packs")
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".pack") {
			if !strings.HasSuffix(name, ".idx") {
				// leftover temporary file of an interrupted write
				if info, ierr := d.Info(); ierr == nil && time.Since(info.ModTime()) > 24*time.Hour {
					os.Remove(p)
				}
			}
			return nil
		}
		raw, err := hex.DecodeString(strings.TrimSuffix(name, ".pack"))
		if err != nil || len(raw) != 16 {
			return nil
		}
		var id packID
		copy(id[:], raw)
		entries, err := readIdx(s.idxPath(id))
		if err != nil {
			if entries, err = s.recoverPack(id); err != nil {
				return fmt.Errorf("pack %s: %w", id, err)
			}
		}
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // empty pack, removed by recoverPack
		}
		if err != nil {
			return err
		}
		s.packs[id] = info.Size()
		for _, e := range entries {
			s.idx[e.id] = loc{pack: id, off: e.off, n: e.n}
		}
		return nil
	})
}

// recoverPack rebuilds the index of a pack from its records. Every record
// is decrypted, so nothing half-written gets into the index: a later
// backup may rely on a chunk the store reports as present. An incomplete
// record at the end (crash while appending) is cut off. An empty pack is
// removed.
func (s *Store) recoverPack(id packID) ([]idxEntry, error) {
	p := s.packPath(id)
	f, err := os.OpenFile(p, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	head := make([]byte, len(packMagic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != packMagic {
		if size <= int64(len(packMagic)) {
			f.Close()
			os.Remove(p)
			return nil, nil
		}
		return nil, errors.New("kein Bastion-Pack (Kennung fehlt)")
	}
	var entries []idxEntry
	off := int64(len(packMagic))
	hdr := make([]byte, recHeader)
	for off+recHeader <= size {
		if _, err := f.ReadAt(hdr, off); err != nil {
			break
		}
		var cid ID
		copy(cid[:], hdr[:32])
		n := binary.LittleEndian.Uint32(hdr[32:])
		if int(n) < len(magic)+s.aead.NonceSize()+s.aead.Overhead() || n > maxBlob || off+recHeader+int64(n) > size {
			break // not a complete record: the rest was never finished
		}
		blob := make([]byte, n)
		if _, err := f.ReadAt(blob, off+recHeader); err != nil {
			break
		}
		// a record that does not decrypt is left out, the ones after it are kept
		if _, err := s.openBlob(cid, blob); err == nil {
			entries = append(entries, idxEntry{id: cid, off: off + recHeader, n: n})
		}
		off += recHeader + int64(n)
	}
	if off < size {
		if err := f.Truncate(off); err != nil {
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	f.Close()
	if len(entries) == 0 && off == int64(len(packMagic)) {
		os.Remove(p)
		return nil, nil
	}
	return entries, writeIdx(s.idxPath(id), entries)
}

// The index of a pack: magic | count | (chunk ID | offset | length)* | CRC.
func writeIdx(p string, entries []idxEntry) error {
	buf := make([]byte, 0, 8+len(entries)*44+4)
	buf = append(buf, idxMagic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries)))
	for _, e := range entries {
		buf = append(buf, e.id[:]...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.off))
		buf = binary.LittleEndian.AppendUint32(buf, e.n)
	}
	buf = binary.LittleEndian.AppendUint32(buf, checksum(buf))
	return writeDurable(p, buf)
}

func readIdx(p string) ([]idxEntry, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[:4]) != idxMagic {
		return nil, errors.New("index damaged")
	}
	n := int(binary.LittleEndian.Uint32(b[4:]))
	if len(b) != 8+n*44+4 || binary.LittleEndian.Uint32(b[len(b)-4:]) != checksum(b[:len(b)-4]) {
		return nil, errors.New("index damaged")
	}
	entries := make([]idxEntry, n)
	for i := range entries {
		rec := b[8+i*44:]
		copy(entries[i].id[:], rec[:32])
		entries[i].off = int64(binary.LittleEndian.Uint64(rec[32:]))
		entries[i].n = binary.LittleEndian.Uint32(rec[40:])
	}
	return entries, nil
}

// checksum is FNV-1a; it only has to notice a damaged index file, which is
// then rebuilt from the pack.
func checksum(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// ---------- queries ----------

// minChunkFile is the size of an encrypted empty chunk; a shorter loose
// chunk file is a torn write of an old version and counts as missing.
var minChunkFile = int64(len(magic) + 12 + 16)

func (s *Store) Has(id ID) bool {
	s.mu.RLock()
	_, ok := s.idx[id]
	loose := s.hasLoose
	s.mu.RUnlock()
	if ok || !loose {
		return ok
	}
	fi, err := os.Stat(s.loosePath(id))
	return err == nil && fi.Size() >= minChunkFile
}

// Put stores plaintext data and returns its ID.
func (s *Store) Put(data []byte) (ID, error) {
	id := Hash(data)
	if s.Has(id) {
		return id, nil
	}
	return id, s.write(id, Compress(data))
}

// PutCompressed stores a zstd-compressed chunk received from an agent after
// verifying that it really decompresses to content with the given ID.
func (s *Store) PutCompressed(id ID, z []byte) error {
	plain, err := Decompress(z)
	if err != nil {
		return fmt.Errorf("chunk %s: invalid compression: %w", id, err)
	}
	if Hash(plain) != id {
		return fmt.Errorf("chunk %s: content hash mismatch", id)
	}
	if s.Has(id) {
		return nil
	}
	return s.write(id, z)
}

func (s *Store) write(id ID, z []byte) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	blob := make([]byte, 0, len(magic)+len(nonce)+len(z)+s.aead.Overhead())
	blob = append(blob, magic...)
	blob = append(blob, nonce...)
	blob = s.aead.Seal(blob, nonce, z, id[:])
	return s.appendBlob(id, blob, false)
}

// appendBlob adds a blob to the open pack. With move it is written even if
// the chunk is already indexed (Compact moving it out of an old pack).
func (s *Store) appendBlob(id ID, blob []byte, move bool) error {
	if len(blob) > maxBlob {
		return fmt.Errorf("chunk %s: too large", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx[id]; ok && !move {
		return nil
	}
	if s.open == nil {
		if err := s.newPack(); err != nil {
			return err
		}
	}
	o := s.open
	rec := make([]byte, 0, recHeader+len(blob))
	rec = append(rec, id[:]...)
	rec = binary.LittleEndian.AppendUint32(rec, uint32(len(blob)))
	rec = append(rec, blob...)
	if _, err := o.f.WriteAt(rec, o.size); err != nil {
		// e.g. disk full: take the partial record back, the pack stays usable
		o.f.Truncate(o.size)
		return err
	}
	e := idxEntry{id: id, off: o.size + recHeader, n: uint32(len(blob))}
	o.entries = append(o.entries, e)
	o.size += int64(len(rec))
	o.dirty = true
	s.idx[id] = loc{pack: o.id, off: e.off, n: e.n}
	if o.size >= s.PackSize {
		return s.sealLocked()
	}
	return nil
}

func (s *Store) newPack() error {
	var id packID
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	p := s.packPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(packMagic)); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	s.open = &openPack{id: id, f: f, size: int64(len(packMagic)), dirty: true}
	return nil
}

// sealLocked finishes the open pack: data on disk, then the index.
func (s *Store) sealLocked() error {
	o := s.open
	if o == nil {
		return nil
	}
	if err := o.f.Sync(); err != nil {
		return err
	}
	o.dirty = false
	if len(o.entries) == 0 {
		o.f.Close()
		os.Remove(s.packPath(o.id))
		s.open = nil
		return nil
	}
	if err := writeIdx(s.idxPath(o.id), o.entries); err != nil {
		return err
	}
	if err := o.f.Close(); err != nil {
		return err
	}
	s.packs[o.id] = o.size
	s.open = nil
	return nil
}

// Flush makes everything stored so far durable. The server calls it before
// it accepts a backup, so a committed backup never refers to data that a
// power failure could still take away.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil || !s.open.dirty {
		return nil
	}
	if err := s.open.f.Sync(); err != nil {
		return err
	}
	s.open.dirty = false
	return nil
}

// Seal finishes the pack that is being written; the next chunk starts a
// new one.
func (s *Store) Seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealLocked()
}

// Close finishes the open pack. The store must not be used afterwards.
func (s *Store) Close() error { return s.Seal() }

// writeDurable writes a file so that after a crash it is either complete
// under its final name or absent: data is flushed to disk before the
// rename, and the rename itself is flushed where the OS allows it.
func writeDurable(p string, data []byte) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp%d", p, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// directory entry (Linux); Windows can not open directories this way
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// ---------- reading ----------

// readBlob returns the stored blob of a chunk.
func (s *Store) readBlob(id ID) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		s.mu.RLock()
		l, ok := s.idx[id]
		var blob []byte
		var err error
		if ok && s.open != nil && s.open.id == l.pack {
			// still being written: read through the open file
			blob = make([]byte, l.n)
			_, err = s.open.f.ReadAt(blob, l.off)
			s.mu.RUnlock()
			if err != nil {
				return nil, err
			}
			return blob, nil
		}
		s.mu.RUnlock()
		if !ok {
			b, err := os.ReadFile(s.loosePath(id))
			if errors.Is(err, fs.ErrNotExist) {
				return nil, ErrNotFound
			}
			return b, err
		}
		f, err := os.Open(s.packPath(l.pack))
		if err != nil {
			// Compact may have moved the chunk to another pack meanwhile
			if errors.Is(err, fs.ErrNotExist) && attempt == 0 {
				continue
			}
			return nil, fmt.Errorf("chunk %s: %w", id, err)
		}
		blob = make([]byte, l.n)
		_, err = f.ReadAt(blob, l.off)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("chunk %s: %w", id, err)
		}
		return blob, nil
	}
}

func (s *Store) openBlob(id ID, b []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(b) < len(magic)+ns || !bytes.Equal(b[:len(magic)], magic) {
		return nil, fmt.Errorf("chunk %s: corrupt header", id)
	}
	z, err := s.aead.Open(nil, b[len(magic):len(magic)+ns], b[len(magic)+ns:], id[:])
	if err != nil {
		return nil, fmt.Errorf("chunk %s: authentication failed (corrupt or wrong key)", id)
	}
	return z, nil
}

// GetCompressed returns the zstd-compressed chunk (what agents download).
func (s *Store) GetCompressed(id ID) ([]byte, error) {
	b, err := s.readBlob(id)
	if err != nil {
		return nil, err
	}
	return s.openBlob(id, b)
}

func (s *Store) Get(id ID) ([]byte, error) {
	z, err := s.GetCompressed(id)
	if err != nil {
		return nil, err
	}
	return Decompress(z)
}

// Verify fully decrypts and hashes a chunk.
func (s *Store) Verify(id ID) error {
	b, err := s.Get(id)
	if err != nil {
		return err
	}
	if Hash(b) != id {
		return fmt.Errorf("chunk %s: content hash mismatch", id)
	}
	return nil
}

// ---------- removing ----------

// Delete removes a chunk from the store. In a pack only the index entry
// goes; Compact reclaims the space.
func (s *Store) Delete(id ID) error {
	s.mu.Lock()
	l, ok := s.idx[id]
	if !ok {
		s.mu.Unlock()
		return os.Remove(s.loosePath(id))
	}
	defer s.mu.Unlock()
	delete(s.idx, id)
	if s.open != nil && s.open.id == l.pack {
		for i, e := range s.open.entries {
			if e.id == id {
				s.open.entries = append(s.open.entries[:i], s.open.entries[i+1:]...)
				break
			}
		}
		return nil
	}
	// the index file must forget the chunk too, or it is back after a restart
	entries, err := readIdx(s.idxPath(l.pack))
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.id != id {
			kept = append(kept, e)
		}
	}
	return writeIdx(s.idxPath(l.pack), kept)
}

// Quarantine takes a damaged chunk out of the store, so Has reports it as
// missing and the next full read of the machine uploads it again. What can
// still be read of it is kept in quarantine/ for analysis.
func (s *Store) Quarantine(id ID) error {
	dir := filepath.Join(s.dir, "quarantine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s.%d", id, time.Now().Unix()))
	s.mu.RLock()
	_, packed := s.idx[id]
	s.mu.RUnlock()
	if !packed {
		return os.Rename(s.loosePath(id), dst)
	}
	if blob, err := s.readBlob(id); err == nil {
		os.WriteFile(dst, blob, 0o600)
	}
	return s.Delete(id)
}

// Location tells where the blob of a chunk lies: the file, the offset in
// it and its length (for diagnosis and tests).
func (s *Store) Location(id ID) (file string, off int64, n int, ok bool) {
	s.mu.RLock()
	l, packed := s.idx[id]
	s.mu.RUnlock()
	if packed {
		return s.packPath(l.pack), l.off, int(l.n), true
	}
	p := s.loosePath(id)
	fi, err := os.Stat(p)
	if err != nil {
		return "", 0, 0, false
	}
	return p, 0, int(fi.Size()), true
}

// Dir is the directory of the store.
func (s *Store) Dir() string { return s.dir }

// CopyTo copies a chunk unchanged (still encrypted) into dst. Both stores
// must use the same key, which Verify on dst confirms.
func (s *Store) CopyTo(dst *Store, id ID) (int64, error) {
	if dst.Has(id) {
		return 0, nil
	}
	b, err := s.readBlob(id)
	if err != nil {
		return 0, err
	}
	if err := dst.appendBlob(id, b, false); err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

// ChunkInfo is passed to Walk callbacks.
type ChunkInfo struct {
	ID      ID
	Size    int64
	ModTime time.Time
}

// Walk calls fn for every stored chunk.
func (s *Store) Walk(fn func(ChunkInfo) error) error {
	s.mu.RLock()
	list := make([]ChunkInfo, 0, len(s.idx))
	mods := map[packID]time.Time{}
	for id, l := range s.idx {
		mod, ok := mods[l.pack]
		if !ok {
			mod = time.Now()
			if fi, err := os.Stat(s.packPath(l.pack)); err == nil {
				mod = fi.ModTime()
			}
			mods[l.pack] = mod
		}
		list = append(list, ChunkInfo{ID: id, Size: int64(l.n), ModTime: mod})
	}
	loose := s.hasLoose
	s.mu.RUnlock()
	for _, c := range list {
		if err := fn(c); err != nil {
			return err
		}
	}
	if !loose {
		return nil
	}
	return s.walkLoose(func(c ChunkInfo, _ string) error {
		s.mu.RLock()
		_, packed := s.idx[c.ID]
		s.mu.RUnlock()
		if packed {
			return nil // also in a pack (torn loose file, or migration in progress)
		}
		return fn(c)
	})
}

// walkLoose visits the chunk files of the old format.
func (s *Store) walkLoose(fn func(ChunkInfo, string) error) error {
	root := filepath.Join(s.dir, "chunks")
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		id, perr := ParseID(d.Name())
		if perr != nil {
			// leftover temp file from an interrupted write
			if info, ierr := d.Info(); ierr == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(p)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		return fn(ChunkInfo{ID: id, Size: info.Size(), ModTime: info.ModTime()}, p)
	})
}

// Stats counts the stored chunks and the space they take on disk.
func (s *Store) Stats() (chunks, bytes int64) {
	s.mu.RLock()
	chunks = int64(len(s.idx))
	for _, size := range s.packs {
		bytes += size
	}
	if s.open != nil {
		bytes += s.open.size
	}
	loose := s.hasLoose
	s.mu.RUnlock()
	if loose {
		s.walkLoose(func(c ChunkInfo, _ string) error {
			s.mu.RLock()
			_, packed := s.idx[c.ID]
			s.mu.RUnlock()
			if !packed {
				chunks++
			}
			bytes += c.Size
			return nil
		})
	}
	return chunks, bytes
}
