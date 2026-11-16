package repo

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"testing"
)

func testStore(t *testing.T) *Store {
	key := make([]byte, 32)
	rand.Read(key)
	s, err := Open(t.TempDir(), key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// flipByte damages the last byte of a stored chunk.
func flipByte(t *testing.T, s *Store, id ID) {
	t.Helper()
	file, off, n, ok := s.Location(id)
	if !ok {
		t.Fatal("chunk not stored")
	}
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 1)
	f.ReadAt(b, off+int64(n)-1)
	b[0] ^= 1
	f.WriteAt(b, off+int64(n)-1)
}

func TestRoundTripAndDedup(t *testing.T) {
	s := testStore(t)
	data := bytes.Repeat([]byte("bastion "), 100000)
	id, err := s.Put(data)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.Put(data)
	if id != id2 {
		t.Fatal("same content must give same id")
	}
	n := 0
	s.Walk(func(ChunkInfo) error { n++; return nil })
	if n != 1 {
		t.Fatalf("expected 1 stored chunk, got %d", n)
	}
	got, err := s.Get(id)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("round trip failed", err)
	}
	if err := s.Verify(id); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTo(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, _ := Open(t.TempDir(), key)
	b, _ := Open(t.TempDir(), key)
	id, _ := a.Put([]byte("verschieben"))
	if n, err := a.CopyTo(b, id); err != nil || n == 0 {
		t.Fatal("copy failed", err)
	}
	if err := b.Verify(id); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.CopyTo(b, id); n != 0 {
		t.Fatal("existing chunk must not be copied again")
	}
	other := make([]byte, 32)
	rand.Read(other)
	c, _ := Open(t.TempDir(), other)
	a.CopyTo(c, id)
	if c.Verify(id) == nil {
		t.Fatal("store with another key must not accept the chunk")
	}
}

func TestRejectsWrongContent(t *testing.T) {
	s := testStore(t)
	id := Hash([]byte("a"))
	if err := s.PutCompressed(id, Compress([]byte("b"))); err == nil {
		t.Fatal("expected hash mismatch")
	}
}

func TestDetectsTampering(t *testing.T) {
	s := testStore(t)
	id, _ := s.Put([]byte("important"))
	flipByte(t, s, id)
	if _, err := s.Get(id); err == nil {
		t.Fatal("tampering not detected")
	}
}

func TestTreeRoundTrip(t *testing.T) {
	s := testStore(t)
	var buf bytes.Buffer
	w := NewTreeWriter(&buf)
	w.Write(&Node{Path: "", Type: TypeDir})
	w.Write(&Node{Path: "a.txt", Type: TypeFile, Size: 3, Inline: []byte("abc")})
	w.Flush()
	id, _ := s.Put(buf.Bytes())
	var got []*Node
	err := ReadTree(NewChunkReader([]ID{id}, s.Get), func(n *Node) error { got = append(got, n); return nil })
	if err != nil || len(got) != 2 || string(got[1].Inline) != "abc" {
		t.Fatal("tree round trip failed", err)
	}
	r := NewChunkReader([]ID{id, id}, s.Get)
	all, _ := io.ReadAll(r)
	if len(all) != 2*buf.Len() {
		t.Fatal("chunk reader length")
	}
}
