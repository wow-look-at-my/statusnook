package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every admin page renders. They are one bad template reference away from a
// 500 apiece and nothing else in the suite executes them.
func TestAdminPagesRender(t *testing.T) {
	app := withTestApp(t)

	serviceID := app.createService("Web", "the site")
	monitorID := app.createMonitor("API", "https://example.com/health")
	channelID := app.createSlackChannel("Chat", "https://hooks.example.com/x")
	groupID := app.createMailGroup("Ops", "a@example.com")
	alertID := app.createAlert("Outage", serviceID, "we are looking")

	for _, path := range []string{
		"/admin",
		"/admin/alerts",
		"/admin/alerts/notifications",
		"/admin/alerts/create",
		"/admin/alerts/" + strconv.Itoa(alertID),
		"/admin/alerts/" + strconv.Itoa(alertID) + "/edit",
		"/admin/alerts/" + strconv.Itoa(alertID) + "/messages",
		"/admin/monitors",
		"/admin/monitors/create",
		"/admin/monitors/" + strconv.Itoa(monitorID),
		"/admin/monitors/" + strconv.Itoa(monitorID) + "/edit",
		"/admin/monitors/" + strconv.Itoa(monitorID) + "/view",
		"/admin/services",
		"/admin/services/create",
		"/admin/services/" + strconv.Itoa(serviceID) + "/edit",
		"/admin/notifications",
		"/admin/notifications/create",
		"/admin/notifications/" + strconv.Itoa(channelID) + "/edit",
		"/admin/notifications/" + strconv.Itoa(channelID) + "/view",
		"/admin/notifications/mail-groups/create",
		"/admin/notifications/mail-groups/" + strconv.Itoa(groupID) + "/edit",
		"/admin/notifications/mail-groups/" + strconv.Itoa(groupID) + "/view",
		"/admin/settings",
		"/admin/settings/config-settings",
		"/admin/settings/users/1/edit",
	} {
		resp := app.get(path)
		require.Equal(t, http.StatusOK, resp.status, "GET %s: %s", path, resp.body)
	}
}

