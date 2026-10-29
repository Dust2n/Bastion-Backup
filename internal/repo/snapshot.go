package repo

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"

	"bastion/internal/layout"
)

// Snapshot is one backup of one machine.
type Snapshot struct {
	ID       string          `json:"id"`
	AgentID  string          `json:"agent_id"`
	Hostname string          `json:"hostname"`
	OS       string          `json:"os"`
	PlanID   int64           `json:"plan_id"`
	Kind     string          `json:"kind"` // image | files
	Time     time.Time       `json:"time"`
	Parent   string          `json:"parent,omitempty"`
	Layout   *layout.Machine `json:"layout,omitempty"`
	Volumes  []Volume        `json:"volumes"`
	Warnings []string        `json:"warnings,omitempty"`
}

// Volume is one backed-up file system (or a folder for file backups).
type Volume struct {
	Key       string `json:"key"`  // "C:", "/", "/home", or a folder path
	Root      string `json:"root"` // path that was read on the source
	FS        string `json:"fs,omitempty"`
	Label     string `json:"label,omitempty"`
	UUID      string `json:"uuid,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Used      int64  `json:"used,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"` // vss | lvm | btrfs | none
	Tree      []ID   `json:"tree"`               // chunks of the JSONL tree
	Files     int64  `json:"files"`
	Bytes     int64  `json:"bytes"`
	NewBytes  int64  `json:"new_bytes"`
	Errors    int64  `json:"errors"`
	WinStream bool   `json:"win_stream,omitempty"` // file content is BackupRead format
}

// Node types.
const (
	TypeFile     = "f"
	TypeDir      = "d"
	TypeSymlink  = "l"
	TypeHardlink = "h"
	TypeChar     = "c"
	TypeBlock    = "b"
	TypeFifo     = "p"
	TypeSocket   = "s"
)

// Node is one entry of a volume tree. Trees are stored as JSON lines in
// depth-first pre-order (a directory always comes before its children).
type Node struct {
	Path   string            `json:"p"` // relative, "/" separated, "" is the root
	Type   string            `json:"t"`
	Size   int64             `json:"s,omitempty"` // logical file size
	Mode   uint32            `json:"a,omitempty"` // unix mode or Windows attributes
	Mtime  int64             `json:"m,omitempty"` // unix nanoseconds
	Atime  int64             `json:"at,omitempty"`
	Ctime  int64             `json:"ct,omitempty"` // change time
	Btime  int64             `json:"bt,omitempty"` // creation time (Windows)
	UID    uint32            `json:"u,omitempty"`
	GID    uint32            `json:"g,omitempty"`
	Rdev   uint64            `json:"r,omitempty"`
	Link   string            `json:"l,omitempty"` // symlink target, or hardlink target path
	Short  string            `json:"sn,omitempty"`
	FileID string            `json:"fid,omitempty"`
	Xattrs map[string][]byte `json:"x,omitempty"`
	// Content: either inline (small streams) or a list of chunks.
	Inline     []byte `json:"i,omitempty"`
	Chunks     []ID   `json:"c,omitempty"`
	StreamSize int64  `json:"ss,omitempty"`
}

// Name returns the last path element.
func (n *Node) Name() string {
	if i := strings.LastIndexByte(n.Path, '/'); i >= 0 {
		return n.Path[i+1:]
	}
	return n.Path
}

// Dir returns the parent path.
func (n *Node) Dir() string {
	if i := strings.LastIndexByte(n.Path, '/'); i >= 0 {
		return n.Path[:i]
	}
	return ""
}

// TreeWriter writes nodes as JSON lines.
type TreeWriter struct {
	w   *bufio.Writer
	enc *json.Encoder
}

func NewTreeWriter(w io.Writer) *TreeWriter {
	bw := bufio.NewWriterSize(w, 1<<20)
	return &TreeWriter{w: bw, enc: json.NewEncoder(bw)}
}

func (t *TreeWriter) Write(n *Node) error { return t.enc.Encode(n) }
func (t *TreeWriter) Flush() error        { return t.w.Flush() }

// ReadTree calls fn for every node of a JSONL tree.
func ReadTree(r io.Reader, fn func(*Node) error) error {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	for {
		var n Node
		if err := dec.Decode(&n); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if err := fn(&n); err != nil {
			return err
		}
	}
}

// ChunkReader streams the concatenated content of a list of chunks.
type ChunkReader struct {
	ids  []ID
	get  func(ID) ([]byte, error)
	cur  []byte
	next int
}

func NewChunkReader(ids []ID, get func(ID) ([]byte, error)) *ChunkReader {
	return &ChunkReader{ids: ids, get: get}
}

func (r *ChunkReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.next >= len(r.ids) {
			return 0, io.EOF
		}
		b, err := r.get(r.ids[r.next])
		if err != nil {
			return 0, err
		}
		r.next++
		r.cur = b
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// Refs calls fn for every chunk referenced by a snapshot (tree chunks and
// file chunks). get loads chunk contents.
func (s *Snapshot) Refs(get func(ID) ([]byte, error), fn func(ID)) error {
	for _, v := range s.Volumes {
		for _, id := range v.Tree {
			fn(id)
		}
		err := ReadTree(NewChunkReader(v.Tree, get), func(n *Node) error {
			for _, id := range n.Chunks {
				fn(id)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
