package main

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// The first-boot wizard. Its gate middleware pins the instance to exactly one
// step at a time, so this walks the whole thing and checks each step both
// advances the state and refuses to be skipped.
func TestSetupFlowWalksEveryStep(t *testing.T) {
	app := newTestApp(t)

	// schema.sql seeds setup=domain, and everything outside /setup redirects.
	require.Equal(t, "domain", metaSetup.Load())

	resp := app.get("/")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/setup", resp.header.Get("Location"))

	resp = app.get("/setup")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/setup/domain", resp.header.Get("Location"))

	require.Equal(t, http.StatusOK, app.get("/setup/domain").status)

	// A later step is not reachable yet.
	resp = app.get("/setup/name")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/setup/domain", resp.header.Get("Location"))

	// The domain has to look like one.
	require.Equal(t, http.StatusBadRequest, app.post("/setup/skip-domain", nil).status)
	require.Equal(t, http.StatusBadRequest,
		app.post("/setup/skip-domain", url.Values{"domain": {"not a domain"}}).status)

	resp = app.post("/setup/skip-domain", url.Values{"domain": {"status.example.com"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "/setup/account", resp.header.Get("HX-Location"))
	require.Equal(t, "account", metaSetup.Load())
	require.Equal(t, "status.example.com", metaUnconfirmedDomain.Load())

	require.Equal(t, http.StatusOK, app.get("/setup/account").status)

	require.Equal(t, http.StatusBadRequest, app.post("/setup/account", url.Values{
		"username":              {"admin"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"something else"},
	}).status)

	resp = app.post("/setup/account", url.Values{
		"username":              {"admin"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"hunter2hunter2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "name", metaSetup.Load())
	require.Contains(t, app.usernames(), "admin")

	require.Equal(t, http.StatusOK, app.get("/setup/name").status)
	require.Equal(t, http.StatusBadRequest, app.post("/setup/name", nil).status)

	resp = app.post("/setup/name", url.Values{"name": {"Test Status"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "done", metaSetup.Load())
	require.Equal(t, "Test Status", metaName.Load())

	// Setup is over: the public page serves, and the wizard is closed.
	require.Equal(t, http.StatusOK, app.get("/").status)

	resp = app.get("/setup/name")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/", resp.header.Get("Location"))
}

// The probe an installer script uses to tell a statusnook apart from whatever
// else answers on that host. It has to work before setup and after.
func TestSetupStatusnookProbeAnswersAtAnyStage(t *testing.T) {
	app := newTestApp(t)

	resp := app.post("/setup/statusnook", nil)
	require.Equal(t, http.StatusOK, resp.status)
	require.Equal(t, "true", resp.header.Get("X-Statusnook-Setup"))
	require.Equal(t, "*", resp.header.Get("Access-Control-Allow-Origin"))

	resp = app.do(http.MethodOptions, "/setup/statusnook", nil)
	require.Equal(t, http.StatusOK, resp.status)
	require.Equal(t, "true", resp.header.Get("X-Statusnook-Setup"))
}
