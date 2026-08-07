package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// A running instance: the real router, the real database wiring (initDB, the
// migration runner, the schema), a real session cookie. Handlers reach the
// database through package globals, so this swaps them and puts them back.
type testApp struct {
	t         *testing.T
	server    *httptest.Server
	client    *http.Client
	csrfToken string
}

// A brand new instance, exactly as a first boot leaves it: schema.sql's
// setup=domain, no account, nobody logged in. TestSetupFlow drives it forward.
func newTestApp(t *testing.T) *testApp {
	t.Helper()

	// initDB writes statusnook-data/ relative to the working directory.
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(wd)) })

	previousDB, previousRWDB := db, rwDB
	db = initDB(false)
	rwDB = initDB(true)
	rwDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		db.Close()
		rwDB.Close()
		db, rwDB = previousDB, previousRWDB
	})

	// Handlers that start a background loop hand it these; nil would panic.
	previousCtx, previousCancel := appCtx, cancelAppCtx
	appCtx, cancelAppCtx = context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelAppCtx()
		appWg.Wait()
		appCtx, cancelAppCtx = previousCtx, previousCancel
	})

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, loadMetaState(tx, "false"))
	require.NoError(t, tx.Commit())

	metaConfigFileEnabled.Store(false)

	// The login throttle is a package-level map keyed by client IP, and every
	// test here shares 127.0.0.1. Without this, a test that spends the ten
	// attempts locks every test that runs after it out of logging in.
	loginLimiterMu.Lock()
	clear(loginLimiter)
	loginLimiterMu.Unlock()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)

	app := &testApp{
		t:      t,
		server: httptest.NewServer(newRouter()),
		client: &http.Client{
			Jar: jar,
			// A redirect is an assertable outcome here, not something to chase.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	t.Cleanup(app.server.Close)

	return app
}

func withTestApp(t *testing.T) *testApp {
	t.Helper()

	app := newTestApp(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	// Past setup, so the redirect middleware lets requests through.
	require.NoError(t, updateMetaValue(tx, "setup", "done"))
	require.NoError(t, updateMetaValue(tx, "name", "Test Status"))

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2hunter2"), bcrypt.MinCost)
	require.NoError(t, err)
	_, err = createUser(tx, "admin", string(hash))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	metaSetup.Store("done")
	metaName.Store("Test Status")

	app.login()

	return app
}

func (a *testApp) login() {
	a.t.Helper()

	resp := a.post("/login", url.Values{"username": {"admin"}, "password": {"hunter2hunter2"}})
	require.Equal(a.t, http.StatusOK, resp.status, "login failed: %s", resp.body)
	require.Equal(a.t, "/admin/alerts", resp.header.Get("HX-Location"))

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	// The page injects the CSRF token with its own JavaScript, so a test that
	// wants to POST has to read it the way the page would be handed it. Newest
	// first: a test that logs in twice, or accepts an invitation, leaves more
	// than one session behind, and only the last one matches the cookie jar.
	require.NoError(a.t, tx.QueryRow(
		"select csrf_token from session order by id desc limit 1",
	).Scan(&a.csrfToken))
}

type testResponse struct {
	status int
	body   string
	header http.Header
}

func (a *testApp) do(method string, path string, body io.Reader) testResponse {
	a.t.Helper()

	req, err := http.NewRequest(method, a.server.URL+path, body)
	require.NoError(a.t, err)

	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("csrf-token", a.csrfToken)

	resp, err := a.client.Do(req)
	require.NoError(a.t, err)
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	require.NoError(a.t, err)

	return testResponse{status: resp.StatusCode, body: string(out), header: resp.Header}
}

func (a *testApp) get(path string) testResponse {
	a.t.Helper()

	return a.do(http.MethodGet, path, nil)
}

func (a *testApp) post(path string, form url.Values) testResponse {
	a.t.Helper()

	return a.do(http.MethodPost, path, strings.NewReader(form.Encode()))
}

func (a *testApp) delete(path string) testResponse {
	a.t.Helper()

	return a.do(http.MethodDelete, path, nil)
}
