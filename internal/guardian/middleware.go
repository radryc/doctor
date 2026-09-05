package guardian

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AuthMiddleware validates bearer tokens on Guardian event ingestion endpoints.
type AuthMiddleware struct {
	token string
}

// NewAuthMiddleware creates an auth middleware. If token is empty, all requests pass.
func NewAuthMiddleware(token string) *AuthMiddleware {
	return &AuthMiddleware{token: token}
}

// Wrap returns an http.Handler that checks the Authorization header before
// passing to the next handler.
func (am *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	if am.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		expected := "Bearer " + am.token
		if subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RateLimiter implements a simple token-bucket rate limiter per remote address.
type RateLimiter struct {
	maxPerSecond int
	mu           sync.Mutex
	buckets      map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// NewRateLimiter creates a rate limiter allowing maxPerSecond requests per client.
// If maxPerSecond <= 0, no limiting is applied.
func NewRateLimiter(maxPerSecond int) *RateLimiter {
	return &RateLimiter{
		maxPerSecond: maxPerSecond,
		buckets:      make(map[string]*bucket),
	}
}

// Wrap returns a handler that enforces rate limits.
func (rl *RateLimiter) Wrap(next http.Handler) http.Handler {
	if rl.maxPerSecond <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := r.RemoteAddr
		if idx := strings.LastIndex(addr, ":"); idx > 0 {
			addr = addr[:idx]
		}
		if !rl.allow(addr) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) allow(addr string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[addr]
	if !ok {
		b = &bucket{tokens: float64(rl.maxPerSecond), lastFill: now}
		rl.buckets[addr] = b
	}
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * float64(rl.maxPerSecond)
	if b.tokens > float64(rl.maxPerSecond) {
		b.tokens = float64(rl.maxPerSecond)
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Deduplicator tracks recently seen event IDs to skip duplicates.
type Deduplicator struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

// NewDeduplicator creates a deduplicator with the given TTL for seen entries.
func NewDeduplicator(ttl time.Duration) *Deduplicator {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &Deduplicator{
		seen: make(map[string]time.Time),
		ttl:  ttl,
	}
}

// IsDuplicate returns true if the given key was already seen within the TTL window.
func (d *Deduplicator) IsDuplicate(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	// Periodically evict old entries.
	if len(d.seen) > 10000 {
		for k, t := range d.seen {
			if now.Sub(t) > d.ttl {
				delete(d.seen, k)
			}
		}
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) < d.ttl {
		return true
	}
	d.seen[key] = now
	return false
}
