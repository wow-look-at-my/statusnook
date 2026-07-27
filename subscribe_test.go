package main

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useMailChannel points the instance's alert email at a local SMTP server and
// returns it, so the subscribe and notification paths can be driven end to end.
func useMailChannel(t *testing.T) *fakeSMTP {
	t.Helper()

	server := startFakeSMTP(t)

	details, err := json.Marshal(SMTPNotificationDetails{
		Host: "127.0.0.1",
		Port: smtpPort(t, server.addr()),
		From: "status@example.com",
	})
	require.NoError(t, err)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	require.NoError(t, createNotification(tx, "mail", "Mail", "smtp", string(details)))

	channel, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)
	require.NoError(t, updateAlertSMTPNotificationSetting(tx, channel.ID))
	require.NoError(t, updateAlertSettings(tx, "", "", true))
	require.NoError(t, tx.Commit())

	// The suppression sync is throttled by a package-level timestamp; reset it
	// so each test starts from the same state.
	supressionSyncMu.Lock()
	lastSuppressionSync = time.Now().UTC()
	supressionSyncMu.Unlock()

	return server
}

// TestSubscribeEmailFlow covers the public subscription path: request, the
// confirmation email, confirming, and unsubscribing.
func TestSubscribeEmailFlow(t *testing.T) {
	ts := newTestServer(t)
	mail := useMailChannel(t)

	subscribeLimiter = newRateLimiter(10, time.Hour, time.Hour)

	requested := ts.post("/subscribe/email", url.Values{"email": {"reader@example.com"}})
	require.Equal(t, http.StatusOK, requested.code, requested.body)

	require.True(t, waitFor(t, 5*time.Second, func() bool {
		mail.mu.Lock()
		defer mail.mu.Unlock()

		return strings.Contains(mail.body, "confirm your subscription")
	}), "no confirmation email was sent")

	mail.mu.Lock()
	body := mail.body
	mail.mu.Unlock()

	token := regexp.MustCompile(`token=([^"&\s<]+)`).FindStringSubmatch(body)
	require.Len(t, token, 2, "no confirmation token in %s", body)
	confirmToken := html.UnescapeString(token[1])

	assert.Equal(t, 1, countRows(t,
		"select count(*) from pending_email_alert_subscription where confirmed_at is null"))

	page := ts.get("/subscribe/email/confirm?token=" + confirmToken)
	assert.Equal(t, http.StatusOK, page.code)

	// Confirming redirects to the status page.
	confirmed := ts.post("/subscribe/email/confirm?token="+confirmToken, nil)
	require.Equal(t, http.StatusFound, confirmed.code, confirmed.body)

	assert.Equal(t, 1, countRows(t,
		"select count(*) from alert_subscription where type = 'email' and active = true"))

	// A second request for an address that is already subscribed is answered
	// with the "already subscribed" fragment rather than another email.
	again := ts.post("/subscribe/email", url.Values{"email": {"reader@example.com"}})
	assert.Equal(t, http.StatusOK, again.code)
	assert.Contains(t, again.body, "email-already-subscribed-modal")

	unsubscribeToken := subscriptionMeta(t, "reader@example.com")

	unsubscribePage := ts.get("/unsubscribe?token=" + url.QueryEscape(unsubscribeToken))
	assert.Equal(t, http.StatusOK, unsubscribePage.code)

	unsubscribed := ts.post("/unsubscribe", url.Values{"token": {unsubscribeToken}})
	require.Equal(t, http.StatusOK, unsubscribed.code, unsubscribed.body)
	assert.Equal(t, 0, countRows(t,
		"select count(*) from alert_subscription where active = true"))

	resubscribed := ts.post("/resubscribe", url.Values{"token": {unsubscribeToken}})
	require.Equal(t, http.StatusOK, resubscribed.code, resubscribed.body)
	assert.Equal(t, 1, countRows(t,
		"select count(*) from alert_subscription where active = true"))
}

