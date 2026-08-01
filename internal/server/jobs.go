package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"omnidrive/internal/store"
)

// A job is a long-running transfer the UI wants live feedback on. Progress is
// pushed over Server-Sent Events rather than WebSockets: SSE is a few lines of
// code on both sides, survives the browser backgrounding a tab on Android, and
// reconnects on its own.

// Job is one transfer.
type Job struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "upload" | "download" | "sync"
	Name      string    `json:"name"`
	Account   string    `json:"account"`
	Total     int64     `json:"total"`
	Sent      int64     `json:"sent"`
	Status    string    `json:"status"` // "running" | "done" | "error"
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
}

type jobRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*Job
	subs map[chan []byte]struct{}
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{
		jobs: map[string]*Job{},
		subs: map[chan []byte]struct{}{},
	}
}

func (jr *jobRegistry) start(kind, name, account string, total int64) *Job {
	j := &Job{
		ID: store.NewID(), Kind: kind, Name: name, Account: account,
		Total: total, Status: "running", StartedAt: time.Now(),
	}
	jr.mu.Lock()
	jr.jobs[j.ID] = j
	jr.mu.Unlock()
	jr.publish(j)
	return j
}

// progress records bytes transferred. Updates are throttled to roughly four
// per second so a fast upload does not flood the SSE stream and stall the
// phone's rendering.
func (jr *jobRegistry) progress(j *Job, sent int64) {
	jr.mu.Lock()
	prev := j.Sent
	j.Sent = sent
	shouldPublish := sent-prev > 256<<10 || sent == j.Total
	jr.mu.Unlock()
	if shouldPublish {
		jr.publish(j)
	}
}

// retarget switches a multi-item job to reporting the file it is currently on,
// so the progress bar means something during a folder copy.
func (jr *jobRegistry) retarget(j *Job, name string, addBytes int64) {
	jr.mu.Lock()
	j.Name = name
	if addBytes > 0 {
		j.Total += addBytes
	}
	jr.mu.Unlock()
	jr.publish(j)
}

// note records a non-fatal problem against a job, so a partly failed batch can
// still explain itself.
func (jr *jobRegistry) note(j *Job, msg string) {
	jr.mu.Lock()
	if j.Error == "" {
		j.Error = msg
	} else if len(j.Error) < 500 {
		j.Error += "; " + msg
	}
	jr.mu.Unlock()
	jr.publish(j)
}

func (jr *jobRegistry) finish(j *Job, err error) {
	jr.mu.Lock()
	j.EndedAt = time.Now()
	if err != nil {
		j.Status, j.Error = "error", err.Error()
	} else {
		j.Status = "done"
		if j.Total > 0 {
			j.Sent = j.Total
		}
	}
	jr.mu.Unlock()
	jr.publish(j)
	jr.prune()
}

// prune keeps the job list bounded; finished jobs older than ten minutes go.
func (jr *jobRegistry) prune() {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, j := range jr.jobs {
		if j.Status != "running" && j.EndedAt.Before(cutoff) {
			delete(jr.jobs, id)
		}
	}
}

func (jr *jobRegistry) list() []*Job {
	jr.mu.RLock()
	defer jr.mu.RUnlock()
	out := make([]*Job, 0, len(jr.jobs))
	for _, j := range jr.jobs {
		cp := *j
		out = append(out, &cp)
	}
	return out
}

func (jr *jobRegistry) publish(j *Job) {
	jr.mu.RLock()
	cp := *j
	jr.mu.RUnlock()

	payload, err := json.Marshal(cp)
	if err != nil {
		return
	}
	msg := []byte("event: job\ndata: " + string(payload) + "\n\n")

	jr.mu.RLock()
	defer jr.mu.RUnlock()
	for ch := range jr.subs {
		// Never block on a slow reader: a backgrounded browser tab must not
		// wedge an upload.
		select {
		case ch <- msg:
		default:
		}
	}
}

// maxSubscribers bounds the event stream.
//
// One page needs one subscription. A client that leaks them — as the UI once
// did, opening a fresh EventSource on every reconnect without closing the old
// one — would otherwise pile up goroutines and buffers here until the process
// died. Refusing the excess keeps a browser bug from becoming a server one.
const maxSubscribers = 32

func (jr *jobRegistry) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	jr.mu.Lock()
	defer jr.mu.Unlock()

	if len(jr.subs) >= maxSubscribers {
		// Drop the oldest rather than refuse the newest: the newest is the
		// live page, and a stale one is what has gone wrong.
		//
		// Unregister only — never close. The handler that owns this channel
		// closes it when its request ends, and closing here as well would
		// panic on the double close. Once unregistered it simply stops
		// receiving, and its handler exits on the request context.
		for old := range jr.subs {
			delete(jr.subs, old)
			break
		}
	}
	jr.subs[ch] = struct{}{}
	return ch
}

func (jr *jobRegistry) unsubscribe(ch chan []byte) {
	jr.mu.Lock()
	delete(jr.subs, ch)
	jr.mu.Unlock()
	close(ch)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Without this, any reverse proxy in front of us will buffer the stream
	// and the progress bar will jump from 0 to 100.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Replay current jobs so a page reload immediately shows transfers in
	// flight rather than an empty list.
	for _, j := range s.jobs.list() {
		if b, err := json.Marshal(j); err == nil {
			fmt.Fprintf(w, "event: job\ndata: %s\n\n", b)
		}
	}
	flusher.Flush()

	ch := s.jobs.subscribe()
	defer s.jobs.unsubscribe(ch)

	// A periodic comment keeps the connection alive through Android's
	// aggressive idle-socket reaping.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
