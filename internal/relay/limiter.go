package relay

import (
	"sync"
	"time"
)

type ipBucket struct {
	inflight int
	stamps   []time.Time
}

type ipLimiter struct {
	mu          sync.Mutex
	maxInflight int
	maxPerWin   int
	window      time.Duration
	buckets     map[string]*ipBucket
	lastPrune   time.Time
}

func newIPLimiter(maxInflight, maxPerWin int, window time.Duration) *ipLimiter {
	return &ipLimiter{
		maxInflight: maxInflight,
		maxPerWin:   maxPerWin,
		window:      window,
		buckets:     make(map[string]*ipBucket),
		lastPrune:   time.Now(),
	}
}

func (l *ipLimiter) acquire(ip string) bool {
	if ip == "" {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastPrune) > l.window*3 && len(l.buckets) > 256 {
		for k, b := range l.buckets {
			if b.inflight == 0 && (len(b.stamps) == 0 || b.stamps[len(b.stamps)-1].Before(cutoff)) {
				delete(l.buckets, k)
			}
		}
		l.lastPrune = now
	}

	b := l.buckets[ip]
	if b == nil {
		b = &ipBucket{}
		l.buckets[ip] = b
	}

	// Prune timestamps outside the sliding window.
	keep := b.stamps[:0]
	for _, ts := range b.stamps {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	b.stamps = keep

	if b.inflight >= l.maxInflight || len(b.stamps) >= l.maxPerWin {
		return false
	}
	b.inflight++
	b.stamps = append(b.stamps, now)
	return true
}

func (l *ipLimiter) release(ip string) {
	if ip == "" {
		return
	}
	l.mu.Lock()
	if b := l.buckets[ip]; b != nil && b.inflight > 0 {
		b.inflight--
	}
	l.mu.Unlock()
}
