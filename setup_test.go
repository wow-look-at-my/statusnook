package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUnconfiguredServer boots the router on a database that has never been set
// up, which is how a fresh install starts.
func newUnconfiguredServer(t *testing.T) *testServer {
	t.Helper()

	useTestDBs(t)
	setTestSecretKey(t)

	prevSetup, prevName, prevSSL, prevDomain := metaSetup, metaName, metaSSL, metaDomain
	metaSetup, metaName, metaSSL, metaDomain = "domain", "", "false", ""
	t.Cleanup(func() {
		metaSetup, metaName, metaSSL, metaDomain = prevSetup, prevName, prevSSL, prevDomain
	})

	server := httptest.NewServer(newRouter())
	t.Cleanup(server.Close)

	jar, err := newCookieJar()
	require.NoError(t, err)

	return &testServer{
		t:      t,
		server: server,
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// TestSetupWizard walks the first-run flow: everything redirects to /setup, the
// domain step can be skipped, and the account and name steps complete it.
func TestSetupWizard(t *testing.T) {
	ts := newUnconfiguredServer(t)

	redirected := ts.get("/")
	assert.Equal(t, http.StatusFound, redirected.code)
	assert.Equal(t, "/setup", redirected.hdr.Get("Location"))

	// The apex domain probe used by statusnook.com answers on any instance.
	probe := ts.post("/setup/statusnook", nil)
	assert.Equal(t, http.StatusOK, probe.code)
	assert.Equal(t, "true", probe.hdr.Get("X-Statusnook-Setup"))

	domainPage := ts.get("/setup/domain")
	require.Equal(t, http.StatusOK, domainPage.code)

	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/domain", url.Values{"domain": {""}}).code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/domain", url.Values{"domain": {"http://example.com"}}).code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/domain", url.Values{"domain": {"192.0.2.1"}}).code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/domain", url.Values{"domain": {"not a domain"}}).code)

	// Skipping the domain still needs one to watch for, so an empty value is
	// rejected the same way.
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/skip-domain", url.Values{"domain": {""}}).code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/setup/skip-domain", url.Values{"domain": {"not a domain"}}).code)

	accepted := ts.post("/setup/domain", url.Values{"domain": {"status.example.com"}})
	require.Equal(t, http.StatusOK, accepted.code, accepted.body)
	assert.Equal(t, "/setup/account", accepted.hdr.Get("HX-Location"))
	assert.Equal(t, "account", metaSetup)
	assert.Equal(t, "status.example.com", metaDomain)

	accountPage := ts.get("/setup/account")
	require.Equal(t, http.StatusOK, accountPage.code)

	assert.Equal(t, http.StatusBadRequest, ts.post("/setup/account", url.Values{
		"username": {"admin"}, "password": {"short"}, "password-confirmation": {"short"},
	}).code)
	assert.Equal(t, http.StatusBadRequest, ts.post("/setup/account", url.Values{
		"username": {"admin"}, "password": {"long-enough"}, "password-confirmation": {"mismatch"},
	}).code)

	created := ts.post("/setup/account", url.Values{
		"username": {"admin"}, "password": {"long-enough"},
		"password-confirmation": {"long-enough"},
	})
	require.Equal(t, http.StatusOK, created.code, created.body)
	assert.Equal(t, "name", metaSetup)

	namePage := ts.get("/setup/name")
	require.Equal(t, http.StatusOK, namePage.code)

	assert.Equal(t, http.StatusBadRequest, ts.post("/setup/name", url.Values{"name": {""}}).code)

	named := ts.post("/setup/name", url.Values{"name": {"My Nook"}})
	require.Equal(t, http.StatusOK, named.code, named.body)

	assert.Equal(t, "done", metaSetup)
	assert.Equal(t, "My Nook", metaName)

	// Setup is closed once complete, and the session from the account step is
	// already signed in.
	assert.Equal(t, http.StatusFound, ts.get("/setup/account").code)
	assert.Contains(t, ts.get("/").body, "My Nook")
}

