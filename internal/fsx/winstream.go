package fsx

import (
	"encoding/binary"
	"errors"
	"io"
)

// Stream ids of the Win32 backup format (WIN32_STREAM_ID).
const (
	BackupData        = 1
	BackupSecurity    = 3
	BackupAltData     = 4
	BackupReparseData = 8
	BackupSparseBlock = 9
)

// ExtractData copies the main data stream (the file content) out of a
// BackupRead stream. It works on every platform, so the server can offer
// file downloads from Windows backups.
func ExtractData(r io.Reader, w io.Writer) error {
	var hdr [20]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		id := binary.LittleEndian.Uint32(hdr[0:])
		size := int64(binary.LittleEndian.Uint64(hdr[8:]))
		nameSize := int64(binary.LittleEndian.Uint32(hdr[16:]))
		if _, err := io.CopyN(io.Discard, r, nameSize); err != nil {
			return err
		}
		switch id {
		case BackupData:
			if _, err := io.CopyN(w, r, size); err != nil {
				return err
			}
		case BackupSparseBlock:
			var off [8]byte
			if _, err := io.ReadFull(r, off[:]); err != nil {
				return err
			}
			if wa, ok := w.(io.WriterAt); ok {
				if _, err := io.Copy(&offsetWriter{wa, int64(binary.LittleEndian.Uint64(off[:]))}, io.LimitReader(r, size-8)); err != nil {
					return err
				}
			} else if _, err := io.CopyN(w, r, size-8); err != nil {
				return err
			}
		default:
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return err
			}
		}
	}
}

type offsetWriter struct {
	w   io.WriterAt
	off int64
}

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.w.WriteAt(p, o.off)
	o.off += int64(n)
	return n, err
}
