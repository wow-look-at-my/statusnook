package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Confirming a pending subscription is the step that turns an address into a
// live subscriber, and it is the one half of the flow that sends no mail.
func TestSubscribeEmailConfirm(t *testing.T) {
	app := withTestApp(t)
	app.useSMTPChannelForAlerts()

	require.Equal(t, http.StatusOK, app.get("/subscribe/email/confirm").status)
	require.Equal(t, http.StatusBadRequest, app.post("/subscribe/email/confirm", nil).status)

	// A token nobody issued cannot resolve to an address.
	require.Equal(t, http.StatusInternalServerError,
		app.post("/subscribe/email/confirm?token=made-up", nil).status)

	app.addPendingSubscription("sub@example.com", "tok")

	resp := app.post("/subscribe/email/confirm?token=tok", nil)
	require.Equal(t, http.StatusFound, resp.status, resp.body)
	require.Equal(t, "/?email_subscribed=1", resp.header.Get("Location"))

	sub := app.subscription("sub@example.com")
	require.True(t, sub.Active)
	require.NotEmpty(t, sub.Meta, "the unsubscribe token is minted here")

	// The pending row is retired, so a replayed confirmation finds nothing.
	require.Equal(t, http.StatusInternalServerError,
		app.post("/subscribe/email/confirm?token=tok", nil).status)
}

// Unsubscribe and resubscribe are driven by the token embedded in every alert
// email, and both are reachable without a session.
func TestUnsubscribeAndResubscribeByToken(t *testing.T) {
	app := withTestApp(t)
	app.useSMTPChannelForAlerts()

	app.addPendingSubscription("sub@example.com", "tok")
	require.Equal(t, http.StatusFound, app.post("/subscribe/email/confirm?token=tok", nil).status)

	token := app.subscription("sub@example.com").Meta
	require.NotEmpty(t, token)

	require.Equal(t, http.StatusOK, app.get("/unsubscribe?token="+url.QueryEscape(token)).status)
	require.Equal(t, http.StatusBadRequest, app.get("/unsubscribe").status)

	resp := app.post("/unsubscribe", url.Values{"token": {token}})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.False(t, app.subscription("sub@example.com").Active)

	resp = app.post("/resubscribe", url.Values{"token": {token}})
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.True(t, app.subscription("sub@example.com").Active)

	require.Equal(t, http.StatusBadRequest, app.post("/unsubscribe", url.Values{}).status)
	require.Equal(t, http.StatusBadRequest, app.post("/resubscribe", url.Values{}).status)
}

// The subscribe form sends a confirmation email, so only its rejections are
// reachable without a live SMTP server.
func TestSubscribeEmailRejectsAnAddressThatIsNotOne(t *testing.T) {
	app := withTestApp(t)

	for _, address := range []string{"", "not an address", "@example.com", "a@"} {
		resp := app.post("/subscribe/email", url.Values{"email": {address}})
		require.Equal(t, http.StatusBadRequest, resp.status, "address %q", address)
	}
}

func (a *testApp) useSMTPChannelForAlerts() {
	a.t.Helper()

	channelID := a.createSMTPChannel("Mail")

	resp := a.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		// Managed subscriptions keeps the flow off the Postmark suppression
		// API, which is the only other network call on this path.
		"managed-subscriptions": {"on"},
	})
	require.Less(a.t, resp.status, 400, resp.body)
}

func (a *testApp) addPendingSubscription(email string, token string) {
	a.t.Helper()

	tx, err := rwDB.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	require.NoError(a.t, createPendingEmailAlertSubscription(tx, token, email, time.Now().UTC()))
	require.NoError(a.t, tx.Commit())
}

func (a *testApp) subscription(email string) AlertSubscription {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	sub, err := getAlertSubscriptionByEmail(tx, email)
	require.NoError(a.t, err)

	return sub
}
