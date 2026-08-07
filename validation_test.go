package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every create and edit form is a contract with whoever posts to it, and the
// rejections are the half of that contract nothing else in the suite reaches.
// A value that slips past one of these ends up in the database, where the
// monitor loop or the config generator is the one that breaks.

func TestMonitorFormRejectsEveryBadField(t *testing.T) {
	app := withTestApp(t)

	valid := func() url.Values {
		return url.Values{
			"name":      {"API"},
			"url":       {"https://example.com/health"},
			"method":    {"GET"},
			"frequency": {"60"},
			"timeout":   {"5"},
			"attempts":  {"2"},
		}
	}

	for field, bad := range map[string][]string{
		"name":      {""},
		"url":       {"", "not a url", "ftp://example.com", "/relative"},
		"method":    {"", "TRACE"},
		"frequency": {"", "notanumber", "45"},
		"timeout":   {"", "notanumber", "7"},
		"attempts":  {"", "notanumber", "4"},
	} {
		for _, value := range bad {
			form := valid()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest,
				app.post("/admin/monitors/create", form).status,
				"%s=%q was accepted", field, value)
		}
	}

	// The whole form is refused while a config file owns the instance.
	metaConfigFileEnabled.Store(true)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })
	require.Equal(t, http.StatusBadRequest, app.post("/admin/monitors/create", valid()).status)
}

