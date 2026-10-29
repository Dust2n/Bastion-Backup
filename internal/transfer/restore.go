package transfer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"bastion/internal/repo"
)

// Writer is implemented by fsx.Writer.
type Writer interface {
	Write(n *repo.Node, content io.Reader) error
	Finish() error
}

// Getter returns the plaintext of a chunk.
type Getter func(ctx context.Context, id repo.ID) ([]byte, error)

type future struct {
	done chan struct{}
	data []byte
	err  error
}

// task is sent to the writer before its chunks are queued, so a file with
// more chunks than maxInflight can not block the pipeline.
type task struct {
	n    *repo.Node
	futs chan *future // nil when the node has no chunks
}

// Selected reports whether path is inside the selection (empty = all).
func Selected(path string, sel []string) bool {
	if len(sel) == 0 {
		return true
	}
	for _, s := range sel {
		s = strings.Trim(s, "/")
		if s == "" || path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}

// RestoreStats is reported while restoring.
type RestoreStats struct {
	Files  int64
	Bytes  int64
	Errors []string
}

const maxInflight = 48 // chunks fetched ahead of the writer

// RestoreVolume writes the selected part of a volume tree through w.
// Chunks are downloaded in parallel ahead of the writer while the writer
// keeps the original tree order.
func RestoreVolume(ctx context.Context, get Getter, vol repo.Volume, w Writer, sel []string, prog func(RestoreStats)) (RestoreStats, error) {
	var st RestoreStats
	getPlain := func(id repo.ID) ([]byte, error) { return get(ctx, id) }

	// Hard links whose first occurrence lies outside the selection are
	// restored as copies; find those targets first.
	needed := map[string]*repo.Node{}
	if len(sel) > 0 {
		err := repo.ReadTree(repo.NewChunkReader(vol.Tree, getPlain), func(n *repo.Node) error {
			if n.Type == repo.TypeHardlink && Selected(n.Path, sel) && !Selected(n.Link, sel) {
				needed[n.Link] = nil
			}
			return nil
		})
		if err != nil {
			return st, fmt.Errorf("Verzeichnisbaum lesen: %w", err)
		}
	}

	sem := make(chan struct{}, maxInflight)
	work := make(chan struct {
		id repo.ID
		f  *future
	}, maxInflight)
	ordered := make(chan task, 256)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				j.f.data, j.f.err = get(ctx, j.id)
				close(j.f.done)
			}
		}()
	}

	var prodErr error
	go func() {
		defer close(ordered)
		defer close(work)
		prodErr = repo.ReadTree(repo.NewChunkReader(vol.Tree, getPlain), func(n *repo.Node) error {
			if _, ok := needed[n.Path]; ok {
				needed[n.Path] = n
			}
			if !Selected(n.Path, sel) {
				return nil
			}
			t := task{n: n}
			if len(n.Chunks) > 0 {
				t.futs = make(chan *future, len(n.Chunks))
			}
			select {
			case ordered <- t:
			case <-ctx.Done():
				return ctx.Err()
			}
			for _, id := range n.Chunks {
				f := &future{done: make(chan struct{})}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					close(t.futs)
					return ctx.Err()
				}
				select {
				case work <- struct {
					id repo.ID
					f  *future
				}{id, f}:
				case <-ctx.Done():
					close(t.futs)
					return ctx.Err()
				}
				t.futs <- f
			}
			if t.futs != nil {
				close(t.futs)
			}
			return nil
		})
	}()

	var deferred []*repo.Node
	fail := func(path string, err error) {
		if len(st.Errors) < 500 {
			st.Errors = append(st.Errors, fmt.Sprintf("%s: %v", path, err))
		}
	}
	for t := range ordered {
		n := t.n
		if n.Type == repo.TypeHardlink {
			if _, ok := needed[n.Link]; ok {
				deferred = append(deferred, n)
				continue
			}
		}
		var r io.Reader
		var fr *futureReader
		switch {
		case t.futs != nil:
			fr = &futureReader{futs: t.futs, sem: sem}
			r = fr
		case n.Inline != nil:
			r = bytes.NewReader(n.Inline)
		}
		if err := w.Write(n, r); err != nil {
			fail(n.Path, err)
		}
		if fr != nil {
			if fr.err != nil && ctx.Err() == nil {
				fail(n.Path, fr.err)
			}
			fr.drain()
		}
		if n.Type == repo.TypeFile {
			st.Files++
			st.Bytes += n.Size
		}
		if prog != nil && st.Files%200 == 0 {
			prog(st)
		}
	}
	cancel()
	wg.Wait()
	if prodErr != nil && prodErr != context.Canceled {
		return st, fmt.Errorf("Verzeichnisbaum lesen: %w", prodErr)
	}

	for _, h := range deferred {
		t := needed[h.Link]
		if t == nil {
			fail(h.Path, fmt.Errorf("Ziel des Hardlinks %s fehlt", h.Link))
			continue
		}
		c := *t
		c.Path = h.Path
		var r io.Reader
		if c.Inline != nil {
			r = bytes.NewReader(c.Inline)
		} else {
			r = repo.NewChunkReader(c.Chunks, getPlain)
		}
		if err := w.Write(&c, r); err != nil {
			fail(h.Path, err)
		}
	}
	if err := w.Finish(); err != nil {
		return st, err
	}
	if prog != nil {
		prog(st)
	}
	return st, nil
}

type futureReader struct {
	futs chan *future
	sem  chan struct{}
	cur  []byte
	err  error
}

func (r *futureReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		f, ok := <-r.futs
		if !ok {
			return 0, io.EOF
		}
		<-f.done
		<-r.sem
		if f.err != nil {
			r.err = f.err
			return 0, f.err
		}
		r.cur = f.data
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// drain releases chunks the writer did not consume.
func (r *futureReader) drain() {
	for f := range r.futs {
		<-f.done
		<-r.sem
	}
	r.cur = nil
}
