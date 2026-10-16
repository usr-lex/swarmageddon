package auth

import (
	"sync"
	"time"
)

// Limiter rate-limits setup token attempts per client IP: Max attempts per
// Window, then a lockout that doubles with every further lockout (§3.3).
type Limiter struct {
	Max     int
	Window  time.Duration
	Lockout time.Duration // first lockout
	MaxLock time.Duration
	Now     func() time.Time
	mu      sync.Mutex
	clients map[string]*limitState
	lastGC  time.Time
}

type limitState struct {
	attempts    []time.Time
	lockedUntil time.Time
	lockouts    int
}

// NewLimiter returns the setup token limiter: 5 a minute, lockouts from 1 minute doubling up to 24 hours.
func NewLimiter() *Limiter {
	return &Limiter{Max: 5, Window: time.Minute, Lockout: time.Minute, MaxLock: 24 * time.Hour, Now: time.Now}
}

// Allow records an attempt from ip and reports whether it may proceed, and if
// not, how long until it may.
func (l *Limiter) Allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if l.clients == nil {
		l.clients = map[string]*limitState{}
	}
	l.gc(now)
	st := l.clients[ip]
	if st == nil {
		st = &limitState{}
		l.clients[ip] = st
	}
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	kept := st.attempts[:0]
	for _, t := range st.attempts {
		if now.Sub(t) < l.Window {
			kept = append(kept, t)
		}
	}
	st.attempts = kept
	if len(st.attempts) >= l.Max {
		d := l.Lockout << st.lockouts
		if d > l.MaxLock || d <= 0 {
			d = l.MaxLock
		}
		st.lockouts++
		st.lockedUntil = now.Add(d)
		st.attempts = nil
		return false, d
	}
	st.attempts = append(st.attempts, now)
	return true, 0
}

// Reset forgets ip, after a successful attempt.
func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.clients, ip)
}

func (l *Limiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < 10*time.Minute {
		return
	}
	l.lastGC = now
	for ip, st := range l.clients {
		if now.After(st.lockedUntil.Add(l.MaxLock)) && len(st.attempts) == 0 {
			delete(l.clients, ip)
		}
	}
}
