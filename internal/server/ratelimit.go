package server

import (
	"sync"
	"time"

	"github.com/liueic/avater/internal/metrics"
)

// limiterSet is a lazy-refill token-bucket limiter over arbitrary keys
// (global and per-IP buckets, SPEC §14). Refill happens on access — no
// background goroutines. Bucket overflow evicts the whole table, which
// briefly relaxes limits instead of growing memory without bound.
type limiterSet struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucketState
	maxKeys int
	reg     *metrics.Registry
}

type bucketState struct {
	tokens float64
	last   time.Time
}

func newLimiterSet(rps float64, burst int, maxKeys int, reg *metrics.Registry) *limiterSet {
	if rps <= 0 {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	if maxKeys < 1024 {
		maxKeys = 1024
	}
	return &limiterSet{
		rate:    rps,
		burst:   float64(burst),
		buckets: make(map[string]*bucketState, 256),
		maxKeys: maxKeys,
		reg:     reg,
	}
}

// Allow consumes one token for key, reporting whether the request may pass.
func (l *limiterSet) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	if len(l.buckets) >= l.maxKeys {
		l.buckets = make(map[string]*bucketState, 256)
		l.reg.Counter("avater_ratelimit_evictions_total", "Rate-limit tables reset after key overflow", nil, 1)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucketState{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		l.mu.Unlock()
		return true
	}
	l.mu.Unlock()
	return false
}

// Len reports the number of tracked buckets (metrics).
func (l *limiterSet) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
