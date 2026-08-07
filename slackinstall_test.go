package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Slack's install flow ends here: Slack redirects back with a code, statusnook
// trades it for an incoming webhook, and that webhook becomes an alert
// subscription. Nothing else creates a slack subscriber.
func serveSlackAccess(t *testing.T, status int, body any) *int64 {
	t.Helper()

	calls := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++

		require.NoError(t, r.ParseForm())
		require.NotEmpty(t, r.PostForm.Get("code"))
		require.Equal(t, "abc123", r.PostForm.Get("client_id"),
			"the client id comes out of the stored install URL's query")
		require.Equal(t, "shh", r.PostForm.Get("client_secret"))

		w.WriteHeader(status)
		if raw, ok := body.(string); ok {
			w.Write([]byte(raw))
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)

	previous := slackAPIBaseURL
	slackAPIBaseURL = server.URL
	t.Cleanup(func() { slackAPIBaseURL = previous })

	return &calls
}

func (a *testApp) configureSlackApp() {
	a.t.Helper()

	channelID := a.createSMTPChannel("Mail")

	resp := a.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		"slack-install-url":         {"https://slack.example.com/install?client_id=abc123"},
		"slack-client-secret":       {"shh"},
	})
	require.Less(a.t, resp.status, 400, resp.body)
}

func TestSlackInstallCreatesASubscription(t *testing.T) {
	app := withTestApp(t)
	app.configureSlackApp()

	serveSlackAccess(t, http.StatusOK, map[string]any{
		"ok":   true,
		"team": map[string]string{"id": "T123"},
		"incoming_webhook": map[string]string{
			"url":        "https://hooks.slack.example.com/T123/C456",
			"channel_id": "C456",
		},
	})

	resp := app.get("/callback/slack?code=oauthcode")
	require.Equal(t, http.StatusFound, resp.status, resp.body)
	require.Equal(t, "/?slack_app_installed=1", resp.header.Get("Location"))

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	subs, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, subs)

	var meta string
	require.NoError(t, tx.QueryRow(
		`select meta from alert_subscription where type = 'slack'`,
	).Scan(&meta))
	require.Equal(t, "T123_C456", meta, "the team and channel are the identity of the install")

	var destination string
	require.NoError(t, tx.QueryRow(
		`select destination from alert_subscription where type = 'slack'`,
	).Scan(&destination))
	require.Equal(t, "https://hooks.slack.example.com/T123/C456", destination)
}

// Re-installing into the same channel replaces the subscription rather than
// adding a second one, or every alert would go out twice.
func TestSlackInstallIsIdempotentPerChannel(t *testing.T) {
	app := withTestApp(t)
	app.configureSlackApp()

	response := map[string]any{
		"ok":   true,
		"team": map[string]string{"id": "T123"},
		"incoming_webhook": map[string]string{
			"url":        "https://hooks.slack.example.com/T123/C456",
			"channel_id": "C456",
		},
	}

	serveSlackAccess(t, http.StatusOK, response)
	require.Equal(t, http.StatusFound, app.get("/callback/slack?code=one").status)
	require.Equal(t, http.StatusFound, app.get("/callback/slack?code=two").status)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	var count int
	require.NoError(t, tx.QueryRow(
		`select count(*) from alert_subscription where type = 'slack'`,
	).Scan(&count))
	require.Equal(t, 1, count)
}

func TestSlackInstallRejectsEveryFailure(t *testing.T) {
	app := withTestApp(t)
	app.configureSlackApp()

	// No code at all: Slack did not send the visitor here.
	require.Equal(t, http.StatusBadRequest, app.get("/callback/slack").status)

	serveSlackAccess(t, http.StatusUnauthorized, map[string]any{"ok": false})
	require.Equal(t, http.StatusInternalServerError, app.get("/callback/slack?code=x").status)

	serveSlackAccess(t, http.StatusOK, "not json")
	require.Equal(t, http.StatusInternalServerError, app.get("/callback/slack?code=x").status)

	// Slack reports its own failures with a 200 and ok:false.
	serveSlackAccess(t, http.StatusOK, map[string]any{"ok": false, "error": "invalid_code"})
	require.Equal(t, http.StatusInternalServerError, app.get("/callback/slack?code=x").status)

	previous := slackAPIBaseURL
	slackAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { slackAPIBaseURL = previous })
	require.Equal(t, http.StatusInternalServerError, app.get("/callback/slack?code=x").status)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	var count int
	require.NoError(t, tx.QueryRow(
		`select count(*) from alert_subscription where type = 'slack'`,
	).Scan(&count))
	require.Zero(t, count, "no failure may leave a subscription behind")
}

// Without a stored install URL there is no client id to trade the code with,
// so the callback cannot be completed and must say so rather than calling
// Slack with an empty one.
func TestSlackCallbackNeedsTheAppConfigured(t *testing.T) {
	app := withTestApp(t)

	require.Equal(t, http.StatusInternalServerError, app.get("/callback/slack?code=x").status)
}

// The callback creates the subscription in one transaction, replacing whatever
// the same team and channel had before, so a failure partway must not leave the
// old one deleted and no new one in its place.
func TestSlackCallbackSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	app.configureSlackApp()

	serveSlackAccess(t, http.StatusOK, map[string]any{
		"ok":   true,
		"team": map[string]string{"id": "T123"},
		"incoming_webhook": map[string]string{
			"url":        "https://hooks.slack.example.com/T123/C456",
			"channel_id": "C456",
		},
	})

	withFaultyDB(app)

	for depth := int64(1); depth <= 30; depth++ {
		failAtStatement(depth)
		resp := app.get("/callback/slack?code=oauthcode")
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"the callback answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusFound, app.get("/callback/slack?code=oauthcode").status)
}