// Request headers and bodies are the parts of a monitor that reach the wire, so
// both spellings of a body -- raw and form pairs -- have to round-trip.
func TestMonitorFormStoresHeadersAndBodies(t *testing.T) {
	app := withTestApp(t)

	before := app.monitorIDs()
	resp := app.post("/admin/monitors/create", url.Values{
		"name":         {"Post JSON"},
		"url":          {"https://example.com/ingest"},
		"method":       {"POST"},
		"frequency":    {"60"},
		"timeout":      {"5"},
		"attempts":     {"1"},
		"header-key":   {"Content-Type", "X-Trace"},
		"header-value": {"application/json", "on"},
		"format":       {"json"},
		"body":         {`{"ping":1}`},
	})
	require.Less(t, resp.status, 400, resp.body)
	jsonID := onlyNewID(t, before, app.monitorIDs())

	monitor := app.monitor(jsonID)
	require.Equal(t, "application/json", monitor.RequestHeaders["Content-Type"])
	require.Equal(t, "on", monitor.RequestHeaders["X-Trace"])
	require.Equal(t, `{"ping":1}`, monitor.Body.String)

	before = app.monitorIDs()
	resp = app.post("/admin/monitors/create", url.Values{
		"name":       {"Post form"},
		"url":        {"https://example.com/ingest"},
		"method":     {"POST"},
		"frequency":  {"60"},
		"timeout":    {"5"},
		"attempts":   {"1"},
		"form-key":   {"a", "b"},
		"form-value": {"1", "2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	formID := onlyNewID(t, before, app.monitorIDs())

	// form-key/form-value pairs are urlencoded into the body.
	require.Equal(t, "a=1&b=2", app.monitor(formID).Body.String)

	// Both monitors render their edit page with everything filled in.
	for _, id := range []int{jsonID, formID} {
		require.Equal(t, http.StatusOK,
			app.get("/admin/monitors/"+strconv.Itoa(id)+"/edit").status)
	}
}

func TestNotificationChannelFormRejectsEveryBadField(t *testing.T) {
	app := withTestApp(t)

	smtp := func() url.Values {
		return url.Values{
			"type":         {"smtp"},
			"display-name": {"Mail"},
			"host":         {"smtp.example.com"},
			"port":         {"587"},
			"username":     {"statusnook"},
			"password":     {"shh"},
			"from":         {"status@example.com"},
		}
	}

	for field, bad := range map[string][]string{
		"type":         {"", "carrier-pigeon"},
		"display-name": {""},
		"host":         {""},
		"port":         {"", "notanumber"},
		"username":     {""},
		"password":     {""},
		"from":         {"not an address"},
	} {
		for _, value := range bad {
			form := smtp()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest,
				app.post("/admin/notifications/create", form).status,
				"%s=%q was accepted", field, value)
		}
	}

	// Postmark carries two message streams, and neither is optional.
	postmark := smtp()
	postmark.Set("host", "smtp.postmarkapp.com")
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/notifications/create", postmark).status)

	postmark.Set("pm-transactional", "outbound")
	postmark.Set("pm-broadcast", "broadcast")
	require.Less(t, app.post("/admin/notifications/create", postmark).status, 400)

	// And a slack channel needs a webhook that is actually a URL.
	require.Equal(t, http.StatusBadRequest, app.post("/admin/notifications/create", url.Values{
		"type":         {"slack"},
		"display-name": {"Chat"},
		"webhook-url":  {"not a url"},
	}).status)
}

// The headers an SMTP channel carries survive the round trip, and Postmark's
// stream header is injected rather than stored, so it must not come back.
func TestSMTPChannelStoresItsHeaders(t *testing.T) {
	app := withTestApp(t)

	before := app.channelIDs()
	resp := app.post("/admin/notifications/create", url.Values{
		"type":         {"smtp"},
		"display-name": {"Mail"},
		"host":         {"smtp.example.com"},
		"port":         {"587"},
		"username":     {"statusnook"},
		"password":     {"shh"},
		"from":         {"status@example.com"},
		"header-key":   {"X-Mailer"},
		"header-value": {"statusnook"},
	})
	require.Less(t, resp.status, 400, resp.body)

	id := onlyNewID(t, before, app.channelIDs())

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, id)
	require.NoError(t, err)

	details, ok := channel.Details.(SMTPNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "statusnook", details.Headers["X-Mailer"])
	require.Equal(t, 587, details.Port)
}

func TestAlertFormRejectsEveryBadField(t *testing.T) {
	app := withTestApp(t)

	serviceID := strconv.Itoa(app.createService("Web", "the site"))

	valid := func() url.Values {
		return url.Values{
			"title":    {"Outage"},
			"message":  {"we are looking"},
			"services": {serviceID},
			"type":     {"incident"},
			"severity": {"red"},
		}
	}

	for field, bad := range map[string][]string{
		"title":    {""},
		"message":  {""},
		"services": {"", "notanumber"},
		"type":     {"", "rumour"},
		"severity": {"", "puce"},
	} {
		for _, value := range bad {
			form := valid()
			form.Set(field, value)

			require.Equal(t, http.StatusBadRequest,
				app.post("/admin/alerts/create", form).status,
				"%s=%q was accepted", field, value)
		}
	}

	// A maintenance alert carries no severity, so the severity check does not
	// apply to it.
	maintenance := valid()
	maintenance.Set("type", "maintenance")
	maintenance.Set("severity", "")
	require.Less(t, app.post("/admin/alerts/create", maintenance).status, 400)
}

func TestMailGroupAndUserFormsRejectBadInput(t *testing.T) {
	app := withTestApp(t)

	require.Equal(t, http.StatusBadRequest, app.post("/admin/notifications/mail-groups/create",
		url.Values{"description": {"no name"}}).status)

	require.Equal(t, http.StatusBadRequest, app.post("/admin/notifications/mail-groups/create",
		url.Values{"name": {"Ops"}, "members": {"not an address"}}).status)

	id := strconv.Itoa(app.createSecondUser("second"))

	for _, form := range []url.Values{
		{"password": {"hunter2hunter2"}},
		{"username": {"second"}},
		{"username": {"second"}, "password": {"short"}},
	} {
		require.Equal(t, http.StatusBadRequest,
			app.post("/admin/settings/users/"+id+"/edit", form).status, "%v was accepted", form)
	}

	// "retain" is the sentinel the form posts when the password box is left
	// alone, and it must not be taken for a new password.
	resp := app.post("/admin/settings/users/"+id+"/edit",
		url.Values{"username": {"renamed"}, "password": {"retain"}})
	require.Less(t, resp.status, 400, resp.body)
	require.True(t, app.credentialsWork("renamed", "hunter2hunter2"),
		"retain must leave the existing password alone")

	require.Equal(t, http.StatusNotFound, app.post("/admin/settings/users/99999/edit",
		url.Values{"username": {"ghost"}, "password": {"retain"}}).status)
}

func (a *testApp) monitor(id int) Monitor {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	monitor, err := getMonitorByID(tx, id)
	require.NoError(a.t, err)

	return monitor
}

// The edit form takes the same rich shapes the create form does, and it is the
// only way to change what an existing monitor sends.
func TestMonitorEditFormStoresHeadersBodiesAndRecipients(t *testing.T) {
	app := withTestApp(t)

	channelID := strconv.Itoa(app.createSlackChannel("Chat", "https://hooks.example.com/x"))
	groupID := strconv.Itoa(app.createMailGroup("Ops", "ops@example.com"))
	id := app.createMonitor("Ingest", "https://example.com/health")
	path := "/admin/monitors/" + strconv.Itoa(id) + "/edit"

	resp := app.post(path, url.Values{
		"name":                  {"Ingest"},
		"url":                   {"https://example.com/ingest"},
		"method":                {"POST"},
		"frequency":             {"30"},
		"timeout":               {"10"},
		"attempts":              {"3"},
		"header-key":            {"Content-Type", "X-Trace"},
		"header-value":          {"application/json", "on"},
		"format":                {"json"},
		"body":                  {`{"ping":1}`},
		"notification-channels": {channelID},
		"mail-groups":           {groupID},
	})
	require.Less(t, resp.status, 400, resp.body)

	monitor := app.monitor(id)
	require.Equal(t, "application/json", monitor.RequestHeaders["Content-Type"])
	require.Equal(t, `{"ping":1}`, monitor.Body.String)
	require.Equal(t, 30, monitor.Frequency)

	// form-key/form-value wins over body, the same way it does on create.
	resp = app.post(path, url.Values{
		"name":       {"Ingest"},
		"url":        {"https://example.com/ingest"},
		"method":     {"POST"},
		"frequency":  {"60"},
		"timeout":    {"5"},
		"attempts":   {"1"},
		"body":       {`{"ping":1}`},
		"form-key":   {"a", "b"},
		"form-value": {"1", "2"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "a=1&b=2", app.monitor(id).Body.String)

	// The recipient lists arrive as ids, and anything else is a form nobody
	// could have submitted from the page.
	for _, field := range []string{"notification-channels", "mail-groups"} {
		resp = app.post(path, url.Values{
			"name": {"Ingest"}, "url": {"https://example.com/ingest"},
			"method": {"GET"}, "frequency": {"60"}, "timeout": {"5"}, "attempts": {"1"},
			field: {"not-an-id"},
		})
		require.Equal(t, http.StatusInternalServerError, resp.status,
			"%s took a value that is not an id", field)
	}

	// A monitor that is not there, and an id that is not a number.
	require.Equal(t, http.StatusBadRequest, app.post("/admin/monitors/99999/edit", url.Values{
		"name": {"Gone"}, "url": {"https://example.com/x"},
		"method": {"GET"}, "frequency": {"60"}, "timeout": {"5"}, "attempts": {"1"},
	}).status)
}

// Editing an SMTP channel re-validates every field the create form does, and
// carries the two extras Postmark needs.
func TestSMTPChannelEditFormRejectsEveryBadField(t *testing.T) {
	app := withTestApp(t)

	id := strconv.Itoa(app.createSMTPChannel("Mail"))
	path := "/admin/notifications/" + id + "/edit"

	valid := func() url.Values {
		return url.Values{
			"display-name": {"Mail"},
			"host":         {"smtp.example.com"},
			"port":         {"587"},
			"username":     {"statusnook"},
			"password":     {"shh"},
			"from":         {"status@example.com"},
		}
	}

	for field, bad := range map[string]string{
		"display-name": "", "host": "", "port": "", "username": "",
		"password": "", "from": "not-an-address",
	} {
		form := valid()
		form.Set(field, bad)

		require.Equal(t, http.StatusBadRequest, app.post(path, form).status,
			"a %s of %q was accepted", field, bad)
	}

	notANumber := valid()
	notANumber.Set("port", "half past")
	require.Equal(t, http.StatusBadRequest, app.post(path, notANumber).status)

	withHeaders := valid()
	withHeaders["header-key"] = []string{"X-Trace", "X-Env"}
	withHeaders["header-value"] = []string{"on", "test"}
	require.Less(t, app.post(path, withHeaders).status, 400)
	require.Equal(t, "on", app.smtpDetails(id).Headers["X-Trace"])

	// Postmark needs both message streams named, since it refuses a send on
	// the wrong one rather than picking a default.
	postmark := valid()
	postmark.Set("host", "smtp.postmarkapp.com")
	require.Equal(t, http.StatusBadRequest, app.post(path, postmark).status)

	postmark.Set("pm-transactional", "outbound")
	require.Equal(t, http.StatusBadRequest, app.post(path, postmark).status)

	postmark.Set("pm-broadcast", "broadcast")
	require.Less(t, app.post(path, postmark).status, 400)
}

func (a *testApp) smtpDetails(id string) SMTPNotificationDetails {
	a.t.Helper()

	channelID, err := strconv.Atoi(id)
	require.NoError(a.t, err)

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, channelID)
	require.NoError(a.t, err)

	details, ok := channel.Details.(SMTPNotificationDetails)
	require.True(a.t, ok)

	return details
}
