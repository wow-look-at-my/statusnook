package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Eleven assets are embedded only gzipped. A client that advertises gzip gets
// the compressed bytes with the encoding header; one that does not gets them
// inflated on the fly, because there is no plain copy to fall back to and
// labelling the response with an encoding nobody asked for breaks it.
func TestGzipOnlyAssetsServeBothWays(t *testing.T) {
	app := withTestApp(t)

	const path = "/static/htmx-1.9.12.js"

	// DisableCompression stops the transport adding Accept-Encoding and
	// silently inflating the reply, which is what hides this branch.
	plain := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	res, err := plain.Get(app.server.URL + path)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Empty(t, res.Header.Get("Content-Encoding"),
		"nothing was compressed for this client")
	require.Contains(t, res.Header.Get("Content-Type"), "javascript")
	require.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "htmx", "the inflated asset, not the gzip stream")

	req, err := http.NewRequest(http.MethodGet, app.server.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")

	gz, err := plain.Do(req)
	require.NoError(t, err)
	defer gz.Body.Close()
	require.Equal(t, http.StatusOK, gz.StatusCode)
	require.Equal(t, "gzip", gz.Header.Get("Content-Encoding"))

	compressed, err := io.ReadAll(gz.Body)
	require.NoError(t, err)
	require.Equal(t, []byte{0x1f, 0x8b}, compressed[:2], "a gzip stream starts 1f 8b")
	require.Less(t, len(compressed), len(body))
}

// The assets are baked into the binary, so their ETag cannot change without a
// new binary -- which is what makes the immutable cache header safe.
func TestStaticAssetsRevalidateWithAnETag(t *testing.T) {
	app := withTestApp(t)

	resp := app.get("/static/main.css")
	require.Equal(t, http.StatusOK, resp.status)

	etag := resp.header.Get("ETag")
	require.NotEmpty(t, etag)
	require.Contains(t, resp.header.Get("Cache-Control"), "immutable")

	req, err := http.NewRequest(http.MethodGet, app.server.URL+"/static/main.css", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", etag)

	res, err := app.client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusNotModified, res.StatusCode)
}

// The cross-auth handshake: the admin page mints a one-shot token, and the
// public page trades it for a session on the other origin.
func TestCrossAuthTokenIsOneShot(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/resolve", nil)
	require.Equal(t, http.StatusOK, resp.status)

	token := strings.TrimSpace(resp.body)
	require.NotEmpty(t, token)

	// A visitor who already has a session is redirected before the token is
	// even looked at, so redemption has to be driven by a fresh client -- which
	// is the situation the handshake exists for: the other origin.
	require.Equal(t, http.StatusFound, app.get("/cross-auth?token="+token).status,
		"an authenticated visitor skips straight to the redirect")

	other := func(query string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, app.server.URL+"/cross-auth"+query, nil)
		require.NoError(t, err)

		res, err := (&http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}).Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { res.Body.Close() })

		return res
	}

	require.Equal(t, http.StatusBadRequest, other("").StatusCode, "no token")
	require.Equal(t, http.StatusBadRequest, other("?token=made-up").StatusCode)

	redeemed := other("?token=" + token)
	require.Equal(t, http.StatusFound, redeemed.StatusCode)
	require.NotEmpty(t, redeemed.Cookies(), "redemption issues a session cookie")

	// Spent: a replay must not mint a second session.
	require.Equal(t, http.StatusBadRequest, other("?token="+token).StatusCode)
}