func TestSubscribeEmailRejectsBadInput(t *testing.T) {
	ts := newTestServer(t)
	useMailChannel(t)

	subscribeLimiter = newRateLimiter(10, time.Hour, time.Hour)

	assert.Equal(t, http.StatusBadRequest,
		ts.post("/subscribe/email", url.Values{"email": {"not-an-email"}}).code)
	assert.Equal(t, http.StatusBadRequest,
		ts.post("/subscribe/email", url.Values{}).code)

	// The confirmation page renders without a token; confirming needs one.
	assert.Equal(t, http.StatusOK, ts.get("/subscribe/email/confirm").code)
	assert.Equal(t, http.StatusBadRequest, ts.post("/subscribe/email/confirm", url.Values{}).code)
	assert.Equal(t, http.StatusBadRequest, ts.post("/unsubscribe", url.Values{}).code)
}

// TestSubscribeEmailRateLimited covers the cap that stops the endpoint being
// used to mail arbitrary addresses.
func TestSubscribeEmailRateLimited(t *testing.T) {
	ts := newTestServer(t)
	useMailChannel(t)

	subscribeLimiter = newRateLimiter(2, time.Hour, time.Hour)
	t.Cleanup(func() { subscribeLimiter = newRateLimiter(10, time.Hour, time.Hour) })

	ts.post("/subscribe/email", url.Values{"email": {"one@example.com"}})
	ts.post("/subscribe/email", url.Values{"email": {"two@example.com"}})

	limited := ts.post("/subscribe/email", url.Values{"email": {"three@example.com"}})
	assert.Equal(t, http.StatusTooManyRequests, limited.code)
	assert.NotEmpty(t, limited.hdr.Get("Retry-After"))
}

// TestNotificationLoopDeliversAlert covers the loop that mails subscribers when
// an alert is posted.
func TestNotificationLoopDeliversAlert(t *testing.T) {
	ts := newTestServer(t)
	mail := useMailChannel(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, createAlertSubscription(tx, "email", "reader@example.com", "unsub-token"))
	require.NoError(t, tx.Commit())

	require.Equal(t, http.StatusOK, ts.post("/admin/services/create", url.Values{
		"name": {"Website"}, "helper-text": {"example.com"},
	}).code)

	created := ts.post("/admin/alerts/create", url.Values{
		"title": {"Outage"}, "type": {"incident"}, "severity": {"red"},
		"message": {"The site is down"}, "services": {"1"},
	})
	require.Equal(t, http.StatusOK, created.code, created.body)

	assert.Positive(t, countRows(t,
		"select count(*) from alert_notification where sent_at is null"))

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go notificationLoop(ctx, &wg)

	require.True(t, waitFor(t, 20*time.Second, func() bool {
		return countRows(t, "select count(*) from alert_notification where sent_at is null") == 0
	}), "the notification was never marked sent")

	cancel()
	wg.Wait()

	mail.mu.Lock()
	defer mail.mu.Unlock()
	assert.Contains(t, mail.body, "The site is down")
	assert.Contains(t, strings.Join(mail.conv, "\n"), "RCPT TO:<reader@example.com>")
}