// TestInvitationFlow covers inviting a second admin and accepting the invite.
func TestInvitationFlow(t *testing.T) {
	ts := newTestServer(t)

	invited := ts.post("/admin/settings/users/invite", nil)
	require.Equal(t, http.StatusOK, invited.code, invited.body)

	// The handler stores the token and the settings page renders the link, so
	// read it back out of the database.
	token := ""
	require.NoError(t, db.QueryRow("select token from user_invitation").Scan(&token))
	require.NotEmpty(t, token)
	assert.Contains(t, ts.get("/admin/settings").body, "/invitation/"+token)

	page := ts.get("/invitation/" + token)
	assert.Equal(t, http.StatusOK, page.code)
	assert.Equal(t, http.StatusBadRequest, ts.get("/invitation/not-a-token").code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/invitation/not-a-token", url.Values{"username": {"nope"}}).code)

	// The invitee arrives in their own browser: accepting the invite signs them
	// in, so doing it on the admin's client would replace the admin's session.
	invitee := func(form url.Values) testResponse {
		t.Helper()

		resp, err := (&http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}).PostForm(ts.server.URL+"/invitation/"+token, form)
		require.NoError(t, err)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		return testResponse{code: resp.StatusCode, body: string(body), hdr: resp.Header}
	}

	assert.Equal(t, http.StatusBadRequest, invitee(url.Values{
		"username": {"second"}, "password": {"short"}, "password-confirmation": {"short"},
	}).code)

	assert.Equal(t, http.StatusBadRequest, invitee(url.Values{
		"username": {"second"}, "password": {"long-enough"},
		"password-confirmation": {"mismatch"},
	}).code)

	assert.Equal(t, http.StatusBadRequest, invitee(url.Values{
		"username": {""}, "password": {"long-enough"},
		"password-confirmation": {"long-enough"},
	}).code)

	assert.Equal(t, http.StatusBadRequest, invitee(url.Values{
		"username": {"admin"}, "password": {"long-enough"},
		"password-confirmation": {"long-enough"},
	}).code, "the existing username must be rejected")

	accepted := invitee(url.Values{
		"username": {"second"}, "password": {"long-enough"},
		"password-confirmation": {"long-enough"},
	})
	require.Equal(t, http.StatusOK, accepted.code, accepted.body)

	assert.Equal(t, 2, countRows(t, "select count(*) from user"))
	assert.Equal(t, 0, countRows(t, "select count(*) from user_invitation"),
		"an accepted invitation must be consumed")

	// A second invitation can be revoked instead of used.
	second := ts.post("/admin/settings/users/invite", nil)
	require.Equal(t, http.StatusOK, second.code)
	assert.Equal(t, 1, countRows(t, "select count(*) from user_invitation"))

	id := ""
	require.NoError(t, db.QueryRow("select id from user_invitation").Scan(&id))
	assert.Equal(t, http.StatusOK,
		ts.send(http.MethodDelete, "/admin/settings/users/invite/"+id, nil).code)
	assert.Equal(t, 0, countRows(t, "select count(*) from user_invitation"))
}

