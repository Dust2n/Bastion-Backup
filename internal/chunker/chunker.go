// Package chunker implements content-defined chunking (FastCDC with
// normalized chunking). Identical data produces identical chunk boundaries,
// even when bytes are inserted or removed earlier in the stream, which is
// what makes deduplication across backups and machines work.
package chunker

import (
	"errors"
	"io"
)

const (
	MinSize = 256 << 10
	AvgSize = 1 << 20
	MaxSize = 4 << 20
)

// maskS is used before the average size (harder to match, 22 bits),
// maskL after it (easier to match, 18 bits). High bits are used because
// they depend on a longer window of input bytes.
const (
	maskS uint64 = ((1 << 22) - 1) << 40
	maskL uint64 = ((1 << 18) - 1) << 44
)

var gear [256]uint64

func init() {
	// Deterministic table: must never change, otherwise chunk boundaries
	// (and therefore deduplication against existing backups) change.
	x := uint64(0x6a09e667f3bcc908)
	for i := range gear {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		gear[i] = z ^ (z >> 31)
	}
}

// Chunker splits a stream into content-defined chunks.
type Chunker struct {
	r          io.Reader
	buf        []byte
	start, end int
	eof        bool
}

func New(r io.Reader) *Chunker {
	return &Chunker{r: r, buf: make([]byte, 2*MaxSize)}
}

// Reset reuses the chunker (and its buffer) for a new stream.
func (c *Chunker) Reset(r io.Reader) {
	c.r, c.start, c.end, c.eof = r, 0, 0, false
}

// Next returns the next chunk. The returned slice is only valid until the
// next call. At the end of the stream it returns io.EOF.
func (c *Chunker) Next() ([]byte, error) {
	if c.end-c.start < MaxSize && !c.eof {
		if err := c.fill(); err != nil {
			return nil, err
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := Cut(c.buf[c.start:c.end])
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}

func (c *Chunker) fill() error {
	copy(c.buf, c.buf[c.start:c.end])
	c.end -= c.start
	c.start = 0
	for c.end < len(c.buf) {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if errors.Is(err, io.EOF) {
			c.eof = true
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Cut returns the length of the first chunk in data.
func Cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := AvgSize
	if n < normal {
		normal = n
	}
	var fp uint64
	i := MinSize
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[data[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	return n
}
