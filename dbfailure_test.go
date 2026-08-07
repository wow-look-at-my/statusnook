package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every handler opens a transaction first and has a branch for that failing.
// Nothing else in the suite reaches those branches, and the thing they have to
// guarantee is worth stating: a database that has gone away produces a 5xx and
// a log line, never a panic, a 200 with half a page, or a write that lands
// anyway. Closing the pool is the cheapest way to make every one of them fire
// at once.
func TestHandlersFailCleanlyWhenTheDatabaseIsGone(t *testing.T) {
	app := withTestApp(t)

	serviceID := app.createService("Web", "the site")
	monitorID := app.createMonitor("API", "https://example.com/health")
	channelID := app.createSlackChannel("Chat", "https://hooks.example.com/x")
	groupID := app.createMailGroup("Ops", "a@example.com")
	alertID := app.createAlert("Outage", serviceID, "we are looking")

	messageID := app.alert(alertID).Messages[0].ID

	service := strconv.Itoa(serviceID)
	monitor := strconv.Itoa(monitorID)
	channel := strconv.Itoa(channelID)
	group := strconv.Itoa(groupID)
	alert := strconv.Itoa(alertID)
	message := strconv.Itoa(messageID)

	require.NoError(t, db.Close())
	require.NoError(t, rwDB.Close())

	gets := []string{
		"/",
		"/history",
		"/healthz",
		"/admin",
		"/admin/alerts",
		"/admin/alerts/notifications",
		"/admin/alerts/create",
		"/admin/alerts/" + alert,
		"/admin/alerts/" + alert + "/edit",
		"/admin/alerts/" + alert + "/messages",
		"/admin/alerts/" + alert + "/messages/" + message,
		"/admin/monitors",
		"/admin/monitors/create",
		"/admin/monitors/" + monitor,
		"/admin/monitors/" + monitor + "/edit",
		"/admin/monitors/" + monitor + "/view",
		"/admin/services",
		"/admin/services/create",
		"/admin/services/" + service + "/edit",
		"/admin/notifications",
		"/admin/notifications/create",
		"/admin/notifications/" + channel + "/edit",
		"/admin/notifications/" + channel + "/view",
		"/admin/notifications/mail-groups/create",
		"/admin/notifications/mail-groups/" + group + "/edit",
		"/admin/notifications/mail-groups/" + group + "/view",
		"/admin/settings",
		"/admin/settings/config-settings",
		"/admin/settings/users/1/edit",
	}

	for _, path := range gets {
		resp := app.get(path)
		require.GreaterOrEqual(t, resp.status, 500, "GET %s answered %d", path, resp.status)
	}

	posts := map[string]url.Values{
		"/admin/settings":                      {"name": {"Renamed"}},
		"/admin/settings/config":               {"config": {fullConfig}},
		"/admin/settings/secrets":              {"action": {"encrypt"}, "input": {"x"}},
		"/admin/settings/users/invite":         {},
		"/admin/services/create":               {"name": {"New"}},
		"/admin/alerts/" + alert + "/resolve":  {},
		"/admin/alerts/" + alert + "/messages": {"message": {"still looking"}},
	}

	for path, form := range posts {
		resp := app.post(path, form)
		require.GreaterOrEqual(t, resp.status, 500, "POST %s answered %d", path, resp.status)
	}

	deletes := []string{
		"/admin/services/" + service,
		"/admin/monitors/" + monitor,
		"/admin/notifications/" + channel,
		"/admin/notifications/mail-groups/" + group,
		"/admin/alerts/" + alert,
		"/admin/alerts/" + alert + "/messages/" + message,
	}

	for _, path := range deletes {
		resp := app.delete(path)
		require.GreaterOrEqual(t, resp.status, 500, "DELETE %s answered %d", path, resp.status)
	}

	// Static assets are served from the embedded filesystem and owe the
	// database nothing, so they must still answer.
	require.Equal(t, http.StatusOK, app.get("/static/main.css").status)

	// And the session middleware itself: a request carrying a cookie it cannot
	// look up must not be treated as authenticated.
	require.Equal(t, http.StatusInternalServerError, app.get("/login").status)

	require.True(t, strings.HasPrefix(app.server.URL, "http://"))
}