func TestServiceCRUDThroughTheRouter(t *testing.T) {
	app := withTestApp(t)

	id := app.createService("Ingest", "the pipeline")

	resp := app.post("/admin/services/"+strconv.Itoa(id)+"/edit",
		url.Values{"name": {"Ingest API"}, "helper": {"the pipeline front door"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.serviceNames(), "Ingest API")

	resp = app.delete("/admin/services/" + strconv.Itoa(id))
	require.Less(t, resp.status, 400, resp.body)
	require.NotContains(t, app.serviceNames(), "Ingest API")
}

func TestMonitorCRUDThroughTheRouter(t *testing.T) {
	app := withTestApp(t)

	id := app.createMonitor("API", "https://example.com/health")

	resp := app.post("/admin/monitors/"+strconv.Itoa(id)+"/edit", url.Values{
		"name":      {"API v2"},
		"url":       {"https://example.com/healthz"},
		"method":    {"GET"},
		"frequency": {"60"},
		"timeout":   {"5"},
		"attempts":  {"2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.monitorNames(), "API v2")

	// A monitor with no logs yet still renders its page.
	require.Equal(t, http.StatusOK, app.get("/admin/monitors/"+strconv.Itoa(id)).status)

	resp = app.delete("/admin/monitors/" + strconv.Itoa(id))
	require.Less(t, resp.status, 400, resp.body)
	require.NotContains(t, app.monitorNames(), "API v2")
}

func TestNotificationChannelAndMailGroupCRUD(t *testing.T) {
	app := withTestApp(t)

	channelID := app.createSlackChannel("Chat", "https://hooks.example.com/x")

	resp := app.post("/admin/notifications/"+strconv.Itoa(channelID)+"/edit", url.Values{
		"display-name": {"Chatter"},
		"webhook-url":  {"https://hooks.example.com/y"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.channelNames(), "Chatter")

	groupID := app.createMailGroup("Ops", "a@example.com")

	resp = app.post("/admin/notifications/mail-groups/"+strconv.Itoa(groupID)+"/edit", url.Values{
		"name":        {"Operations"},
		"description": {"rota"},
		"members":     {"b@example.com"},
	})
	require.Less(t, resp.status, 400, resp.body)

	require.Equal(t, []string{"b@example.com"}, app.mailGroupMembers(groupID),
		"members are replaced, not appended")

	resp = app.delete("/admin/notifications/mail-groups/" + strconv.Itoa(groupID))
	require.Less(t, resp.status, 400, resp.body)

	resp = app.delete("/admin/notifications/" + strconv.Itoa(channelID))
	require.Less(t, resp.status, 400, resp.body)
	require.NotContains(t, app.channelNames(), "Chatter")
}

func TestAlertLifecycleThroughTheRouter(t *testing.T) {
	app := withTestApp(t)

	serviceID := app.createService("Web", "the site")
	alertID := app.createAlert("Outage", serviceID, "we are looking")
	path := "/admin/alerts/" + strconv.Itoa(alertID)

	require.Contains(t, app.get(path).body, "we are looking")

	resp := app.post(path+"/messages", url.Values{"message": {"a workaround is live"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.get(path).body, "a workaround is live")

	// The severity banner follows the worst ongoing alert and drops back when
	// the last one is resolved.
	require.Contains(t, app.get("/").body, "Outage")

	resp = app.post(path+"/resolve", nil)
	require.Less(t, resp.status, 400, resp.body)
	require.NotNil(t, app.alert(alertID).EndedAt, "resolve must stamp ended_at")

	resp = app.post(path+"/unresolve", nil)
	require.Less(t, resp.status, 400, resp.body)
	require.Nil(t, app.alert(alertID).EndedAt, "unresolve must clear ended_at")

	resp = app.post(path+"/edit", url.Values{
		"title":    {"Partial outage"},
		"services": {strconv.Itoa(serviceID)},
		"type":     {"incident"},
		"severity": {"amber"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "Partial outage", app.alert(alertID).Title)

	resp = app.delete(path)
	require.Less(t, resp.status, 400, resp.body)
	require.NotContains(t, app.alertIDs(), alertID)
}

// A session cookie is not enough on its own: the mutating routes also want a
// CSRF token, and the admin tree redirects anonymous requests to /login.
func TestAdminRejectsMissingCSRFAndAnonymousRequests(t *testing.T) {
	app := withTestApp(t)

	good := app.csrfToken
	app.csrfToken = "not-the-token"
	resp := app.post("/admin/services/create", url.Values{"name": {"Nope"}})
	require.Equal(t, http.StatusForbidden, resp.status)
	app.csrfToken = good

	anonymous := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := anonymous.Get(app.server.URL + "/admin/services")
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusFound, res.StatusCode)
	require.Equal(t, "/login", res.Header.Get("Location"))
}

func (a *testApp) createService(name string, helper string) int {
	a.t.Helper()

	before := a.serviceIDs()
	resp := a.post("/admin/services/create", url.Values{"name": {name}, "helper": {helper}})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.serviceIDs())
}

func (a *testApp) serviceIDs() map[int]bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	services, err := listServices(tx)
	require.NoError(a.t, err)

	ids := map[int]bool{}
	for _, s := range services {
		ids[s.ID] = true
	}

	return ids
}

func (a *testApp) createMonitor(name string, monitorURL string) int {
	a.t.Helper()

	before := a.monitorIDs()
	resp := a.post("/admin/monitors/create", url.Values{
		"name":      {name},
		"url":       {monitorURL},
		"method":    {"GET"},
		"frequency": {"60"},
		"timeout":   {"5"},
		"attempts":  {"2"},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.monitorIDs())
}

func (a *testApp) monitorIDs() map[int]bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	require.NoError(a.t, err)

	ids := map[int]bool{}
	for _, m := range monitors {
		ids[m.ID] = true
	}

	return ids
}

func (a *testApp) createSlackChannel(name string, webhookURL string) int {
	a.t.Helper()

	before := a.channelIDs()
	resp := a.post("/admin/notifications/create", url.Values{
		"type":         {"slack"},
		"display-name": {name},
		"webhook-url":  {webhookURL},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.channelIDs())
}

func (a *testApp) channelIDs() map[int]bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(a.t, err)

	ids := map[int]bool{}
	for _, c := range channels {
		ids[c.ID] = true
	}

	return ids
}

func (a *testApp) createMailGroup(name string, member string) int {
	a.t.Helper()

	before := a.mailGroupIDs()
	resp := a.post("/admin/notifications/mail-groups/create", url.Values{
		"name":        {name},
		"description": {"on call"},
		"members":     {member},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.mailGroupIDs())
}

func (a *testApp) mailGroupIDs() map[int]bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	groups, err := listMailGroups(tx)
	require.NoError(a.t, err)

	ids := map[int]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}

	return ids
}

func (a *testApp) createAlert(title string, serviceID int, message string) int {
	a.t.Helper()

	before := a.alertIDs()
	resp := a.post("/admin/alerts/create", url.Values{
		"title":    {title},
		"message":  {message},
		"services": {strconv.Itoa(serviceID)},
		"type":     {"incident"},
		"severity": {"red"},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.alertIDs())
}

func (a *testApp) alertIDs() map[int]bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	alerts, err := listAlerts(tx)
	require.NoError(a.t, err)

	ids := map[int]bool{}
	for _, alert := range alerts {
		ids[alert.ID] = true
	}

	return ids
}

func onlyNewID(t *testing.T, before map[int]bool, after map[int]bool) int {
	t.Helper()

	created := []int{}
	for id := range after {
		if !before[id] {
			created = append(created, id)
		}
	}

	require.Len(t, created, 1, "expected exactly one new row")

	return created[0]
}

func (a *testApp) serviceNames() []string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	services, err := listServices(tx)
	require.NoError(a.t, err)

	names := []string{}
	for _, s := range services {
		names = append(names, s.Name)
	}

	return names
}

func (a *testApp) monitorNames() []string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	require.NoError(a.t, err)

	names := []string{}
	for _, m := range monitors {
		names = append(names, m.Name)
	}

	return names
}

func (a *testApp) channelNames() []string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(a.t, err)

	names := []string{}
	for _, c := range channels {
		names = append(names, c.Name)
	}

	return names
}

func (a *testApp) mailGroupMembers(id int) []string {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	members, err := listMailGroupMembersByID(tx, id)
	require.NoError(a.t, err)

	addresses := []string{}
	for _, m := range members {
		addresses = append(addresses, m.EmailAddress)
	}

	return addresses
}

func (a *testApp) alert(id int) AlertDetail {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	alert, err := getAlertByID(tx, id)
	require.NoError(a.t, err)

	return alert
}
