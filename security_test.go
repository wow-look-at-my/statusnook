package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSafeRedirectPath covers the cross-auth open redirect: "after" is
// concatenated onto "https://" + domain, so anything that can change the host
// hands an attacker a redirect off the status page.
func TestSafeRedirectPath(t *testing.T) {
	cases := map[string]string{
		"":                        "/",
		"/":                       "/",
		"/admin/alerts":           "/admin/alerts",
		"/admin/alerts?id=3":      "/admin/alerts?id=3",
		"//evil.com":              "/",
		"//evil.com/path":         "/",
		"@evil.com":               "/",
		"https://evil.com":        "/",
		"http://evil.com":         "/",
		"admin/alerts":            "/",
		"\\\\evil.com":            "/",
		"/\\evil.com":             "/%5Cevil.com",
		"/path with space":        "/path%20with%20space",
		"javascript:alert(1)":     "/",
		"/redirect?to=//evil.com": "/redirect?to=//evil.com",
	}

	for in, want := range cases {
		assert.Equal(t, want, safeRedirectPath(in), "input %q", in)
	}
}

func TestRequestIsHTTPS(t *testing.T) {
	prev := env
	t.Cleanup(func() { env = prev })

	plain := httptest.NewRequest(http.MethodGet, "http://nas.local:8000/admin", nil)

	env = envConfig{TrustProxy: false}
	assert.False(t, requestIsHTTPS(plain), "plain HTTP must not be treated as TLS")

	forwarded := httptest.NewRequest(http.MethodGet, "http://nas.local:8000/admin", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")

	// Untrusted: a client could set the header itself.
	assert.False(t, requestIsHTTPS(forwarded))

	env = envConfig{TrustProxy: true}
	assert.True(t, requestIsHTTPS(forwarded))

	multi := httptest.NewRequest(http.MethodGet, "http://nas.local:8000/admin", nil)
	multi.Header.Set("X-Forwarded-Proto", "https, http")
	assert.True(t, requestIsHTTPS(multi))

	httpForwarded := httptest.NewRequest(http.MethodGet, "http://nas.local:8000/admin", nil)
	httpForwarded.Header.Set("X-Forwarded-Proto", "http")
	assert.False(t, requestIsHTTPS(httpForwarded))
}

// TestSessionCookieSecureFlag pins the behaviour that made a LAN deployment
// unusable: a Secure cookie over plain HTTP is dropped by the browser, so the
// admin could never stay logged in.
func TestSessionCookieSecureFlag(t *testing.T) {
	prev := env
	t.Cleanup(func() { env = prev })
	env = envConfig{}

	plain := httptest.NewRequest(http.MethodPost, "http://nas.local:8000/login", nil)
	cookie := sessionCookie(plain, "token-value")

	assert.False(t, cookie.Secure)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.WithinDuration(t, time.Now().UTC().Add(sessionLifetime), cookie.Expires, time.Minute)

	tls := httptest.NewRequest(http.MethodPost, "https://status.example.com/login", nil)
	tls.TLS = plainTLSState()
	assert.True(t, sessionCookie(tls, "token-value").Secure)
}

func TestRateLimiterLocksOutAndRecovers(t *testing.T) {
	limiter := newRateLimiter(3, time.Minute, 5*time.Minute)
	now := time.Now().UTC()

	for i := 0; i < 2; i++ {
		ok, _ := limiter.allow("user:admin", now)
		require.True(t, ok, "attempt %d", i)
		limiter.fail("user:admin", now)
	}

	ok, _ := limiter.allow("user:admin", now)
	require.True(t, ok, "the third attempt is still allowed")
	limiter.fail("user:admin", now)

	ok, retryAfter := limiter.allow("user:admin", now)
	assert.False(t, ok, "the limiter must lock out after the third failure")
	assert.Positive(t, retryAfter)

	// Another key is unaffected.
	ok, _ = limiter.allow("user:other", now)
	assert.True(t, ok)

	// The lockout expires.
	ok, _ = limiter.allow("user:admin", now.Add(6*time.Minute))
	assert.True(t, ok)
}

func TestRateLimiterSucceedClearsHistory(t *testing.T) {
	limiter := newRateLimiter(2, time.Minute, time.Minute)
	now := time.Now().UTC()

	limiter.fail("ip:10.0.0.1", now)
	limiter.succeed("ip:10.0.0.1")
	limiter.fail("ip:10.0.0.1", now)

	ok, _ := limiter.allow("ip:10.0.0.1", now)
	assert.True(t, ok, "a successful login must reset the counter")
}

func TestRateLimiterEvictsStaleRecords(t *testing.T) {
	limiter := newRateLimiter(2, time.Minute, time.Minute)
	now := time.Now().UTC()

	limiter.fail("ip:10.0.0.1", now)
	assert.Len(t, limiter.records, 1)

	// A much later attempt for a different key clears the stale record, so the
	// map cannot grow without bound on attacker-chosen keys.
	limiter.fail("ip:10.0.0.2", now.Add(time.Hour))
	assert.Len(t, limiter.records, 1)
}

func TestClientIP(t *testing.T) {
	prev := env
	t.Cleanup(func() { env = prev })

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	env = envConfig{TrustProxy: false}
	assert.Equal(t, "192.0.2.10", clientIP(req), "an untrusted header must be ignored")

	env = envConfig{TrustProxy: true}
	assert.Equal(t, "203.0.113.7", clientIP(req))
}

func TestCrossAuthTokenSingleUse(t *testing.T) {
	store := &crossAuthStore{tokens: map[string]crossAuthToken{}}
	now := time.Now().UTC()

	store.issue("token", 7, now)

	userID, ok := store.redeem("token", now)
	require.True(t, ok)
	assert.Equal(t, 7, userID)

	_, ok = store.redeem("token", now)
	assert.False(t, ok, "a cross-auth token must not be reusable")
}

func TestCrossAuthTokenExpires(t *testing.T) {
	store := &crossAuthStore{tokens: map[string]crossAuthToken{}}
	now := time.Now().UTC()

	store.issue("token", 7, now)

	_, ok := store.redeem("token", now.Add(crossAuthTokenLifetime+time.Second))
	assert.False(t, ok)

	// Issuing prunes expired entries rather than letting the map grow.
	store.issue("stale", 1, now)
	store.issue("fresh", 2, now.Add(crossAuthTokenLifetime+time.Second))
	assert.Len(t, store.tokens, 1)
}

// plainTLSState is the minimum non-nil TLS state a handler checks for.
func plainTLSState() *tls.ConnectionState {
	return &tls.ConnectionState{HandshakeComplete: true}
}

func TestSecurityHeaders(t *testing.T) {
	prev := env
	t.Cleanup(func() { env = prev })
	env = envConfig{}

	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	statusPage := httptest.NewRecorder()
	handler.ServeHTTP(statusPage, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "nosniff", statusPage.Header().Get("X-Content-Type-Options"))
	assert.Empty(t, statusPage.Header().Get("Strict-Transport-Security"))
	assert.Empty(
		t, statusPage.Header().Get("X-Frame-Options"),
		"a public status page is embeddable on purpose",
	)

	admin := httptest.NewRecorder()
	handler.ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "/admin/monitors", nil))
	assert.Equal(t, "SAMEORIGIN", admin.Header().Get("X-Frame-Options"))

	secure := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://status.example.com/admin", nil)
	req.TLS = plainTLSState()
	handler.ServeHTTP(secure, req)
	assert.Contains(t, secure.Header().Get("Strict-Transport-Security"), "max-age=")
}

func TestIsAuthenticatedArea(t *testing.T) {
	for _, path := range []string{"/admin", "/admin/", "/admin/monitors", "/login", "/setup/domain"} {
		assert.True(t, isAuthenticatedArea(path), "path %q", path)
	}

	for _, path := range []string{"/", "/history", "/static/main.css", "/administrator"} {
		assert.False(t, isAuthenticatedArea(path), "path %q", path)
	}
}
