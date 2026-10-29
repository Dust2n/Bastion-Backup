// Package repo is the deduplicating, compressed and encrypted chunk store
// plus the snapshot/tree formats that reference its chunks.
package repo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
)

var magic = []byte("BSC1")

var ErrNotFound = errors.New("chunk not found")

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderMaxMemory(64<<20))
)

// Compress zstd-compresses a chunk (used by agents before upload).
func Compress(b []byte) []byte { return zenc.EncodeAll(b, make([]byte, 0, len(b)/2)) }

// Decompress reverses Compress.
func Decompress(b []byte) ([]byte, error) { return zdec.DecodeAll(b, nil) }

// Store keeps chunks as files: chunks/ab/cd/<id>. Each file holds
// magic | nonce | AES-256-GCM(zstd(plaintext)) with the chunk ID as
// additional data, so a chunk can not be swapped for another one.
type Store struct {
	dir  string
	aead cipher.AEAD
}

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
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, aead: aead}, nil
}

func (s *Store) path(id ID) string {
	h := id.String()
	return filepath.Join(s.dir, "chunks", h[:2], h[2:4], h)
}

func (s *Store) Has(id ID) bool {
	_, err := os.Stat(s.path(id))
	return err == nil
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
	out := make([]byte, 0, len(magic)+len(nonce)+len(z)+s.aead.Overhead())
	out = append(out, magic...)
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, z, id[:])

	p := s.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// GetCompressed returns the zstd-compressed chunk (what agents download).
func (s *Store) GetCompressed(id ID) ([]byte, error) {
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
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

func (s *Store) Delete(id ID) error { return os.Remove(s.path(id)) }

// Dir is the directory of the store.
func (s *Store) Dir() string { return s.dir }

// CopyTo copies a chunk file unchanged (still encrypted) into dst. Both
// stores must use the same key, which Verify on dst confirms.
func (s *Store) CopyTo(dst *Store, id ID) (int64, error) {
	if dst.Has(id) {
		return 0, nil
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	p := dst.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	tmp := fmt.Sprintf("%s.tmp%d", p, time.Now().UnixNano())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
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
		return fn(ChunkInfo{ID: id, Size: info.Size(), ModTime: info.ModTime()})
	})
}