// TestUserManagement covers renaming a user, resetting a password, and deleting
// an account.
func TestUserManagement(t *testing.T) {
	ts := newTestServer(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	otherID, err := createUser(tx, "second", "hash")
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	page := ts.get("/admin/settings/users/1/edit")
	require.Equal(t, http.StatusOK, page.code)

	renamed := ts.post("/admin/settings/users/1/edit", url.Values{
		"username": {"renamed"}, "password": {"retain"},
	})
	require.Equal(t, http.StatusOK, renamed.code, renamed.body)
	assert.Contains(t, ts.get("/admin/settings").body, "renamed")

	// Renaming onto an existing username is rejected.
	clash := ts.post("/admin/settings/users/1/edit", url.Values{
		"username": {"second"}, "password": {"retain"},
	})
	assert.Equal(t, http.StatusBadRequest, clash.code)
	assert.Contains(t, clash.body, "already taken")

	assert.Equal(t, http.StatusNotFound, ts.post("/admin/settings/users/999/edit", url.Values{
		"username": {"ghost"}, "password": {"retain"},
	}).code)

	// An admin cannot delete the account they are signed in as.
	assert.Equal(t, http.StatusBadRequest,
		ts.send(http.MethodDelete, "/admin/settings/users/1", nil).code)

	assert.Equal(t, http.StatusOK, ts.send(
		http.MethodDelete, "/admin/settings/users/"+itoa(otherID), nil,
	).code)
	assert.Equal(t, 1, countRows(t, "select count(*) from user"))
}

// TestCrossAuthHandoff covers the one-time token that carries a session from
// the apex domain to a custom domain.
func TestCrossAuthHandoff(t *testing.T) {
	ts := newTestServer(t)

	issued := ts.post("/admin/resolve", nil)
	require.Equal(t, http.StatusOK, issued.code)
	require.NotEmpty(t, issued.body)

	// The probe endpoint the apex domain calls.
	probe := ts.get("/resolve")
	assert.Equal(t, http.StatusOK, probe.code)
	assert.Equal(t, "true", probe.hdr.Get("X-Statusnook"))

	// An already-signed-in browser is redirected straight through, without
	// spending the token.
	redirected := ts.get("/cross-auth?token=" + url.QueryEscape(issued.body))
	assert.Equal(t, http.StatusFound, redirected.code)

	// anon returns a client with no cookies at all, so every request lands on
	// the unauthenticated path.
	anon := func() *http.Client {
		return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}

	crossAuth := func(client *http.Client, query string) *http.Response {
		t.Helper()
		resp, err := client.Get(ts.server.URL + "/cross-auth" + query)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	assert.Equal(t, http.StatusBadRequest, crossAuth(anon(), "").StatusCode,
		"a request with no token is rejected")
	assert.Equal(t, http.StatusBadRequest, crossAuth(anon(), "?token=bogus").StatusCode,
		"an unknown token is rejected")

	// A fresh browser redeems the token for a session cookie.
	redeemed := crossAuth(anon(), "?token="+url.QueryEscape(issued.body))
	assert.Equal(t, http.StatusFound, redeemed.StatusCode)

	sessionSet := false
	for _, cookie := range redeemed.Cookies() {
		if cookie.Name == "session" && cookie.Value != "" {
			sessionSet = true
		}
	}
	assert.True(t, sessionSet, "redeeming a token must set a session cookie")

	assert.Equal(t, http.StatusBadRequest,
		crossAuth(anon(), "?token="+url.QueryEscape(issued.body)).StatusCode,
		"the token is single use")
}

// TestSelfSignedCertificate covers the certificate the standalone installer
// generates, and the move of legacy TLS material into the data directory.
func TestSelfSignedCertificate(t *testing.T) {
	dir := t.TempDir()

	prev := env
	env = envConfig{DataDir: dir}
	t.Cleanup(func() { env = prev })

	GenerateSelfSignedCertificate()

	for _, path := range []string{selfSignedCertPath(), selfSignedKeyPath()} {
		info, err := os.Stat(path)
		require.NoError(t, err, path)
		assert.Positive(t, info.Size())
	}

	assert.Equal(t, filepath.Join(dir, "certmagic"), certmagicDir())
}

func TestMigrateLegacyTLSPaths(t *testing.T) {
	work := t.TempDir()

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(work))
	t.Cleanup(func() { os.Chdir(cwd) })

	prev := env
	env = envConfig{DataDir: filepath.Join(work, "statusnook-data")}
	t.Cleanup(func() { env = prev })

	require.NoError(t, os.MkdirAll(env.DataDir, 0o700))
	require.NoError(t, os.MkdirAll("certmagic", 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join("certmagic", "marker"), []byte("cert"), 0o600,
	))
	require.NoError(t, os.WriteFile(SELF_SIGNED_CERT_NAME, []byte("cert"), 0o600))

	migrateLegacyTLSPaths()

	_, err = os.Stat(filepath.Join(certmagicDir(), "marker"))
	assert.NoError(t, err, "the legacy certmagic directory should have moved")

	_, err = os.Stat(selfSignedCertPath())
	assert.NoError(t, err, "the legacy self-signed certificate should have moved")
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
