package main

import (
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServer boots the real routing table against a temporary database, with an
// admin account, and returns a client that carries its session.
type testServer struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	csrf   string
}

var csrfPattern = regexp.MustCompile(`csrf-token"\] = "([^"]*)"`)

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	useTestDBs(t)
	setTestSecretKey(t)

	// Complete setup the way the environment bootstrap does, so the routes
	// under test are not all redirects to /setup.
	tx, err := rwDB.Begin()
	require.NoError(t, err)

	require.NoError(t, updateMetaValue(tx, "setup", "done"))
	require.NoError(t, updateMetaValue(tx, "name", "Test Nook"))
	require.NoError(t, updateMetaValue(tx, "ssl", "false"))
	require.NoError(t, tx.Commit())

	prevSetup, prevName, prevSSL := metaSetup, metaName, metaSSL
	metaSetup, metaName, metaSSL = "done", "Test Nook", "false"
	t.Cleanup(func() { metaSetup, metaName, metaSSL = prevSetup, prevName, prevSSL })

	env.AdminUsername = "admin"
	env.AdminPassword = "test-password"

	tx, err = rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, ensureEnvAdmin(tx))
	require.NoError(t, tx.Commit())

	server := httptest.NewServer(newRouter())
	t.Cleanup(server.Close)

	jar, err := newCookieJar()
	require.NoError(t, err)

	ts := &testServer{
		t:      t,
		server: server,
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	ts.login()

	return ts
}

func (ts *testServer) login() {
	ts.t.Helper()

	resp := ts.post("/login", url.Values{
		"username": {"admin"},
		"password": {"test-password"},
	})
	require.Equal(ts.t, http.StatusOK, resp.code, "login failed: %s", resp.body)

	// The CSRF token is rendered into every page for htmx to send back.
	page := ts.get("/admin/settings")
	require.Equal(ts.t, http.StatusOK, page.code)

	match := csrfPattern.FindStringSubmatch(page.body)
	require.Len(ts.t, match, 2, "no csrf token in the settings page")

	ts.csrf = unescapeJS(match[1])
}

// unescapeJS undoes the JS string escaping html/template applies to the token.
func unescapeJS(value string) string {
	value = regexp.MustCompile(`\\u00([0-9a-fA-F]{2})`).ReplaceAllStringFunc(
		value,
		func(match string) string {
			var code int
			for _, c := range match[len(match)-2:] {
				code = code*16 + digitValue(byte(c))
			}
			return string(rune(code))
		},
	)

	return strings.ReplaceAll(value, `\/`, "/")
}

func digitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

type testResponse struct {
	code int
	body string
	hdr  http.Header
}

