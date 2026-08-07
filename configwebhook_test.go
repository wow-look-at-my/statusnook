package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const webhookSecret = "hooksecret"

// Stands in for the GitHub contents API. Returns whatever config the test set,
// under whatever sha it set, and counts how often it was asked.
type fakeGitHub struct {
	server  *httptest.Server
	config  atomic.Pointer[string]
	sha     atomic.Pointer[string]
	raw     atomic.Pointer[string]
	fetches atomic.Int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	gh := &fakeGitHub{}
	gh.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer ghtoken", r.Header.Get("Authorization"))

		// The settings page checks the repo exists before it saves; only the
		// contents call is the one the webhook makes.
		if !strings.Contains(r.URL.Path, "/contents/") {
			w.Write([]byte(`{"full_name":"example/status"}`))
			return
		}

		gh.fetches.Add(1)

		if raw := gh.raw.Load(); raw != nil {
			w.Write([]byte(*raw))
			return
		}

		config, sha := gh.config.Load(), gh.sha.Load()
		if config == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}

		require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
			"type":    "file",
			"content": base64.StdEncoding.EncodeToString([]byte(*config)),
			"sha":     *sha,
		}))
	}))
	t.Cleanup(gh.server.Close)

	previous := githubAPIBaseURL
	githubAPIBaseURL = gh.server.URL
	t.Cleanup(func() { githubAPIBaseURL = previous })

	// Saving the settings checks the config path resolves, so something has to
	// be there before enableGitHubSync runs.
	gh.serve("general-settings:\n  name: Test Status\n", "sha-zero")

	return gh
}

func (g *fakeGitHub) serve(config string, sha string) {
	g.raw.Store(nil)
	g.config.Store(&config)
	g.sha.Store(&sha)
}

func (g *fakeGitHub) serveMissing() {
	g.config.Store(nil)
}

// Answers the contents call with a body of the test's choosing rather than the
// shape the API actually returns.
func (g *fakeGitHub) serveRaw(body string) {
	g.raw.Store(&body)
}

// The webhook is a public route, so everything before the config fetch is a
// gate: the feature has to be on, the signature has to be present, and it has
// to verify against the stored secret.
func TestConfigWebhookRejectsUntrustedDeliveries(t *testing.T) {
	app := withTestApp(t)
	newFakeGitHub(t)

	// Never configured, so there is not even a githubManagedConfig row: this
	// route is public and answered every delivery with a 500 before.
	require.Equal(t, http.StatusBadRequest, app.deliverWebhook("{}", "sha256=deadbeef").status)

	app.enableGitHubSync(t)

	require.Equal(t, http.StatusBadRequest, app.deliverWebhook("{}", "").status)
	require.Equal(t, http.StatusBadRequest, app.deliverWebhook("{}", "md5=whatever").status)

	// A well-formed signature over the wrong key is refused.
	wrong := hmac.New(sha256.New, []byte("not-the-secret"))
	wrong.Write([]byte("{}"))
	resp := app.deliverWebhook("{}", "sha256="+hex.EncodeToString(wrong.Sum(nil)))
	require.Equal(t, http.StatusInternalServerError, resp.status)
}

func TestConfigWebhookAppliesAPushedConfig(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	gh.serve(fullConfig, "sha-one")

	resp := app.deliverSignedWebhook(`{"ref":"refs/heads/master"}`)
	require.Less(t, resp.status, 400, resp.body)

	require.Contains(t, app.serviceNames(), "Website")
	require.Contains(t, app.monitorNames(), "API")
	require.Equal(t, "sha-one", app.metaValue("githubConfigSHA"))
	require.Empty(t, app.metaValue("githubConfigErrors"))

	// The same sha again is a no-op: the file is fetched, compared, and the
	// apply skipped.
	before := gh.fetches.Load()
	resp = app.deliverSignedWebhook(`{"ref":"refs/heads/master"}`)
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, before+1, gh.fetches.Load())
	require.Equal(t, "sha-one", app.metaValue("githubConfigSHA"))
}

