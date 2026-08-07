package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The transition rules, which decide when anyone is told anything. A monitor
// alerts when it goes down and again when it comes back, and stays quiet while
// nothing has changed -- an alert per failed check would make an outage
// unreadable.
func TestMonitorLoopAlertsOnTransitionsOnly(t *testing.T) {
	app := withTestApp(t)

	down := atomic.Bool{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	posts := atomic.Int64{}
	bodies := make(chan string, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)

		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		select {
		case bodies <- string(buf[:n]):
		default:
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	channelID := app.createSlackChannel("Chat", webhook.URL)

	before := app.monitorIDs()
	resp := app.post("/admin/monitors/create", url.Values{
		"name":                  {"Flaky"},
		"url":                   {target.URL},
		"method":                {"GET"},
		"frequency":             {"10"},
		"timeout":               {"5"},
		"attempts":              {"1"},
		"notification-channels": {strconv.Itoa(channelID)},
	})
	require.Less(t, resp.status, 400, resp.body)
	id := onlyNewID(t, before, app.monitorIDs())

	// A first check that succeeds is not a transition, so nobody is told.
	runMonitorLoopUntil(t, func() bool { return len(app.monitorLogs(id)) > 0 })
	require.Zero(t, posts.Load(), "a healthy first check must not alert")

	// Down: that is a transition. A fresh loop rechecks immediately, and the
	// happy/unhappy comparison is read from the database rather than from the
	// loop's own memory, so restarting it does not lose the transition.
	down.Store(true)
	runMonitorLoopUntil(t, func() bool { return app.lastResult(id) == "error" })

	require.Eventually(t, func() bool { return posts.Load() >= 1 },
		10*time.Second, 20*time.Millisecond, "going down must alert")

	require.Contains(t, <-bodies, "Flaky")
	sent := posts.Load()

	// Back up: the other transition, and the only one that reports a recovery.
	down.Store(false)
	runMonitorLoopUntil(t, func() bool { return app.lastResult(id) == "success" })

	require.Eventually(t, func() bool { return posts.Load() > sent },
		10*time.Second, 20*time.Millisecond, "coming back up must alert")
}

// A monitor added for an endpoint that is already down has no healthy state to
// transition from, and used to stay silent until it recovered -- so the team
// first heard about an outage when it ended.
func TestMonitorLoopAlertsOnAFirstCheckThatFails(t *testing.T) {
	app := withTestApp(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer target.Close()

	posted := make(chan struct{}, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case posted <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	channelID := app.createSlackChannel("Chat", webhook.URL)

	before := app.monitorIDs()
	resp := app.post("/admin/monitors/create", url.Values{
		"name":                  {"Already down"},
		"url":                   {target.URL},
		"method":                {"GET"},
		"frequency":             {"10"},
		"timeout":               {"5"},
		"attempts":              {"1"},
		"notification-channels": {strconv.Itoa(channelID)},
	})
	require.Less(t, resp.status, 400, resp.body)
	id := onlyNewID(t, before, app.monitorIDs())

	runMonitorLoopUntil(t, func() bool { return len(app.monitorLogs(id)) > 0 })

	select {
	case <-posted:
	case <-time.After(10 * time.Second):
		t.Fatal("a first check that fails must alert")
	}
}

// A target that never answers is a timeout, which reads differently from an
// error status: whoever is debugging needs to know nothing came back at all.
func TestMonitorLoopRecordsATimeout(t *testing.T) {
	app := withTestApp(t)

	block := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer target.Close()
	defer close(block)

	before := app.monitorIDs()
	resp := app.post("/admin/monitors/create", url.Values{
		"name":      {"Hangs"},
		"url":       {target.URL},
		"method":    {"GET"},
		"frequency": {"10"},
		"timeout":   {"5"},
		"attempts":  {"1"},
	})
	require.Less(t, resp.status, 400, resp.body)
	id := onlyNewID(t, before, app.monitorIDs())

	runMonitorLoopUntil(t, func() bool { return len(app.monitorLogs(id)) > 0 })

	logs := app.monitorLogs(id)
	require.Equal(t, "timeout", logs[0].Result)
	require.False(t, logs[0].ResponseCode.Valid, "nothing answered, so there is no status")
}

func (a *testApp) lastResult(monitorID int) string {
	a.t.Helper()

	logs := a.monitorLogs(monitorID)
	if len(logs) == 0 {
		return ""
	}

	return logs[0].Result
}
