package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The edit forms carry the same contract as the create forms, and are the ones
// an operator uses under pressure. A rejection here has to leave the stored
// resource exactly as it was.
func TestMonitorEditFormRejectsBadInputAndKeepsTheOldValues(t *testing.T) {
	app := withTestApp(t)

	channelID := app.createSlackChannel("Chat", "https://hooks.example.com/x")
	groupID := app.createMailGroup("Ops", "ops@example.com")
	id := app.createMonitor("API", "https://example.com/health")
	path := "/admin/monitors/" + strconv.Itoa(id) + "/edit"

	valid := func() url.Values {
		return url.Values{
			"name":      {"API v2"},
			"url":       {"https://example.com/healthz"},
			"method":    {"GET"},
			"frequency": {"60"},
			"timeout":   {"5"},
			"attempts":  {"2"},
		}
	}

	for field, bad := range map[string][]string{
		"name":      {""},
		"url":       {"", "not a url", "ftp://example.com"},
		"method":    {"", "TRACE"},
		"frequency": {"", "45"},
		"timeout":   {"", "7"},
		"attempts":  {"", "4"},
	} {
		for _, value := range bad {
			form := valid()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest, app.post(path, form).status,
				"%s=%q was accepted", field, value)
		}
	}

	require.Equal(t, "API", app.monitor(id).Name, "a rejected edit must change nothing")

	// A good edit attaches the references and the page renders them back.
	form := valid()
	form["notification-channels"] = []string{strconv.Itoa(channelID)}
	form["mail-groups"] = []string{strconv.Itoa(groupID)}
	form.Set("header-key", "Range")
	form.Set("header-value", "bytes=0-0")

	resp := app.post(path, form)
	require.Less(t, resp.status, 400, resp.body)

	monitor := app.monitor(id)
	require.Equal(t, "API v2", monitor.Name)
	require.Equal(t, "bytes=0-0", monitor.RequestHeaders["Range"])

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	channels, err := listNotificationChannelsByMonitorID(tx, id)
	require.NoError(t, err)
	require.Len(t, channels, 1)

	groups, err := listMailGroupIDsByMonitorID(tx, id)
	require.NoError(t, err)
	require.Len(t, groups, 1)

	require.NoError(t, tx.Rollback())

	require.Equal(t, http.StatusOK, app.get(path).status)
	require.Equal(t, http.StatusOK, app.get("/admin/monitors/"+strconv.Itoa(id)).status)

	metaConfigFileEnabled.Store(true)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })
	require.Equal(t, http.StatusBadRequest, app.post(path, valid()).status)
}

func TestNotificationEditFormRejectsBadInput(t *testing.T) {
	app := withTestApp(t)

	smtpID := app.createSMTPChannel("Mail")
	smtpPath := "/admin/notifications/" + strconv.Itoa(smtpID) + "/edit"

	valid := func() url.Values {
		return url.Values{
			"display-name": {"Mailer"},
			"host":         {"smtp.example.com"},
			"port":         {"587"},
			"username":     {"statusnook"},
			"password":     {"shh"},
			"from":         {"status@example.com"},
		}
	}

	for field, bad := range map[string][]string{
		"display-name": {""},
		"host":         {""},
		"port":         {"", "notanumber"},
		"username":     {""},
		"password":     {""},
		"from":         {"not an address"},
	} {
		for _, value := range bad {
			form := valid()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest, app.post(smtpPath, form).status,
				"%s=%q was accepted", field, value)
		}
	}

	require.Contains(t, app.channelNames(), "Mail", "a rejected edit must change nothing")

	resp := app.post(smtpPath, valid())
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.channelNames(), "Mailer")

	// Switching an existing channel to Postmark needs both streams, same as
	// creating one there.
	postmark := valid()
	postmark.Set("host", "smtp.postmarkapp.com")
	require.Equal(t, http.StatusBadRequest, app.post(smtpPath, postmark).status)

	slackID := app.createSlackChannel("Chat", "https://hooks.example.com/x")
	slackPath := "/admin/notifications/" + strconv.Itoa(slackID) + "/edit"

	require.Equal(t, http.StatusBadRequest,
		app.post(slackPath, url.Values{"display-name": {"Chat"}, "webhook-url": {"nope"}}).status)
	require.Equal(t, http.StatusBadRequest,
		app.post(slackPath, url.Values{"webhook-url": {"https://hooks.example.com/y"}}).status)

	metaConfigFileEnabled.Store(true)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })
	require.Equal(t, http.StatusBadRequest, app.post(smtpPath, valid()).status)
}

func TestAlertEditFormRejectsBadInput(t *testing.T) {
	app := withTestApp(t)

	serviceID := app.createService("Web", "the site")
	alertID := app.createAlert("Outage", serviceID, "we are looking")
	path := "/admin/alerts/" + strconv.Itoa(alertID) + "/edit"

	valid := func() url.Values {
		return url.Values{
			"title":    {"Partial outage"},
			"services": {strconv.Itoa(serviceID)},
			"type":     {"incident"},
			"severity": {"amber"},
		}
	}

	for field, bad := range map[string][]string{
		"title":    {""},
		"services": {"", "notanumber"},
		"type":     {"", "rumour"},
		"severity": {"", "puce"},
	} {
		for _, value := range bad {
			form := valid()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest, app.post(path, form).status,
				"%s=%q was accepted", field, value)
		}
	}

	require.Equal(t, "Outage", app.alert(alertID).Title, "a rejected edit must change nothing")
}

// The public history page is paged by month, and the period parameter comes
// straight off the query string.
func TestHistoryPagesByMonth(t *testing.T) {
	app := withTestApp(t)

	serviceID := app.createService("Web", "the site")
	app.createAlert("Outage", serviceID, "we are looking")

	require.Contains(t, app.get("/history").body, "Outage")

	// A month with nothing in it still renders.
	require.Equal(t, http.StatusOK, app.get("/history?period=2020-01").status)
	require.NotContains(t, app.get("/history?period=2020-01").body, "we are looking")

	// A period that is not one is a link somebody typed, not a fault: both the
	// wrong-length and the unparseable spelling go back to the current month.
	for _, period := range []string{"nonsense", "2020-13", "2020-1", "20-01"} {
		resp := app.get("/history?period=" + period)
		require.Equal(t, http.StatusFound, resp.status, "period=%s", period)
		require.Equal(t, "/history", resp.header.Get("Location"))
	}
}
