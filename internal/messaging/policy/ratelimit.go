package policy

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a per-sender token bucket. It is per replica, so the limit is
// approximate across replicas (§7.4).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewLimiter returns an empty Limiter.
func NewLimiter() *Limiter { return &Limiter{buckets: map[string]*rate.Limiter{}} }

// Allow takes one token from sender's bucket, which holds perMinute tokens
// and refills at perMinute per minute.
func (l *Limiter) Allow(sender string, perMinute int, now time.Time) *Refusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim := rate.Limit(float64(perMinute) / 60)
	b := l.buckets[sender]
	if b == nil {
		b = rate.NewLimiter(lim, perMinute)
		l.buckets[sender] = b
	} else if b.Limit() != lim || b.Burst() != perMinute {
		b.SetLimitAt(now, lim)
		b.SetBurstAt(now, perMinute)
	}
	if !b.AllowN(now, 1) {
		return refuse(Exhausted, "rate", "%s is sending too fast: the limit is %d messages a minute; wait and retry", sender, perMinute)
	}
	return nil
}