// TestNotificationChannelLifecycle covers creating, editing, viewing and
// deleting both channel types through the forms.
func TestNotificationChannelLifecycle(t *testing.T) {
	ts := newTestServer(t)

	slackCalls := make(chan string, 4)
	slackWebhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackCalls <- r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer slackWebhook.Close()

	mail := startFakeSMTP(t)

	smtpCreated := ts.post("/admin/notifications/create", url.Values{
		"type": {"smtp"}, "display-name": {"Mail"}, "host": {"127.0.0.1"},
		"port":     {strconv.Itoa(smtpPort(t, mail.addr()))},
		"username": {"mailer"}, "password": {"secret"}, "from": {"status@example.com"},
	})
	require.Equal(t, http.StatusOK, smtpCreated.code, smtpCreated.body)

	slackCreated := ts.post("/admin/notifications/create", url.Values{
		"type": {"slack"}, "display-name": {"Chat"}, "webhook-url": {slackWebhook.URL + "/hook"},
	})
	require.Equal(t, http.StatusOK, slackCreated.code, slackCreated.body)

	list := ts.get("/admin/notifications")
	assert.Contains(t, list.body, "Mail")
	assert.Contains(t, list.body, "Chat")

	assert.Equal(t, http.StatusOK, ts.get("/admin/notifications/1/edit").code)
	assert.Equal(t, http.StatusOK, ts.get("/admin/notifications/2/view").code)

	edited := ts.post("/admin/notifications/1/edit", url.Values{
		"type": {"smtp"}, "display-name": {"Mail renamed"}, "host": {"127.0.0.1"},
		"port":     {strconv.Itoa(smtpPort(t, mail.addr()))},
		"username": {"mailer"}, "password": {"secret"}, "from": {"status@example.com"},
	})
	require.Equal(t, http.StatusOK, edited.code, edited.body)
	assert.Contains(t, ts.get("/admin/notifications").body, "Mail renamed")

	// The alert email channel is picked on the notification settings page.
	settings := ts.post("/admin/alerts/notifications", url.Values{
		"email-notification-channel": {"1"}, "managed-subscriptions": {"on"},
	})
	require.Equal(t, http.StatusOK, settings.code, settings.body)

	assert.Equal(t, http.StatusOK,
		ts.send(http.MethodDelete, "/admin/notifications/2", nil).code)
	assert.NotContains(t, ts.get("/admin/notifications").body, "Chat")
}

// TestUpdateCheck covers the update banner against a stand-in release feed.
func TestUpdateCheck(t *testing.T) {
	ts := newTestServer(t)

	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v99.0.0",
			"body":         "A much newer release",
			"published_at": time.Now().UTC().Format(time.RFC3339),
			"assets": []map[string]string{
				{"name": "statusnook_linux_amd64_v99.0.0", "url": "http://example.invalid/asset"},
			},
		})
	}))
	defer releases.Close()

	prev := releaseAPIURL
	releaseAPIURL = releases.URL
	t.Cleanup(func() { releaseAPIURL = prev })

	check := ts.get("/admin/update/check")
	require.Equal(t, http.StatusOK, check.code)
	assert.Contains(t, check.body, "Updates are available")
	assert.Contains(t, check.body, "v99.0.0")

	// Self-update is disabled under Docker and by the environment, and the
	// endpoint must refuse rather than replace the binary.
	prevDisabled := env.SelfUpdateDisabled
	env.SelfUpdateDisabled = true
	t.Cleanup(func() { env.SelfUpdateDisabled = prevDisabled })

	refused := ts.post("/admin/update", nil)
	assert.Equal(t, http.StatusBadRequest, refused.code)
	assert.Contains(t, refused.body, "Self-update is disabled")
}

// TestSlackOAuthCallback covers the Slack install callback, which turns an OAuth
// code into a webhook subscription.
func TestSlackOAuthCallback(t *testing.T) {
	ts := newTestServer(t)

	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "the-code", r.PostFormValue("code"))

		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"incoming_webhook": map[string]string{
				"url":     "https://hooks.slack.example/T/B/C",
				"channel": "#status",
			},
		})
	}))
	defer slack.Close()

	prev := slackTokenURL
	slackTokenURL = slack.URL
	t.Cleanup(func() { slackTokenURL = prev })

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, updateAlertSettings(
		tx, "https://slack.com/oauth/v2/authorize?client_id=123", "client-secret", true,
	))
	require.NoError(t, tx.Commit())

	resp := ts.get("/callback/slack?code=the-code")
	assert.Less(t, resp.code, 500, resp.body)

	assert.Equal(t, http.StatusBadRequest, ts.get("/callback/slack").code)
}

// subscriptionMeta returns the unsubscribe token stored for an address.
func subscriptionMeta(t *testing.T, email string) string {
	t.Helper()

	meta := ""
	require.NoError(t, db.QueryRow(
		"select meta from alert_subscription where destination = ?", email,
	).Scan(&meta))

	return meta
}
