package repo

import (
	"os"
	"testing"
)

// A chunk file left empty or cut short by a crash counts as missing and
// is replaced by the next upload.
func TestTornChunkIsMissing(t *testing.T) {
	key := make([]byte, 32)
	s, err := Open(t.TempDir(), key)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("Inhalt eines Blocks")
	id := Hash(data)
	p := s.path(id)
	os.MkdirAll(p[:len(p)-len(id.String())-1], 0o700)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Has(id) {
		t.Fatal("leere Chunk-Datei gilt als vorhanden")
	}
	if _, err := s.Put(data); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil || string(got) != string(data) {
		t.Fatalf("nach erneutem Schreiben: %q %v", got, err)
	}
}
