package main

import (
	"database/sql"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Most admin pages branch on how much of the instance is set up, and the
// version rendered on a bare install is the one the rest of the suite sees.
// This drives them again with everything wired: a domain, a config file, a
// monitor with logs and references, a second user.
func TestAdminPagesRenderAFullyConfiguredInstance(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)

	require.Less(t, app.post("/admin/settings",
		url.Values{"domain": {"status.example.com"}}).status, 400)

	channelID := app.createSMTPChannel("Mail")
	slackID := app.createSlackChannel("Chat", "https://hooks.example.com/x")
	groupID := app.createMailGroup("Ops", "ops@example.com")
	serviceID := app.createService("Ingest", "the pipeline")

	require.Less(t, app.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		"slack-install-url":         {"https://slack.example.com/install"},
		"slack-client-secret":       {"shh"},
		"managed-subscriptions":     {"on"},
	}).status, 400)

	before := app.monitorIDs()
	require.Less(t, app.post("/admin/monitors/create", url.Values{
		"name":                  {"Ingest"},
		"url":                   {"https://example.com/ingest"},
		"method":                {"POST"},
		"frequency":             {"60"},
		"timeout":               {"5"},
		"attempts":              {"2"},
		"header-key":            {"Content-Type"},
		"header-value":          {"application/json"},
		"format":                {"json"},
		"body":                  {`{"ping":1}`},
		"notification-channels": {strconv.Itoa(slackID)},
		"mail-groups":           {strconv.Itoa(groupID)},
	}).status, 400)
	monitorID := onlyNewID(t, before, app.monitorIDs())

	day := time.Now().UTC().Truncate(24 * time.Hour)
	app.seedMonitorLogs(monitorID, day, 3)

	alertID := app.createAlert("Outage", serviceID, "we are looking")
	app.createSecondUser("second")

	require.Less(t, app.post("/admin/settings/users/invite", nil).status, 400)

	editable := []string{
		"/admin/monitors/" + strconv.Itoa(monitorID) + "/edit",
		"/admin/notifications/" + strconv.Itoa(channelID) + "/edit",
		"/admin/notifications/mail-groups/" + strconv.Itoa(groupID) + "/edit",
		"/admin/services/" + strconv.Itoa(serviceID) + "/edit",
	}

	always := []string{
		"/",
		"/history",
		"/admin",
		"/admin/alerts",
		"/admin/alerts/notifications",
		"/admin/alerts/" + strconv.Itoa(alertID),
		"/admin/monitors",
		"/admin/monitors/" + strconv.Itoa(monitorID),
		"/admin/monitors/" + strconv.Itoa(monitorID) + "/view",
		"/admin/notifications",
		"/admin/notifications/" + strconv.Itoa(channelID) + "/view",
		"/admin/notifications/mail-groups/" + strconv.Itoa(groupID) + "/view",
		"/admin/services",
		"/admin/settings",
		"/admin/settings/config-settings",
	}

	for _, path := range append(append([]string{}, always...), editable...) {
		resp := app.get(path)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", path, resp.body)
	}

	// Now under a config file and synced from GitHub, which is the state the
	// settings pages have the most to say about.
	gh.serve("general-settings:\n  name: Test Status\n", "sha-zero")
	require.Less(t, app.post("/admin/settings/config-settings", url.Values{
		"config-file":           {"on"},
		"github-managed":        {"on"},
		"github-repo-url":       {"https://github.com/example/status"},
		"github-branch":         {"master"},
		"github-config-path":    {"config.yaml"},
		"github-token":          {"ghtoken"},
		"github-webhook-secret": {"hooksecret"},
	}).status, 400)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	// The file owns the instance now: the read-only pages still render and the
	// edit forms are closed, because anything typed into them would be undone
	// by the next push.
	for _, path := range always {
		resp := app.get(path)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", path, resp.body)
	}

	for _, path := range editable {
		require.Equal(t, http.StatusBadRequest, app.get(path).status, "GET %s", path)
	}

	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/services/create", url.Values{"name": {"Nope"}}).status)
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/alerts/notifications", url.Values{}).status)
}

// A monitor whose last check failed renders differently from one that has never
// been checked, and both have to survive the same page.
func TestMonitorPageRendersEveryCheckState(t *testing.T) {
	app := withTestApp(t)

	id := app.createMonitor("API", "https://example.com/health")
	path := "/admin/monitors/" + strconv.Itoa(id)

	// Never checked.
	require.Equal(t, http.StatusOK, app.get(path).status)

	day := time.Now().UTC().Truncate(24 * time.Hour)

	tx, err := rwDB.Begin()
	require.NoError(t, err)

	failed, err := createMonitorLog(tx, day, day.Add(time.Second), 500,
		nullString("500 Internal Server Error"), 2, "error", id)
	require.NoError(t, err)
	require.NoError(t, createMonitorLogLastChecked(tx, day, id, failed))
	require.NoError(t, tx.Commit())

	body := app.get(path).body
	require.Contains(t, body, "500")

	// And once it recovers.
	tx, err = rwDB.Begin()
	require.NoError(t, err)

	ok, err := createMonitorLog(tx, day.Add(time.Minute), day.Add(time.Minute), 200,
		nullString(""), 1, "success", id)
	require.NoError(t, err)
	require.NoError(t, createMonitorLogLastChecked(tx, day.Add(time.Minute), id, ok))
	require.NoError(t, tx.Commit())

	require.Equal(t, http.StatusOK, app.get(path).status)
	require.Equal(t, http.StatusOK, app.get(path+"/edit").status)
}

// Two accounts exist, so the settings page has a list to render and the delete
// path has something other than the last account to remove.
func TestUserManagementPages(t *testing.T) {
	app := withTestApp(t)

	id := app.createSecondUser("second")

	require.Less(t, app.post("/admin/settings/users/invite", nil).status, 400)

	body := app.get("/admin/settings").body
	require.Contains(t, body, "second")

	require.Equal(t, http.StatusOK,
		app.get("/admin/settings/users/"+strconv.Itoa(id)+"/edit").status)

	// An id that is not a number, and one that is not a user.
	require.Equal(t, http.StatusBadRequest, app.get("/admin/settings/users/abc/edit").status)
	require.Equal(t, http.StatusNotFound, app.get("/admin/settings/users/99999/edit").status)
	require.Equal(t, http.StatusBadRequest, app.delete("/admin/settings/users/abc").status)
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}

	return sql.NullString{String: s, Valid: true}
}
