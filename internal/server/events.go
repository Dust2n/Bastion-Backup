package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Live updates for the web UI via Server-Sent Events: job progress and
// status, agents coming online or going offline, rollout progress.

type eventHub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

var hub = &eventHub{subs: map[chan []byte]struct{}{}}

func (s *Server) publish(kind string, data any) {
	b, err := json.Marshal(map[string]any{"type": kind, "data": data})
	if err != nil {
		return
	}
	hub.mu.Lock()
	for ch := range hub.subs {
		select {
		case ch <- b:
		default: // slow client: it reloads on its own
		}
	}
	hub.mu.Unlock()
}

func (s *Server) publishJob(id int64) {
	if jobs := s.queryJobs("WHERE j.id=?", []any{id}, 1); len(jobs) == 1 {
		s.publish("job", jobs[0])
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, u *user) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "Streaming nicht unterstützt")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 64)
	hub.mu.Lock()
	hub.subs[ch] = struct{}{}
	hub.mu.Unlock()
	defer func() {
		hub.mu.Lock()
		delete(hub.subs, ch)
		hub.mu.Unlock()
	}()
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// Transfer speed and remaining time per running job, from the byte
// counts of consecutive progress reports (exponential moving average).

type speed struct {
	bytes int64
	at    time.Time
	rate  float64 // bytes per second
	total int64
}

var (
	speedMu sync.Mutex
	speeds  = map[int64]*speed{}
)

func (s *Server) trackSpeed(job, bytes, total int64) {
	speedMu.Lock()
	defer speedMu.Unlock()
	sp, ok := speeds[job]
	now := time.Now()
	if !ok {
		speeds[job] = &speed{bytes: bytes, at: now, total: total}
		return
	}
	dt := now.Sub(sp.at).Seconds()
	if dt < 0.5 {
		return
	}
	cur := float64(bytes-sp.bytes) / dt
	if cur < 0 {
		cur = 0
	}
	if sp.rate == 0 {
		sp.rate = cur
	} else {
		sp.rate = 0.7*sp.rate + 0.3*cur
	}
	sp.bytes, sp.at, sp.total = bytes, now, total
}

// jobSpeed returns bytes/s and the estimated seconds left (0 = unknown).
func jobSpeed(job, bytes, total int64) (float64, int64) {
	speedMu.Lock()
	defer speedMu.Unlock()
	sp, ok := speeds[job]
	if !ok || sp.rate <= 0 {
		return 0, 0
	}
	if time.Since(sp.at) > 2*time.Minute {
		return 0, 0
	}
	var eta int64
	if total > bytes {
		eta = int64(float64(total-bytes) / sp.rate)
	}
	return sp.rate, eta
}

func forgetSpeed(job int64) {
	speedMu.Lock()
	delete(speeds, job)
	speedMu.Unlock()
}
