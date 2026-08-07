package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Points the instance's alert channel at a local SMTP server, so the paths that
// actually send mail can run.
func (a *testApp) useFakeSMTPForAlerts(smtp *fakeSMTP, managedSubscriptions bool) int {
	a.t.Helper()

	before := a.channelIDs()
	resp := a.post("/admin/notifications/create", url.Values{
		"type":         {"smtp"},
		"display-name": {"Mail"},
		"host":         {"localhost"},
		"port":         {strconv.Itoa(smtp.port)},
		"username":     {"statusnook"},
		"password":     {"shh"},
		"from":         {"status@example.com"},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	channelID := onlyNewID(a.t, before, a.channelIDs())

	form := url.Values{"smtp-notification-channel": {strconv.Itoa(channelID)}}
	if managedSubscriptions {
		form.Set("managed-subscriptions", "on")
	}

	resp = a.post("/admin/alerts/notifications", form)
	require.Less(a.t, resp.status, 400, resp.body)

	return channelID
}

// The public subscribe form: it records a pending subscription and mails the
// confirmation link. Nothing is subscribed until that link is followed.
func TestSubscribeEmailSendsAConfirmation(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	app.useFakeSMTPForAlerts(smtp, true)

	resp := app.post("/subscribe/email", url.Values{"email": {"sub@example.com"}})
	require.Less(t, resp.status, 400, resp.body)

	require.Eventually(t, func() bool { return len(smtp.messages()) > 0 },
		10*time.Second, 20*time.Millisecond, "no confirmation email was sent")

	sent := smtp.messages()[0]
	require.Equal(t, "status@example.com", sent.from)
	require.Equal(t, []string{"sub@example.com"}, sent.to)
	require.Contains(t, sent.body, "/subscribe/email/confirm?token=")

	// Nothing is live yet: the address is only pending.
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	_, err = getAlertSubscriptionByEmail(tx, "sub@example.com")
	require.Error(t, err, "the address must not be subscribed before it is confirmed")

	// A second request inside the ten-minute window renders the same "check
	// your inbox" page without sending again, so the form cannot be used to
	// mail somebody repeatedly -- and it says nothing different either way,
	// which is what stops it being an address oracle.
	require.NoError(t, tx.Rollback())
	resp = app.post("/subscribe/email", url.Values{"email": {"sub@example.com"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Never(t, func() bool { return len(smtp.messages()) > 1 },
		time.Second, 50*time.Millisecond, "a second confirmation was sent")
}

// A monitor going down mails the mail groups attached to it, and the message
// carries what the person reading it needs.
func TestMonitorAlertEmailReachesTheMailGroup(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	channelID := app.useFakeSMTPForAlerts(smtp, true)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	groupID := app.createMailGroup("Ops", "ops@example.com")

	before := app.monitorIDs()
	resp := app.post("/admin/monitors/create", url.Values{
		"name":                  {"Down"},
		"url":                   {target.URL},
		"method":                {"GET"},
		"frequency":             {"10"},
		"timeout":               {"5"},
		"attempts":              {"1"},
		"notification-channels": {strconv.Itoa(channelID)},
		"mail-groups":           {strconv.Itoa(groupID)},
	})
	require.Less(t, resp.status, 400, resp.body)
	id := onlyNewID(t, before, app.monitorIDs())

	runMonitorLoopUntil(t, func() bool { return len(smtp.messages()) > 0 })

	sent := smtp.messages()[0]
	require.Equal(t, []string{"ops@example.com"}, sent.to)
	require.Contains(t, sent.body, "Down", "the monitor's name has to be in the subject or body")

	require.NotEmpty(t, app.monitorLogs(id))
}

// The alert queue's email half: one row per subscriber, delivered and stamped.
func TestNotificationLoopMailsAnEmailSubscriber(t *testing.T) {
	app := withTestApp(t)
	withFastNotificationLoop(t)
	smtp := newFakeSMTP(t)
	app.useFakeSMTPForAlerts(smtp, true)

	app.addPendingSubscription("sub@example.com", "tok")
	require.Equal(t, http.StatusFound, app.post("/subscribe/email/confirm?token=tok", nil).status)

	serviceID := app.createService("Web", "the site")
	app.createAlert("Outage", serviceID, "we are looking")

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go notificationLoop(ctx, &wg)
	defer func() {
		cancel()
		wg.Wait()
	}()

	require.Eventually(t, func() bool { return len(smtp.messages()) > 0 },
		10*time.Second, 100*time.Millisecond, "the subscriber was never mailed")

	sent := smtp.messages()[0]
	require.Equal(t, []string{"sub@example.com"}, sent.to)
	require.Contains(t, sent.body, "we are looking")
	require.Contains(t, sent.body, "/unsubscribe?token=",
		"every alert email carries the unsubscribe link the token is minted for")

	require.Eventually(t, func() bool { return len(app.unsentNotifications()) == 0 },
		10*time.Second, 100*time.Millisecond, "sent_at was never stamped")
}

// Whatever headers the channel carries go on every message it sends -- the
// confirmation the subscribe form mails, and the alert the queue mails.
func TestChannelHeadersRideEveryMessage(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)

	before := app.channelIDs()
	require.Less(t, app.post("/admin/notifications/create", url.Values{
		"type": {"smtp"}, "display-name": {"Mail"},
		"host": {"localhost"}, "port": {strconv.Itoa(smtp.port)},
		"username": {"statusnook"}, "password": {"shh"},
		"from":       {"status@example.com"},
		"header-key": {"X-Mailer"}, "header-value": {"statusnook"},
	}).status, 400)

	channelID := onlyNewID(t, before, app.channelIDs())
	require.Less(t, app.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		"managed-subscriptions":     {"on"},
	}).status, 400)

	require.Less(t, app.post("/subscribe/email",
		url.Values{"email": {"sub@example.com"}}).status, 400)

	require.Eventually(t, func() bool { return len(smtp.messages()) > 0 },
		10*time.Second, 20*time.Millisecond, "no confirmation email was sent")
	require.Contains(t, smtp.messages()[0].body, "X-Mailer: statusnook")

	app.addPendingSubscription("live@example.com", "tok")
	require.Equal(t, http.StatusFound, app.post("/subscribe/email/confirm?token=tok", nil).status)

	app.createAlert("Outage", app.createService("Web", "the site"), "we are looking")
	drainNotificationQueue()

	require.Eventually(t, func() bool { return len(smtp.messages()) > 1 },
		10*time.Second, 20*time.Millisecond, "the alert was never mailed")
	require.Contains(t, smtp.messages()[len(smtp.messages())-1].body, "X-Mailer: statusnook")
}
