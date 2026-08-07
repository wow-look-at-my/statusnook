package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Answers every GitHub call with one status and body, whatever was asked for.
func serveGitHubStatus(t *testing.T, status int, body string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	previous := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previous })
}

func githubSyncForm() url.Values {
	return url.Values{
		"config-file":           {"on"},
		"github-managed":        {"on"},
		"github-repo-url":       {"https://github.com/example/status"},
		"github-branch":         {"master"},
		"github-config-path":    {"config.yaml"},
		"github-token":          {"ghtoken"},
		"github-webhook-secret": {"hooksecret"},
	}
}

// The settings form's job is to turn GitHub's answer into something the person
// filling it in can act on, and every one of those answers means a different
// thing to fix.
func TestConfigSettingsExplainsWhatGitHubAnswered(t *testing.T) {
	app := withTestApp(t)

	serveGitHubStatus(t, http.StatusNotFound, `{"message":"Not Found"}`)
	resp := app.post("/admin/settings/config-settings", githubSyncForm())
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "repository could not be found")

	serveGitHubStatus(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)
	resp = app.post("/admin/settings/config-settings", githubSyncForm())
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "personal access token")

	// Anything else used to write a 400 with an empty body, so the form
	// reported nothing and looked like it had ignored the save.
	serveGitHubStatus(t, http.StatusServiceUnavailable, `{"message":"unavailable"}`)
	resp = app.post("/admin/settings/config-settings", githubSyncForm())
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "503")

	// GitHub unreachable at all.
	previous := githubAPIBaseURL
	githubAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { githubAPIBaseURL = previous })

	resp = app.post("/admin/settings/config-settings", githubSyncForm())
	require.Equal(t, http.StatusInternalServerError, resp.status)
	require.Contains(t, resp.body, "unexpected error")

	// Nothing was saved by any of them.
	require.False(t, metaConfigFileEnabled.Load())
	require.NotEqual(t, "true", app.metaValue("githubManagedConfig"))
}

// The webhook's failure modes, from the signature onwards. Every one of them
// has to leave the instance's config exactly as it was.
func TestConfigWebhookFailureModes(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	gh.serve(fullConfig, "sha-one")
	require.Less(t, app.deliverSignedWebhook("{}").status, 400)
	require.Contains(t, app.serviceNames(), "Website")

	// A signature that is not hex at all.
	require.GreaterOrEqual(t, app.deliverWebhook("{}", "sha256=zzzz").status, 400)

	// A payload past the 1 MB cap: the body is buffered before the signature is
	// checked, so an unauthenticated client could otherwise stream until the
	// read timeout, over and over.
	big := strings.Repeat("x", 2<<20)
	require.GreaterOrEqual(t, app.deliverSignedWebhook(big).status, 400)

	// GitHub answering with something that is not the contents API's shape.
	gh.serveRaw("not json at all")
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)

	// Contents that are not base64.
	gh.serveRaw(`{"type":"file","content":"!!!not base64!!!","sha":"sha-two"}`)
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)

	// A file that is not YAML at all comes back as a reported error, not a 500,
	// because the pusher needs to read it in the delivery log.
	gh.serve("services: [oh: no", "sha-three")
	resp := app.deliverSignedWebhook("{}")
	require.Equal(t, http.StatusUnprocessableEntity, resp.status)
	require.Contains(t, resp.body, "did not find expected",
		"the parser's own message, with its yaml: prefix trimmed")

	// Through all of it the applied config is the one from the first delivery.
	require.Contains(t, app.serviceNames(), "Website")
	require.Equal(t, "sha-one", app.metaValue("githubConfigSHA"))
}
