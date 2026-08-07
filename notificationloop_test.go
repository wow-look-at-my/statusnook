package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Ticks the queue every 50ms instead of every ten seconds. go-toolchain gives a
// single test thirty seconds, and a ten-second tick plus setup leaves too
// little of it.
func withFastNotificationLoop(t *testing.T) {
	t.Helper()

	previous := notificationLoopInterval
	notificationLoopInterval = 50 * time.Millisecond
	t.Cleanup(func() { notificationLoopInterval = previous })
}

// The queue drains through notificationLoop: one row per subscriber per alert
// message, delivered and then stamped. A slack subscriber is the half that can
// be driven locally -- an email one needs a real SMTP server.
func TestNotificationLoopDeliversToASlackSubscriber(t *testing.T) {
	app := withTestApp(t)
	withFastNotificationLoop(t)
	app.useSMTPChannelForAlerts()

	posted := make(chan string, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		select {
		case posted <- string(body):
		default:
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	serviceID := app.createService("Web", "the site")
	app.subscribeSlack(webhook.URL)
	alertID := app.createAlert(`Outage "quoted"`, serviceID, "we are looking")

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go notificationLoop(ctx, &wg)
	defer func() {
		cancel()
		wg.Wait()
	}()

	var body string
	select {
	case body = <-posted:
	case <-time.After(10 * time.Second):
		t.Fatal("the slack subscriber was never called")
	}

	// The template is text/template, so the title is escaped by hand; a quote
	// in an alert title used to produce invalid JSON.
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &payload),
		"the webhook body must be valid JSON: %s", body)
	require.Contains(t, body, "we are looking")

	// Stamped, so the next tick does not deliver it twice.
	require.Eventually(t, func() bool { return len(app.unsentNotifications()) == 0 },
		10*time.Second, 100*time.Millisecond, "sent_at was never stamped")

	require.NotZero(t, alertID)
}

// A webhook that answers anything but 200 must leave sent_at null so the next
// tick retries -- stamping it would drop the alert with no record anywhere.
func TestNotificationLoopRetriesAfterARejectedDelivery(t *testing.T) {
	app := withTestApp(t)
	withFastNotificationLoop(t)
	app.useSMTPChannelForAlerts()

	calls := make(chan struct{}, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case calls <- struct{}{}:
		default:
		}

		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("no_service"))
	}))
	defer webhook.Close()

	serviceID := app.createService("Web", "the site")
	app.subscribeSlack(webhook.URL)
	app.createAlert("Outage", serviceID, "we are looking")

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go notificationLoop(ctx, &wg)
	defer func() {
		cancel()
		wg.Wait()
	}()

	select {
	case <-calls:
	case <-time.After(10 * time.Second):
		t.Fatal("the slack subscriber was never called")
	}

	require.NotEmpty(t, app.unsentNotifications(), "a rejected delivery stays queued")
}

func (a *testApp) subscribeSlack(destination string) {
	a.t.Helper()

	tx, err := rwDB.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	require.NoError(a.t, createAlertSubscription(tx, "slack", destination, "T"+strconv.Itoa(1)))
	require.NoError(a.t, tx.Commit())
}

func (a *testApp) unsentNotifications() []UnsentAlertNotification {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	unsent, err := listUnsentAlertNotifications(tx)
	require.NoError(a.t, err)

	return unsent
}
