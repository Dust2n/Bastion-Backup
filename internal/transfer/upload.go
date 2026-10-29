// Package transfer moves chunks between agent and server: deduplicating
// upload with bandwidth limit, and parallel prefetching download for
// restores.
package transfer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"bastion/internal/client"
	"bastion/internal/proto"
	"bastion/internal/repo"
)

// Limiter is a simple token bucket (bytes per second, 0 = unlimited).
type Limiter struct {
	mu   sync.Mutex
	rate float64
	next time.Time
}

func NewLimiter(bytesPerSec int64) *Limiter { return &Limiter{rate: float64(bytesPerSec)} }

func (l *Limiter) Wait(ctx context.Context, n int) error {
	if l == nil || l.rate <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type item struct {
	id   repo.ID
	data []byte
}

// Uploader sends chunks the server does not have yet.
type Uploader struct {
	c       *client.Client
	ctx     context.Context
	limiter *Limiter

	mu    sync.Mutex
	known map[repo.ID]struct{}

	pending chan item
	upload  chan item
	wg      sync.WaitGroup
	done    func()
	err     error

	NewBytes  atomic.Int64 // compressed bytes actually transferred
	NewChunks atomic.Int64
}

func NewUploader(ctx context.Context, c *client.Client, limiter *Limiter) *Uploader {
	u := &Uploader{
		c: c, ctx: ctx, limiter: limiter,
		known:   map[repo.ID]struct{}{},
		pending: make(chan item, 16),
		upload:  make(chan item, 4),
	}
	u.wg.Add(1)
	go u.batcher()
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			u.worker()
		}()
	}
	go func() {
		u.wg.Wait()
		close(u.upload)
	}()
	u.done = func() { workers.Wait() }
	return u
}

func (u *Uploader) fail(err error) {
	u.mu.Lock()
	if u.err == nil {
		u.err = err
	}
	u.mu.Unlock()
}

// Err returns the first upload error.
func (u *Uploader) Err() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

// MarkKnown tells the uploader that the server already has these chunks.
func (u *Uploader) MarkKnown(ids []repo.ID) {
	u.mu.Lock()
	for _, id := range ids {
		u.known[id] = struct{}{}
	}
	u.mu.Unlock()
}

// Add queues a chunk and returns its ID. data is copied.
func (u *Uploader) Add(data []byte) (repo.ID, error) {
	id := repo.Hash(data)
	u.mu.Lock()
	_, ok := u.known[id]
	if !ok {
		u.known[id] = struct{}{}
	}
	err := u.err
	u.mu.Unlock()
	if err != nil {
		return id, err
	}
	if ok {
		return id, nil
	}
	select {
	case u.pending <- item{id: id, data: bytes.Clone(data)}:
	case <-u.ctx.Done():
		return id, u.ctx.Err()
	}
	return id, nil
}

func (u *Uploader) batcher() {
	defer u.wg.Done()
	var batch []item
	timer := time.NewTimer(time.Hour)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ids := make([]repo.ID, len(batch))
		for i, it := range batch {
			ids[i] = it.id
		}
		var resp proto.MissingResponse
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			if _, err = u.c.JSON(u.ctx, "POST", "/api/agent/chunks/missing", proto.MissingRequest{IDs: ids}, &resp); err == nil {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		if err != nil {
			u.fail(err)
			batch = nil
			return
		}
		missing := map[repo.ID]bool{}
		for _, id := range resp.Missing {
			missing[id] = true
		}
		for _, it := range batch {
			if missing[it.id] {
				select {
				case u.upload <- it:
				case <-u.ctx.Done():
					return
				}
			}
		}
		batch = nil
	}
	for {
		select {
		case it, ok := <-u.pending:
			if !ok {
				flush()
				return
			}
			batch = append(batch, it)
			if len(batch) == 1 {
				timer.Reset(200 * time.Millisecond)
			}
			if len(batch) >= 16 {
				flush()
			}
		case <-timer.C:
			flush()
		case <-u.ctx.Done():
			return
		}
	}
}

func (u *Uploader) worker() {
	for it := range u.upload {
		if u.Err() != nil {
			continue
		}
		z := repo.Compress(it.data)
		if err := u.limiter.Wait(u.ctx, len(z)); err != nil {
			u.fail(err)
			continue
		}
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			r, e := u.c.Do(u.ctx, "PUT", "/api/agent/chunks/"+it.id.String(), bytes.NewReader(z), "application/zstd")
			if e == nil {
				r.Body.Close()
				err = nil
				break
			}
			err = e
			var he *client.HTTPError
			if errors.As(e, &he) && he.Status < 500 && he.Status != 429 {
				break
			}
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
		if err != nil {
			u.fail(err)
			continue
		}
		u.NewBytes.Add(int64(len(z)))
		u.NewChunks.Add(1)
	}
}

// Close waits until every queued chunk is on the server.
func (u *Uploader) Close() error {
	close(u.pending)
	u.done()
	return u.Err()
}
