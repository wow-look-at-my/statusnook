package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Login had no throttle of any kind: no per-IP counter, no per-account
// counter, no delay. An admin account here is total control of the instance
// and the password minimum is 8 characters, so online guessing was the
// cheapest way in.
//
// Deliberately in memory rather than SQLite: this is on the path of every
// login attempt, including a flood of them, and it must not put write load on
// the single write connection precisely when someone is hammering it. The
// cost is that a restart clears the counters.
const (
	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

type loginFailures struct {
	count int
	first time.Time
}

var loginLimiterMu sync.Mutex
var loginLimiter = map[string]*loginFailures{}

// loginRateLimitKey buckets by client IP. Behind the reverse proxy this
// project documents, every request shares the proxy's address, so the bucket
// is effectively global -- still a real limit on how fast anyone can guess,
// just a coarser one.
func loginRateLimitKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginRateLimited reports whether this key has spent its attempts. It also
// sweeps expired buckets, so the map cannot grow without bound.
func loginRateLimited(key string) bool {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()

	now := time.Now().UTC()

	for k, v := range loginLimiter {
		if now.Sub(v.first) > loginWindow {
			delete(loginLimiter, k)
		}
	}

	v, ok := loginLimiter[key]
	if !ok {
		return false
	}

	return v.count >= loginMaxFailures
}

func recordLoginFailure(key string) {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()

	now := time.Now().UTC()

	v, ok := loginLimiter[key]
	if !ok || now.Sub(v.first) > loginWindow {
		loginLimiter[key] = &loginFailures{count: 1, first: now}
		return
	}

	v.count++
}

func clearLoginFailures(key string) {
	loginLimiterMu.Lock()
	defer loginLimiterMu.Unlock()

	delete(loginLimiter, key)
}

// dummyPasswordHash is bcrypt of a value nothing can log in with. It exists
// only to spend the same time on an unknown username as on a known one.
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
