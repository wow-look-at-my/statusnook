package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A release build with TLS on adds two middlewares a dev build never sees: the
// ACME challenge responder, and the redirect that stops the instance answering
// on plain HTTP at all. Both are wired at router construction, so the router
// has to be built with those globals already set.
func TestReleaseRouterRedirectsPlainHTTPToTLS(t *testing.T) {
	app := withTestApp(t)

	previousBuild := BUILD
	BUILD = "release"
	metaSSL.Store("true")
	t.Cleanup(func() {
		BUILD = previousBuild
		metaSSL.Store("false")
	})

	plain := httptest.NewServer(newRouter())
	t.Cleanup(plain.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	res, err := client.Get(plain.URL + "/history?period=2020-01")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusFound, res.StatusCode)
	require.Contains(t, res.Header.Get("Location"), "https://")
	require.Contains(t, res.Header.Get("Location"), "/history?period=2020-01",
		"the redirect keeps the path and query it was asked for")

	// Over TLS the request is served, and carries the transport headers a
	// plain-HTTP response cannot.
	secure := httptest.NewTLSServer(newRouter())
	t.Cleanup(secure.Close)

	tlsClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	res, err = tlsClient.Get(secure.URL + "/healthz")
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, res.Header.Get("Strict-Transport-Security"), "max-age=")

	require.NotNil(t, app)
}

// The headers every response carries. X-Frame-Options matters more than usual
// here: the CSRF token is injected by the page's own JavaScript, so a framed
// admin's click would carry a valid one.
func TestSecurityHeadersAreOnEveryResponse(t *testing.T) {
	app := withTestApp(t)

	for _, path := range []string{"/", "/admin/alerts", "/static/main.css", "/healthz"} {
		resp := app.get(path)
		require.Equal(t, "DENY", resp.header.Get("X-Frame-Options"), path)
		require.Equal(t, "nosniff", resp.header.Get("X-Content-Type-Options"), path)
		require.Equal(t, "strict-origin-when-cross-origin", resp.header.Get("Referrer-Policy"), path)

		// Only a TLS request gets HSTS; sending it over plain HTTP would pin a
		// browser to a scheme this instance may not serve.
		require.Empty(t, resp.header.Get("Strict-Transport-Security"), path)
	}
}
