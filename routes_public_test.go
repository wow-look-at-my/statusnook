package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The public pages are what an outage's audience sees, so they have to render
// with an alert running and with none at all.
func TestPublicPagesRender(t *testing.T) {
	app := withTestApp(t)

	for _, path := range []string{"/", "/history", "/healthz"} {
		resp := app.get(path)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", path, resp.body)
	}

	// /login bounces an already-authenticated visitor into the admin area.
	resp := app.get("/login")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/admin/alerts", resp.header.Get("Location"))

	serviceID := app.createService("Web", "the site")
	app.createAlert("Outage", serviceID, "we are looking")

	require.Contains(t, app.get("/").body, "Outage")
	require.Contains(t, app.get("/history").body, "Outage")

	// The severity strip is a separate fragment the page polls for.
	require.Equal(t, http.StatusOK, app.get("/resolve").status)
}

func TestHealthzAnswersWithoutASession(t *testing.T) {
	app := withTestApp(t)

	// No cookie, no CSRF token: a probe is not a user.
	res, err := http.Get(app.server.URL + "/healthz")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestStaticAssetsAreServed(t *testing.T) {
	app := withTestApp(t)

	resp := app.get("/static/main.css")
	require.Equal(t, http.StatusOK, resp.status)
	require.NotEmpty(t, resp.body)

	// Eleven assets exist only gzipped; serving them to a client that did not
	// ask for gzip used to 404 everyone else.
	resp = app.get("/static/htmx-1.9.12.js")
	require.Equal(t, http.StatusOK, resp.status)
	require.NotEmpty(t, resp.body)

	// Directory listings are off: neuter turns a bare directory into a 404.
	require.Equal(t, http.StatusNotFound, app.get("/static/").status)
}

func TestLogoutEndsTheSession(t *testing.T) {
	app := withTestApp(t)

	require.Equal(t, http.StatusOK, app.get("/admin/alerts").status)

	resp := app.post("/logout", nil)
	require.Less(t, resp.status, 400, resp.body)

	// The cookie jar still holds the old token; it just no longer resolves.
	resp = app.get("/admin/alerts")
	require.Equal(t, http.StatusFound, resp.status)
	require.Equal(t, "/login", resp.header.Get("Location"))
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "Incorrect credentials")

	resp = app.post("/login", url.Values{"username": {"admin"}})
	require.Equal(t, http.StatusBadRequest, resp.status)

	resp = app.post("/login", url.Values{"password": {"hunter2hunter2"}})
	require.Equal(t, http.StatusBadRequest, resp.status)

	// An unknown username must not read differently from a wrong password.
	resp = app.post("/login", url.Values{"username": {"nobody"}, "password": {"hunter2hunter2"}})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "Incorrect credentials")
}

// An invitation is the only way a second account gets created, and the token
// is single-use.
func TestInvitationFlowCreatesOneAccount(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/users/invite", nil)
	require.Less(t, resp.status, 400, resp.body)

	token := app.invitationToken()
	require.NotEmpty(t, token)

	require.Equal(t, http.StatusOK, app.get("/invitation/"+token).status)

	// The confirmation has to match, and eight characters is the floor. The
	// handler deletes the invitation before it reads the form, so a rejected
	// attempt only survives because the transaction is rolled back -- an
	// invitee who mistypes must still be able to try again.
	resp = app.post("/invitation/"+token, url.Values{
		"username":              {"second"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"something else"},
	})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "Passwords do not match")
	require.Equal(t, token, app.invitationToken(), "a rejected attempt must not consume the token")

	resp = app.post("/invitation/"+token, url.Values{
		"username":              {"second"},
		"password":              {"short"},
		"password-confirmation": {"short"},
	})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "at least 8 characters")
	require.Equal(t, token, app.invitationToken())

	resp = app.post("/invitation/"+token, url.Values{
		"username":              {"second"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"hunter2hunter2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.usernames(), "second")

	// Redeemed: the token is gone and the page it served rejects it now.
	require.Empty(t, app.invitationToken())
	require.Equal(t, http.StatusBadRequest, app.get("/invitation/"+token).status)
}

func TestInvitationCanBeRevoked(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/users/invite", nil)
	require.Less(t, resp.status, 400, resp.body)

	id := app.invitationID()
	resp = app.delete("/admin/settings/users/invite/" + strconv.Itoa(id))
	require.Less(t, resp.status, 400, resp.body)
	require.Empty(t, app.invitationToken())
}

func (a *testApp) invitationToken() string {
	a.t.Helper()

	invitations := a.invitations()
	if len(invitations) == 0 {
		return ""
	}

	return invitations[0].Token
}

func (a *testApp) invitationID() int {
	a.t.Helper()

	invitations := a.invitations()
	require.NotEmpty(a.t, invitations)

	return invitations[0].ID
}

func (a *testApp) invitations() []UserInvitation {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	// The far past: every live invitation, whatever the lifetime cutoff is.
	invitations, err := listActiveUserInvitations(tx, time.Time{})
	require.NoError(a.t, err)

	return invitations
}

func (a *testApp) usernames() []string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	users, err := listUsers(tx)
	require.NoError(a.t, err)

	names := []string{}
	for _, u := range users {
		names = append(names, u.Username)
	}

	return names
}
