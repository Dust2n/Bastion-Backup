package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ID identifies a chunk by the SHA-256 of its plaintext content.
type ID [32]byte

func Hash(b []byte) ID { return ID(sha256.Sum256(b)) }

func (id ID) String() string { return hex.EncodeToString(id[:]) }

func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *ID) UnmarshalText(b []byte) error {
	p, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*id = p
	return nil
}

func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 64 {
		return id, fmt.Errorf("invalid chunk id %q", s)
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("invalid chunk id %q", s)
	}
	return id, nil
}