// The dangerous case. applyConfig deletes as it validates, so a config that
// fails halfway has already removed whatever the file stopped naming -- the
// handler has to throw that transaction away and record only the errors.
func TestConfigWebhookDiscardsAHalfAppliedBadConfig(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	gh.serve(fullConfig, "sha-one")
	require.Less(t, app.deliverSignedWebhook("{}").status, 400)
	require.Contains(t, app.serviceNames(), "Website")

	gh.serve(`
general-settings:
  name: Test Status
monitors:
  broken:
    name: Broken
    url: not a url
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
`, "sha-two")

	resp := app.deliverSignedWebhook("{}")
	require.Equal(t, http.StatusUnprocessableEntity, resp.status,
		"GitHub's delivery log is the only place a pusher sees this")
	require.Contains(t, resp.body, "url is invalid")

	require.Contains(t, app.serviceNames(), "Website",
		"the rejected config must not have deleted the service it stopped naming")
	require.Contains(t, app.metaValue("githubConfigErrors"), "url is invalid")
	require.Equal(t, "sha-one", app.metaValue("githubConfigSHA"),
		"the sha stays put so a corrected push is not skipped as already-seen")
}

func TestConfigWebhookReportsAFetchFailure(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	// The file was there when the settings were saved and is gone now, which
	// is what a deleted path or a renamed branch looks like.
	gh.serveMissing()
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)
}

func (a *testApp) enableGitHubSync(t *testing.T) {
	t.Helper()

	resp := a.post("/admin/settings/config-settings", url.Values{
		"config-file":           {"on"},
		"github-managed":        {"on"},
		"github-repo-url":       {"https://github.com/example/status"},
		"github-branch":         {"master"},
		"github-config-path":    {"config.yaml"},
		"github-token":          {"ghtoken"},
		"github-webhook-secret": {webhookSecret},
	})
	require.Less(t, resp.status, 400, resp.body)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })
}

func (a *testApp) deliverWebhook(payload string, signature string) testResponse {
	a.t.Helper()

	req, err := http.NewRequest(
		http.MethodPost,
		a.server.URL+"/github-config-webhook",
		strings.NewReader(payload),
	)
	require.NoError(a.t, err)

	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}

	// Deliberately not a.client: GitHub delivers with no session cookie.
	resp, err := (&http.Client{}).Do(req)
	require.NoError(a.t, err)
	defer resp.Body.Close()

	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)

	return testResponse{status: resp.StatusCode, body: string(body[:n]), header: resp.Header}
}

func (a *testApp) deliverSignedWebhook(payload string) testResponse {
	a.t.Helper()

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(payload))

	return a.deliverWebhook(payload, "sha256="+hex.EncodeToString(mac.Sum(nil)))
}

func (a *testApp) metaValue(name string) string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	value, err := getMetaValue(tx, name)
	if err != nil {
		return ""
	}

	return value
}

// The delivery's own fetch of the config, and the two ways that fetch can be
// unusable rather than merely unsuccessful.
func TestConfigWebhookFailsWhenGitHubIsUnusable(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	previous := githubAPIBaseURL
	githubAPIBaseURL = "http://127.0.0.1:1"
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)

	// A body that is not the contents API's shape at all.
	githubAPIBaseURL = previous
	gh.serveRaw("not json")
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)

	// And one that is, but whose content is not base64.
	gh.serveRaw(`{"type":"file","content":"!!!","sha":"sha-bad"}`)
	require.Equal(t, http.StatusInternalServerError, app.deliverSignedWebhook("{}").status)
}

// A pushed config that does not validate takes a second write path: the apply
// is thrown away and only the errors are recorded, in a transaction of its own.
func TestConfigWebhookSurfacesAFailureWhileRecordingErrors(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	gh.serve(`
general-settings:
  name: Test Status
monitors:
  broken:
    name: Broken
    url: not a url
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
`, "sha-broken")

	withFaultyDB(app)

	for depth := int64(1); depth <= 120; depth++ {
		failAtStatement(depth)
		resp := app.deliverSignedWebhook("{}")
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"the webhook answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusUnprocessableEntity, app.deliverSignedWebhook("{}").status)
}
