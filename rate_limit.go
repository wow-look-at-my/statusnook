package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginRateLimit throttles password guessing. Statusnook has no account
// lockout and no second factor, so without this an exposed instance can be
// brute forced at whatever rate bcrypt allows.
const (
	loginMaxAttempts = 10
	loginWindow      = 15 * time.Minute
	loginLockout     = 15 * time.Minute
)

type attemptRecord struct {
	count      int
	first      time.Time
	lockedTill time.Time
}

type rateLimiter struct {
	mu          sync.Mutex
	records     map[string]*attemptRecord
	maxAttempts int
	window      time.Duration
	lockout     time.Duration
}

func newRateLimiter(maxAttempts int, window time.Duration, lockout time.Duration) *rateLimiter {
	return &rateLimiter{
		records:     map[string]*attemptRecord{},
		maxAttempts: maxAttempts,
		window:      window,
		lockout:     lockout,
	}
}

var loginLimiter = newRateLimiter(loginMaxAttempts, loginWindow, loginLockout)

// allow reports whether another attempt may be made for key, and how long the
// caller must wait when it may not.
func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictLocked(now)

	record, ok := l.records[key]
	if !ok {
		return true, 0
	}

	if now.Before(record.lockedTill) {
		return false, record.lockedTill.Sub(now)
	}

	return true, 0
}

// fail records a failed attempt, locking the key out once the window fills.
func (l *rateLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.evictLocked(now)

	record, ok := l.records[key]
	if !ok || now.Sub(record.first) > l.window {
		l.records[key] = &attemptRecord{count: 1, first: now}
		return
	}

	record.count++
	if record.count >= l.maxAttempts {
		record.lockedTill = now.Add(l.lockout)
		record.count = 0
		record.first = now
	}
}

// succeed clears the history for a key after a valid login.
func (l *rateLimiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.records, key)
}

// evictLocked drops records that can no longer deny anything, so the map does
// not grow with every attacker-chosen key.
func (l *rateLimiter) evictLocked(now time.Time) {
	for key, record := range l.records {
		if now.After(record.lockedTill) && now.Sub(record.first) > l.window {
			delete(l.records, key)
		}
	}
}

// clientIP identifies the caller. X-Forwarded-For is only consulted behind a
// trusted proxy: otherwise anyone could send a header per request and get a
// fresh rate-limit bucket every time.
func clientIP(r *http.Request) string {
	if env.TrustProxy {
		forwarded := r.Header.Get("X-Forwarded-For")
		if forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if first != "" {
				return first
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
