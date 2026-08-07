package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Serves the GitHub "latest release" endpoint the update page reads.
func serveLatestRelease(t *testing.T, release any, status int) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(release))
	}))
	t.Cleanup(server.Close)

	previous := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previous })
}

func TestUpdateCheckReportsBothOutcomes(t *testing.T) {
	app := withTestApp(t)

	serveLatestRelease(t, map[string]any{"tag_name": "v99.0.0"}, http.StatusOK)
	resp := app.get("/admin/update/check")
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Contains(t, resp.body, "v99.0.0")

	// The dialog markup names the release either way, so the banner is what
	// says whether there is anything to install.
	require.NotContains(t, resp.body, "up to date")

	serveLatestRelease(t, map[string]any{"tag_name": "v0.0.1"}, http.StatusOK)
	resp = app.get("/admin/update/check")
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Contains(t, resp.body, "up to date", "an older release is not an update")
}

// A rate-limited check used to unmarshal into an empty release whose blank tag
// sorts below every real version, so a failed check rendered as "up to date".
func TestUpdateCheckFailsLoudlyOnANon200(t *testing.T) {
	app := withTestApp(t)

	serveLatestRelease(t, nil, http.StatusForbidden)
	require.Equal(t, http.StatusInternalServerError, app.get("/admin/update/check").status)
}

func TestUpdatePagesRender(t *testing.T) {
	app := withTestApp(t)

	serveLatestRelease(t, map[string]any{"tag_name": "v0.0.1"}, http.StatusOK)

	require.Equal(t, http.StatusOK, app.get("/admin/update").status)
	require.Equal(t, http.StatusOK, app.get("/admin/update/after-update").status)
}

// postUpdate replaces the running binary, so only the paths that stop short of
// downloading are safe to drive: nothing newer to fetch, a failed lookup, and a
// release with no asset for this platform.
func TestPostUpdateStopsBeforeDownloadingWhenItShould(t *testing.T) {
	app := withTestApp(t)

	serveLatestRelease(t, map[string]any{"tag_name": VERSION}, http.StatusOK)
	require.Less(t, app.post("/admin/update", nil).status, 400)

	serveLatestRelease(t, nil, http.StatusForbidden)
	require.Equal(t, http.StatusInternalServerError, app.post("/admin/update", nil).status)

	serveLatestRelease(t, map[string]any{
		"tag_name": "v99.0.0",
		"assets":   []map[string]string{{"name": "statusnook_plan9_sparc", "url": "http://x"}},
	}, http.StatusOK)
	require.Equal(t, http.StatusInternalServerError, app.post("/admin/update", nil).status,
		"no asset for this platform is a failure, not a silent no-op")
}

// GitHub being unreachable, or answering with something that is not a release,
// is a failed check -- never a quiet "up to date".
func TestUpdateCheckFailsWhenGitHubIsUnusable(t *testing.T) {
	app := withTestApp(t)

	previous := githubAPIBaseURL
	githubAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { githubAPIBaseURL = previous })

	require.Equal(t, http.StatusInternalServerError, app.get("/admin/update/check").status)
	require.Equal(t, http.StatusInternalServerError, app.post("/admin/update", nil).status)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	t.Cleanup(server.Close)
	githubAPIBaseURL = server.URL

	require.Equal(t, http.StatusInternalServerError, app.get("/admin/update/check").status)
	require.Equal(t, http.StatusInternalServerError, app.post("/admin/update", nil).status)
}
