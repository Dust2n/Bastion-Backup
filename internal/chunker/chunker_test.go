package chunker

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"testing"
)

func chunks(t *testing.T, data []byte) [][32]byte {
	t.Helper()
	c := New(bytes.NewReader(data))
	var out [][32]byte
	total := 0
	for {
		b, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > MaxSize {
			t.Fatalf("chunk too large: %d", len(b))
		}
		total += len(b)
		out = append(out, sha256.Sum256(b))
	}
	if total != len(data) {
		t.Fatalf("lost data: %d != %d", total, len(data))
	}
	return out
}

func TestDeterministicAndShiftResistant(t *testing.T) {
	data := make([]byte, 40<<20)
	rand.New(rand.NewSource(1)).Read(data)

	a := chunks(t, data)
	b := chunks(t, data)
	if len(a) != len(b) {
		t.Fatal("not deterministic")
	}
	avg := len(data) / len(a)
	if avg < MinSize || avg > 2*AvgSize {
		t.Fatalf("unexpected average chunk size %d", avg)
	}

	// Insert bytes near the beginning: most chunks must still be shared.
	shifted := append(append(append([]byte{}, data[:1000]...), []byte("inserted bytes")...), data[1000:]...)
	c := chunks(t, shifted)
	seen := map[[32]byte]bool{}
	for _, h := range a {
		seen[h] = true
	}
	shared := 0
	for _, h := range c {
		if seen[h] {
			shared++
		}
	}
	if shared < len(a)-3 {
		t.Fatalf("only %d of %d chunks shared after insert", shared, len(a))
	}
}

func TestSmallAndEmpty(t *testing.T) {
	if got := chunks(t, nil); len(got) != 0 {
		t.Fatal("empty input should give no chunks")
	}
	if got := chunks(t, []byte("hello")); len(got) != 1 {
		t.Fatal("small input should give one chunk")
	}
}
