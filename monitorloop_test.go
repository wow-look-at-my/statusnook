package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Runs the real monitor loop against a local target until it has written a log
// row, then stops it. The loop ticks twice a second and treats a monitor it has
// never checked as due immediately, so this settles well inside the timeout.
func runMonitorLoopUntil(t *testing.T, done func() bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}

	wg.Add(1)
	go monitorLoop(ctx, &wg)

	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatal("monitor loop never produced the expected state")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	wg.Wait()
}

func TestMonitorLoopLogsASuccessfulCheck(t *testing.T) {
	app := withTestApp(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "bytes=0-0", r.Header.Get("Range"), "request headers must be sent")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	id := app.createMonitorWithHeaders("Up", target.URL, "Range", "bytes=0-0")

	runMonitorLoopUntil(t, func() bool { return len(app.monitorLogs(id)) > 0 })

	logs := app.monitorLogs(id)
	require.Equal(t, "success", logs[0].Result)
	require.True(t, logs[0].ResponseCode.Valid)
	require.Equal(t, int64(200), logs[0].ResponseCode.Int64)

	// The last-checked row is what the dashboard and the transition logic read.
	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	last, err := getMonitorLogLastChecked(tx, id)
	require.NoError(t, err)
	require.Equal(t, id, last.ID)
}

// A failing check is the interesting one: it retries up to `attempts`, records
// the failure, and fires the monitor's notification channels once.
func TestMonitorLoopRetriesAndNotifiesOnFailure(t *testing.T) {
	app := withTestApp(t)

	hits := atomic.Int64{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	notified := make(chan struct{}, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case notified <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	channelID := app.createSlackChannel("Chat", webhook.URL)
	id := app.createMonitorWithChannel("Down", target.URL, channelID)

	runMonitorLoopUntil(t, func() bool { return len(app.monitorLogs(id)) > 0 })

	logs := app.monitorLogs(id)
	require.Equal(t, "error", logs[0].Result)
	require.Equal(t, int64(500), logs[0].ResponseCode.Int64)
	require.GreaterOrEqual(t, hits.Load(), int64(2), "a 500 must be retried, not trusted once")

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor's notification channel was never called")
	}
}

func (a *testApp) createMonitorWithHeaders(
	name string,
	monitorURL string,
	headerKey string,
	headerValue string,
) int {
	a.t.Helper()

	before := a.monitorIDs()
	resp := a.post("/admin/monitors/create", url.Values{
		"name":         {name},
		"url":          {monitorURL},
		"method":       {"GET"},
		"frequency":    {"10"},
		"timeout":      {"5"},
		"attempts":     {"2"},
		"header-key":   {headerKey},
		"header-value": {headerValue},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.monitorIDs())
}

func (a *testApp) createMonitorWithChannel(name string, monitorURL string, channelID int) int {
	a.t.Helper()

	before := a.monitorIDs()
	resp := a.post("/admin/monitors/create", url.Values{
		"name":                  {name},
		"url":                   {monitorURL},
		"method":                {"GET"},
		"frequency":             {"10"},
		"timeout":               {"5"},
		"attempts":              {"2"},
		"notification-channels": {strconv.Itoa(channelID)},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.monitorIDs())
}

func (a *testApp) monitorLogs(id int) []MonitorLog {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	logs, err := listMonitorLogs(tx, id, 0, 0, 0, time.Now().UTC().Truncate(24*time.Hour))
	require.NoError(a.t, err)

	return logs
}
