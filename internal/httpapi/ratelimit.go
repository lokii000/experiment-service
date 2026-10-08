package httpapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimits control approximate limits per application instance. Limits apply
// to the TCP peer address, not untrusted X-Forwarded-For headers. Deploy behind
// trusted edge-level quotas/rate limiting for distributed abuse protection.
type RateLimits struct {
	AssignmentRPS   int
	AssignmentBurst int
	TrackingRPS     int
	TrackingBurst   int
	AdminRPS        int
	AdminBurst      int
}

func DefaultRateLimits() RateLimits {
	return RateLimits{
		AssignmentRPS: 1500, AssignmentBurst: 3000,
		TrackingRPS: 60, TrackingBurst: 120,
		AdminRPS: 10, AdminBurst: 20,
	}
}

type tokenBucket struct {
	tokens  float64
	updated time.Time
	seen    time.Time
}

// ipLimiter bounds per-peer requests and the aggregate requests handled by a
// single process. A bounded key table prevents unbounded memory growth under
// requests from many different addresses.
type ipLimiter struct {
	mu          sync.Mutex
	clients     map[string]*tokenBucket
	global      tokenBucket
	rate        float64
	burst       float64
	globalRate  float64
	globalBurst float64
	lastSweep   time.Time
}

const maxRateLimitClients = 8192

func newIPLimiter(rate, burst int) *ipLimiter {
	if rate <= 0 || burst <= 0 {
		panic("rate limiter requires positive rate and burst")
	}
	return &ipLimiter{
		clients:     make(map[string]*tokenBucket),
		rate:        float64(rate),
		burst:       float64(burst),
		globalRate:  float64(rate) * 10,
		globalBurst: float64(burst) * 10,
	}
}

func refill(b *tokenBucket, now time.Time, rate, capacity float64) {
	if b.updated.IsZero() {
		b.tokens = capacity
	} else if elapsed := now.Sub(b.updated).Seconds(); elapsed > 0 {
		b.tokens += elapsed * rate
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}
	b.updated = now
}

func (l *ipLimiter) allowAt(peer string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Amortize cleanup: one sweep per minute instead of scanning every request.
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= time.Minute {
		for key, b := range l.clients {
			if now.Sub(b.seen) > 3*time.Minute {
				delete(l.clients, key)
			}
		}
		l.lastSweep = now
	}
	if _, ok := l.clients[peer]; !ok && len(l.clients) >= maxRateLimitClients {
		// New addresses share an overflow bucket instead of growing the map.
		peer = "_overflow"
	}
	client, ok := l.clients[peer]
	if !ok {
		client = &tokenBucket{}
		l.clients[peer] = client
	}
	client.seen = now
	refill(client, now, l.rate, l.burst)
	refill(&l.global, now, l.globalRate, l.globalBurst)
	if client.tokens < 1 || l.global.tokens < 1 {
		return false
	}
	client.tokens--
	l.global.tokens--
	return true
}

func requestPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func (l *ipLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allowAt(requestPeer(r), time.Now()) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		next(w, r)
	}
}
