package auth

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// RateLimitError is returned when a client or an account has to wait
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("too many attempts, retry in %d seconds", e.Seconds())
}

// Seconds returns the wait time in whole seconds, at least 1
func (e *RateLimitError) Seconds() int {
	return int(math.Max(1, math.Ceil(e.RetryAfter.Seconds())))
}

type bucket struct {
	tokens float64
	last   time.Time
}

type failures struct {
	count int
	last  time.Time
}

// limiter holds the per-IP token buckets and the per-account failure counters in memory
type limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	failures map[string]*failures
	logged   map[string]time.Time
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, failures: map[string]*failures{}, logged: map[string]time.Time{}}
}

// AllowIP takes a token from the bucket of a client address
func (s *Service) AllowIP(ip string) error {
	rl := s.cfg.rateLimits
	if rl.Disabled {
		return nil
	}
	now := s.now()
	l := s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 10000 {
		// forget full buckets
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Duration(rl.PerIPBurst)*rl.PerIPRefill {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: float64(rl.PerIPBurst), last: now}
		l.buckets[ip] = b
	}
	b.tokens = math.Min(float64(rl.PerIPBurst), b.tokens+now.Sub(b.last).Seconds()/rl.PerIPRefill.Seconds())
	b.last = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) * float64(rl.PerIPRefill))
		s.logLimitLocked("ip:"+ip, RequestInfo{IP: ip}, "")
		return &RateLimitError{RetryAfter: wait}
	}
	b.tokens--
	return nil
}

// checkAccount returns an error while an account (by username key) must wait after failures
func (s *Service) checkAccount(key string, ri RequestInfo) error {
	rl := s.cfg.rateLimits
	if rl.Disabled {
		return nil
	}
	l := s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.failures[key]
	if f == nil || f.count < rl.AccountFailuresBeforeBackoff {
		return nil
	}
	exp := f.count - rl.AccountFailuresBeforeBackoff
	backoff := rl.MaxBackoff
	if exp < 30 {
		backoff = min(time.Duration(1<<exp)*time.Second, rl.MaxBackoff)
	}
	if wait := f.last.Add(backoff).Sub(s.now()); wait > 0 {
		s.logLimitLocked("account:"+key, ri, key)
		return &RateLimitError{RetryAfter: wait}
	}
	return nil
}

// fail records a failed attempt for an account
func (s *Service) fail(key string) {
	l := s.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	now := s.now()
	if len(l.failures) > 10000 {
		for k, f := range l.failures {
			if now.Sub(f.last) > 24*time.Hour {
				delete(l.failures, k)
			}
		}
	}
	f := l.failures[key]
	if f == nil {
		f = &failures{}
		l.failures[key] = f
	}
	f.count++
	f.last = now
}

// succeed resets the failures of an account
func (s *Service) succeed(key string) {
	s.limiter.mu.Lock()
	delete(s.limiter.failures, key)
	s.limiter.mu.Unlock()
}

// logLimitLocked logs and audits a triggered limit at most once per minute per key
func (s *Service) logLimitLocked(key string, ri RequestInfo, username string) {
	now := s.now()
	if last, ok := s.limiter.logged[key]; ok && now.Sub(last) < time.Minute {
		return
	}
	s.limiter.logged[key] = now
	if len(s.limiter.logged) > 10000 {
		s.limiter.logged = map[string]time.Time{key: now}
	}
	log.Printf("Rate limit triggered: %s", key)
	details := map[string]any{"key": key}
	if username != "" {
		details["username"] = truncate(username, 64)
	}
	go s.Audit(AuditEvent{Type: AuditRateLimitTriggered, IP: ri.IP, UserAgent: ri.UserAgent, Details: details})
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