func (ts *testServer) do(req *http.Request) testResponse {
	ts.t.Helper()

	resp, err := ts.client.Do(req)
	require.NoError(ts.t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(ts.t, err)

	return testResponse{code: resp.StatusCode, body: string(body), hdr: resp.Header}
}

func (ts *testServer) get(path string) testResponse {
	ts.t.Helper()

	req, err := http.NewRequest(http.MethodGet, ts.server.URL+path, nil)
	require.NoError(ts.t, err)

	return ts.do(req)
}

func (ts *testServer) post(path string, form url.Values) testResponse {
	ts.t.Helper()

	return ts.send(http.MethodPost, path, form)
}

func (ts *testServer) send(method string, path string, form url.Values) testResponse {
	ts.t.Helper()

	req, err := http.NewRequest(method, ts.server.URL+path, strings.NewReader(form.Encode()))
	require.NoError(ts.t, err)

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ts.csrf != "" {
		req.Header.Set("csrf-token", ts.csrf)
	}

	return ts.do(req)
}

// newCookieJar returns a jar that keeps the session cookie between requests.
func newCookieJar() (http.CookieJar, error) {
	return cookiejar.New(nil)
}

// lastID returns the highest id linked under a path prefix, so tests do not
// depend on the ids the seed data happens to occupy.
func (ts *testServer) lastID(page string, prefix string) string {
	ts.t.Helper()

	body := ts.get(page).body
	matches := regexp.MustCompile(
		regexp.QuoteMeta(prefix)+`/(\d+)`,
	).FindAllStringSubmatch(body, -1)
	require.NotEmpty(ts.t, matches, "no %s link in %s", prefix, page)

	highest := "0"
	for _, match := range matches {
		if len(match[1]) > len(highest) || (len(match[1]) == len(highest) && match[1] > highest) {
			highest = match[1]
		}
	}

	return highest
}

// TestPagesRender walks every read-only page. Each one renders the layout plus
// its own template, so a missing template, a broken route or a nil dereference
// in a page shows up here.
func TestPagesRender(t *testing.T) {
	ts := newTestServer(t)

	// Give the pages something to show.
	require.Equal(t, http.StatusOK, ts.post("/admin/services/create", url.Values{
		"name": {"Website"}, "helper-text": {"example.com"},
	}).code)

	require.Equal(t, http.StatusOK, ts.post("/admin/monitors/create", url.Values{
		"name": {"Homepage"}, "url": {"https://example.com"}, "method": {"GET"},
		"frequency": {"60"}, "timeout": {"5"}, "attempts": {"1"},
	}).code)

	require.Equal(t, http.StatusOK, ts.post("/admin/notifications/mail-groups/create", url.Values{
		"name": {"Core"}, "description": {"Core team"}, "members": {"core@example.com"},
	}).code)

	pages := []string{
		"/",
		"/history",
		"/history?period=30d",
		"/login",
		"/healthz",
		"/admin/alerts",
		"/admin/alerts/create",
		"/admin/alerts/notifications",
		"/admin/monitors",
		"/admin/monitors/create",
		"/admin/monitors/1",
		"/admin/monitors/1/edit",
		"/admin/monitors/1/view",
		"/admin/services",
		"/admin/services/create",
		"/admin/services/1/edit",
		"/admin/notifications",
		"/admin/notifications/create",
		"/admin/notifications/mail-groups/create",
		"/admin/notifications/mail-groups/1/edit",
		"/admin/notifications/mail-groups/1/view",
		"/admin/settings",
		"/admin/settings/config-settings",
		"/admin/settings/users/1/edit",
		"/admin/update",
		"/unsubscribe?token=unknown",
		"/subscribe/email/confirm?token=unknown",
	}

	for _, page := range pages {
		t.Run(page, func(t *testing.T) {
			resp := ts.get(page)

			assert.Less(t, resp.code, 500, "%s returned %d: %s", page, resp.code, resp.body)
			assert.NotContains(t, resp.body, "&lt;div", "%s double-escaped its markup", page)
		})
	}
}

// TestMonitorLifecycle covers creating, editing and deleting a monitor through
// the forms, which is the path most likely to break silently.
func TestMonitorLifecycle(t *testing.T) {
	ts := newTestServer(t)

	created := ts.post("/admin/monitors/create", url.Values{
		"name": {"Homepage"}, "url": {"https://example.com"}, "method": {"GET"},
		"frequency": {"60"}, "timeout": {"5"}, "attempts": {"1"},
	})
	require.Equal(t, http.StatusOK, created.code, created.body)

	assert.Contains(t, ts.get("/admin/monitors").body, "Homepage")

	edited := ts.post("/admin/monitors/1/edit", url.Values{
		"name": {"Homepage renamed"}, "url": {"https://example.com/status"}, "method": {"GET"},
		"frequency": {"900"}, "timeout": {"30"}, "attempts": {"2"},
	})
	require.Equal(t, http.StatusOK, edited.code, edited.body)

	page := ts.get("/admin/monitors/1/edit")
	assert.Contains(t, page.body, "Homepage renamed")
	assert.Contains(t, page.body, `value="900" required/>`)

	// The allow-list is enforced on the form, not just in the config file.
	rejected := ts.post("/admin/monitors/1/edit", url.Values{
		"name": {"Homepage"}, "url": {"https://example.com"}, "method": {"GET"},
		"frequency": {"45"}, "timeout": {"5"}, "attempts": {"1"},
	})
	assert.Equal(t, http.StatusBadRequest, rejected.code)

	badURL := ts.post("/admin/monitors/create", url.Values{
		"name": {"Bad"}, "url": {"not a url"}, "method": {"GET"},
		"frequency": {"60"}, "timeout": {"5"}, "attempts": {"1"},
	})
	assert.Equal(t, http.StatusBadRequest, badURL.code)
	assert.Contains(t, badURL.body, "Invalid URL")

	deleted := ts.send(http.MethodDelete, "/admin/monitors/1", nil)
	assert.Equal(t, http.StatusOK, deleted.code)
	assert.NotContains(t, ts.get("/admin/monitors").body, "Homepage renamed")
}

// TestAlertLifecycle covers the alert flow that drives the public status page.
func TestAlertLifecycle(t *testing.T) {
	ts := newTestServer(t)

	require.Equal(t, http.StatusOK, ts.post("/admin/services/create", url.Values{
		"name": {"Website"}, "helper-text": {"example.com"},
	}).code)

	created := ts.post("/admin/alerts/create", url.Values{
		"title": {"Database maintenance"}, "type": {"maintenance"},
		"message": {"We are upgrading the database"}, "services": {"1"},
		"severity": {"blue"},
	})
	require.Equal(t, http.StatusOK, created.code, created.body)

	assert.Contains(t, ts.get("/").body, "Database maintenance")
	assert.Contains(t, ts.get("/admin/alerts").body, "Database maintenance")

	// The schema seeds a welcome alert, so the new one is not id 1.
	id := ts.lastID("/admin/alerts", "/admin/alerts")

	messaged := ts.post("/admin/alerts/"+id+"/messages", url.Values{
		"message": {"Upgrade finished"},
	})
	require.Equal(t, http.StatusOK, messaged.code, messaged.body)

	// An empty update must not be stored.
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/admin/alerts/"+id+"/messages", url.Values{"message": {""}}).code)
	assert.Contains(t, ts.get("/admin/alerts/"+id).body, "Upgrade finished")

	resolved := ts.post("/admin/alerts/"+id+"/resolve", nil)
	assert.Equal(t, http.StatusOK, resolved.code)

	unresolved := ts.post("/admin/alerts/"+id+"/unresolve", nil)
	assert.Equal(t, http.StatusOK, unresolved.code)

	deleted := ts.send(http.MethodDelete, "/admin/alerts/"+id, nil)
	assert.Equal(t, http.StatusOK, deleted.code)
	assert.NotContains(t, ts.get("/admin/alerts").body, "Database maintenance")
}

// TestServiceAndMailGroupLifecycle covers the remaining CRUD surfaces.
func TestServiceAndMailGroupLifecycle(t *testing.T) {
	ts := newTestServer(t)

	require.Equal(t, http.StatusOK, ts.post("/admin/services/create", url.Values{
		"name": {"Website"}, "helper-text": {"example.com"},
	}).code)

	edited := ts.post("/admin/services/1/edit", url.Values{
		"name": {"Website renamed"}, "helper-text": {"example.org"},
	})
	require.Equal(t, http.StatusOK, edited.code, edited.body)
	assert.Contains(t, ts.get("/admin/services").body, "Website renamed")

	require.Equal(t, http.StatusOK, ts.post("/admin/notifications/mail-groups/create", url.Values{
		"name": {"Core"}, "description": {"Core team"}, "members": {"core@example.com"},
	}).code)

	group := ts.get("/admin/notifications/mail-groups/1/view")
	assert.Equal(t, http.StatusOK, group.code)
	assert.Contains(t, group.body, "core@example.com")

	assert.Equal(t, http.StatusOK,
		ts.send(http.MethodDelete, "/admin/notifications/mail-groups/1", nil).code)
	assert.Equal(t, http.StatusOK, ts.send(http.MethodDelete, "/admin/services/1", nil).code)
}

// TestConfigEditor covers applying a config document through the settings page,
// which is the same code path the GitHub sync uses.
func TestConfigEditor(t *testing.T) {
	ts := newTestServer(t)

	enable := ts.post("/admin/settings/config-settings", url.Values{"config-file": {"on"}})
	require.Equal(t, http.StatusOK, enable.code, enable.body)

	applied := ts.post("/admin/settings/config", url.Values{"config": {testConfig}})
	require.Equal(t, http.StatusOK, applied.code, applied.body)
	assert.Contains(t, ts.get("/admin/monitors").body, "Homepage")

	broken := ts.post("/admin/settings/config", url.Values{
		"config": {"general-settings:\n  name: [oops"},
	})
	assert.Equal(t, http.StatusBadRequest, broken.code)
	assert.Contains(t, broken.body, "save-overlay--error")

	invalid := ts.post("/admin/settings/config", url.Values{
		"config": {"general-settings: {}\n"},
	})
	assert.Equal(t, http.StatusBadRequest, invalid.code)
	assert.Contains(t, invalid.body, "general-settings.name")

	// With the config file in charge, the form-based editors are closed.
	assert.Equal(t, http.StatusBadRequest, ts.get("/admin/monitors/create").code)
}

// TestSecretRoundTrip covers the encrypt/decrypt helper the config file uses.
func TestSecretRoundTrip(t *testing.T) {
	ts := newTestServer(t)

	encrypted := ts.post("/admin/settings/secrets", url.Values{
		"action": {"encrypt"}, "input": {"smtp-password"},
	})
	require.Equal(t, http.StatusOK, encrypted.code, encrypted.body)

	value := regexp.MustCompile(`value="(secret_[^"]+)"`).FindStringSubmatch(encrypted.body)
	require.Len(t, value, 2, "no secret in %s", encrypted.body)

	decrypted := ts.post("/admin/settings/secrets", url.Values{
		"action": {"decrypt"}, "input": {html.UnescapeString(value[1])},
	})
	require.Equal(t, http.StatusOK, decrypted.code, decrypted.body)
	assert.Contains(t, decrypted.body, "smtp-password")
}

// TestAuthenticationRequired checks that the admin area is closed without a
// session, and that state-changing requests need the CSRF token.
func TestAuthenticationRequired(t *testing.T) {
	ts := newTestServer(t)

	anonymous := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, path := range []string{"/admin/settings", "/admin/monitors", "/admin/alerts"} {
		resp, err := anonymous.Get(ts.server.URL + path)
		require.NoError(t, err)
		resp.Body.Close()

		assert.Equal(t, http.StatusFound, resp.StatusCode, path)
		assert.Equal(t, "/login", resp.Header.Get("Location"), path)
	}

	// A session without the CSRF header cannot change anything.
	prev := ts.csrf
	ts.csrf = ""
	assert.Equal(t, http.StatusForbidden, ts.post("/admin/services/create", url.Values{
		"name": {"Nope"},
	}).code)
	ts.csrf = prev

	// A wrong token is rejected as firmly as a missing one.
	ts.csrf = "not-the-right-token"
	assert.Equal(t, http.StatusForbidden, ts.post("/admin/services/create", url.Values{
		"name": {"Nope"},
	}).code)
	ts.csrf = prev
}

// TestLogout drops the session.
func TestLogout(t *testing.T) {
	ts := newTestServer(t)

	require.Equal(t, http.StatusOK, ts.post("/logout", nil).code)

	resp := ts.get("/admin/settings")
	assert.Equal(t, http.StatusFound, resp.code)
	assert.Equal(t, "/login", resp.hdr.Get("Location"))
}
