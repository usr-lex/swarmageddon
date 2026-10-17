package terminal

import (
	"bytes"
	"sync"
	"time"
)

// Recorded input is audited in chunks: at each line end, every recordMax
// bytes, and recordEvery after the oldest byte not yet audited (for keys
// typed into a TUI without Enter).
const (
	recordMax   = 4096
	recordEvery = 10 * time.Second
)

// inputRecorder collects what a person types into a terminal for the audit
// log, when the workspace records keystrokes (docs/design.md §8.4). Chunks
// are audited in order by one goroutine, so typing never waits on the
// database.
type inputRecorder struct {
	mu    sync.Mutex
	buf   []byte
	timer *time.Timer
	out   chan string
	done  chan struct{}
}

func newInputRecorder(audit func(input string)) *inputRecorder {
	out, done := make(chan string, 64), make(chan struct{})
	go func() {
		defer close(done)
		for s := range out {
			audit(s)
		}
	}()
	return &inputRecorder{out: out, done: done}
}

func (r *inputRecorder) write(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out == nil {
		return
	}
	r.buf = append(r.buf, b...)
	if bytes.ContainsAny(b, "\r\n") || len(r.buf) >= recordMax {
		r.send()
	} else if r.timer == nil {
		r.timer = time.AfterFunc(recordEvery, r.flush)
	}
}

// send queues the buffer; r.mu is held.
func (r *inputRecorder) send() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if len(r.buf) > 0 && r.out != nil {
		r.out <- string(r.buf)
	}
	r.buf = nil
}

func (r *inputRecorder) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.send()
}

// Close audits what is left and waits until every chunk is recorded.
func (r *inputRecorder) Close() {
	r.mu.Lock()
	r.send()
	if r.out != nil {
		close(r.out)
		r.out = nil
	}
	r.mu.Unlock()
	<-r.done
}
