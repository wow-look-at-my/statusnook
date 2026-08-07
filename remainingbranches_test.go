package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Decryption is the half of the secrets tool that can be handed something it
// cannot use, and every shape of that has to come back as a rejection rather
// than as a page reporting success with nothing in it.
func TestSecretsToolRejectsCiphertextItCannotRead(t *testing.T) {
	app := withTestApp(t)

	ciphertext := app.encryptSecret("hunter2")

	for _, input := range []string{
		"no-dot-in-it",
		"!!!not base64!!!.AAAAAAAAAAAAAAAA",
		"AAAA.!!!not base64!!!",
		"AAAA.AAAAAAAAAAAAAAAA",
		ciphertext + "tampered",
	} {
		resp := app.post("/admin/settings/secrets",
			url.Values{"action": {"decrypt"}, "input": {input}})
		require.GreaterOrEqual(t, resp.status, 400, "decrypted %q", input)
	}

	// The real one still works, so none of the above left the tool broken.
	resp := app.post("/admin/settings/secrets",
		url.Values{"action": {"decrypt"}, "input": {ciphertext}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, resp.body, "hunter2")
}

// An invitation creates an account, so it carries the same uniqueness rule the
// user table does -- and a rejected attempt must leave the token spendable.
func TestInvitationRejectsATakenUsername(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/users/invite", nil)
	require.Less(t, resp.status, 400, resp.body)

	token := app.invitationToken()
	require.NotEmpty(t, token)

	resp = app.post("/invitation/"+token, url.Values{
		"username":              {"admin"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"hunter2hunter2"},
	})
	require.GreaterOrEqual(t, resp.status, 400, "a taken username was accepted")
	require.Equal(t, token, app.invitationToken(),
		"the invitation must survive an attempt that created nothing")

	resp = app.post("/invitation/"+token, url.Values{
		"username":              {"second"},
		"password":              {"hunter2hunter2"},
		"password-confirmation": {"hunter2hunter2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.usernames(), "second")
}

// Renaming a user onto a name someone else holds is the same collision from
// the settings side.
func TestUserEditRejectsATakenUsername(t *testing.T) {
	app := withTestApp(t)

	id := strconv.Itoa(app.createSecondUser("second"))

	resp := app.post("/admin/settings/users/"+id+"/edit",
		url.Values{"username": {"admin"}, "password": {"retain"}})
	require.GreaterOrEqual(t, resp.status, 400, "a taken username was accepted")
	require.Contains(t, app.usernames(), "second", "the rename must not have happened")

	// Deleting a user drops their sessions with them, and deleting again is a
	// no-op rather than an error -- the row is already gone either way.
	require.Less(t, app.delete("/admin/settings/users/"+id).status, 400)
	require.NotContains(t, app.usernames(), "second")
	require.Less(t, app.delete("/admin/settings/users/"+id).status, 400)
}

// A slack channel and an SMTP channel keep different details, and switching an
// existing channel between them is not something the edit form offers -- what
// it does offer has to round-trip.
func TestNotificationEditKeepsTheChannelType(t *testing.T) {
	app := withTestApp(t)

	slackID := strconv.Itoa(app.createSlackChannel("Chat", "https://hooks.example.com/x"))

	resp := app.post("/admin/notifications/"+slackID+"/edit", url.Values{
		"display-name": {"Chat"},
		"webhook-url":  {"https://hooks.example.com/y"},
	})
	require.Less(t, resp.status, 400, resp.body)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	id, err := strconv.Atoi(slackID)
	require.NoError(t, err)

	channel, err := getNotificationChannelByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "slack", channel.Type)

	details, ok := channel.Details.(SlackNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "https://hooks.example.com/y", details.WebhookURL)

	// An id that is not a number, and one that is not a channel.
	require.Equal(t, http.StatusBadRequest, app.get("/admin/notifications/abc/edit").status)
	require.GreaterOrEqual(t, app.get("/admin/notifications/99999/edit").status, 400)
	require.Equal(t, http.StatusBadRequest, app.delete("/admin/notifications/abc").status)
}

// Ids that are not numbers reach every route with one in the path, and each has
// to reject rather than fall through to a zero id that matches no row.
func TestRoutesRejectANonNumericID(t *testing.T) {
	app := withTestApp(t)

	for _, path := range []string{
		"/admin/alerts/abc",
		"/admin/alerts/abc/edit",
		"/admin/alerts/abc/messages",
		"/admin/alerts/1/messages/abc",
		"/admin/monitors/abc",
		"/admin/monitors/abc/edit",
		"/admin/monitors/abc/view",
		"/admin/services/abc/edit",
		"/admin/notifications/abc/view",
		"/admin/notifications/mail-groups/abc/edit",
		"/admin/notifications/mail-groups/abc/view",
		"/admin/settings/users/abc/edit",
	} {
		require.Equal(t, http.StatusBadRequest, app.get(path).status, "GET %s", path)
	}

	for _, path := range []string{
		"/admin/alerts/abc",
		"/admin/monitors/abc",
		"/admin/services/abc",
		"/admin/notifications/abc",
		"/admin/notifications/mail-groups/abc",
		"/admin/settings/users/abc",
		"/admin/settings/users/invite/abc",
	} {
		require.Equal(t, http.StatusBadRequest, app.delete(path).status, "DELETE %s", path)
	}

	for _, path := range []string{
		"/admin/alerts/abc/resolve",
		"/admin/alerts/abc/unresolve",
		"/admin/alerts/abc/messages",
		"/admin/monitors/abc/edit",
		"/admin/services/abc/edit",
		"/admin/notifications/abc/edit",
		"/admin/notifications/mail-groups/abc/edit",
		"/admin/settings/users/abc/edit",
	} {
		require.Equal(t, http.StatusBadRequest,
			app.post(path, url.Values{"name": {"x"}, "message": {"x"}, "username": {"x"},
				"password": {"retain"}, "display-name": {"x"}}).status, "POST %s", path)
	}
}
