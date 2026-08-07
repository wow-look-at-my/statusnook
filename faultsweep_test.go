package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fails the nth database statement of a request and repeats with n = 1, 2, 3...
// until the handler gets far enough to answer normally. Every branch a handler
// has after a query is one of these, and what they all owe the caller is the
// same: a 5xx, never a panic and never a 2xx page built from a failed read.
//
// A handler that answers under 400 with a statement failed has swallowed the
// error, which is the defect this is looking for.
func sweepFaults(t *testing.T, app *testApp, method string, path string, form url.Values) {
	t.Helper()

	const maxStatements = 30

	for depth := int64(1); depth <= maxStatements; depth++ {
		failAtStatement(depth)

		var resp testResponse
		switch method {
		case http.MethodGet:
			resp = app.get(path)
		case http.MethodPost:
			resp = app.post(path, form)
		case http.MethodDelete:
			resp = app.delete(path)
		}

		fired := faultFired()
		failAtStatement(0)

		if !fired {
			// The request ran fewer than `depth` statements, so nothing was
			// injected and there is nothing deeper to fail.
			return
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"%s %s answered %d with statement %d failed", method, path, resp.status, depth)
	}
}

func TestHandlersSurfaceAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)

	serviceID := strconv.Itoa(app.createService("Web", "the site"))
	monitorID := strconv.Itoa(app.createMonitor("API", "https://example.com/health"))
	channelID := strconv.Itoa(app.createSlackChannel("Chat", "https://hooks.example.com/x"))
	groupID := strconv.Itoa(app.createMailGroup("Ops", "ops@example.com"))

	alertNumber := app.createAlert("Outage", app.createService("Api", "the api"), "we are looking")
	alertID := strconv.Itoa(alertNumber)
	messageID := strconv.Itoa(app.alert(alertNumber).Messages[0].ID)

	withFaultyDB(app)

	for _, path := range []string{
		"/",
		"/history",
		"/healthz",
		"/resolve",
		"/admin",
		"/admin/alerts",
		"/admin/alerts/notifications",
		"/admin/alerts/create",
		"/admin/alerts/" + alertID,
		"/admin/alerts/" + alertID + "/edit",
		"/admin/alerts/" + alertID + "/messages",
		"/admin/alerts/" + alertID + "/messages/" + messageID,
		"/admin/monitors",
		"/admin/monitors/create",
		"/admin/monitors/" + monitorID,
		"/admin/monitors/" + monitorID + "/edit",
		"/admin/monitors/" + monitorID + "/view",
		"/admin/services",
		"/admin/services/create",
		"/admin/services/" + serviceID + "/edit",
		"/admin/notifications",
		"/admin/notifications/create",
		"/admin/notifications/" + channelID + "/edit",
		"/admin/notifications/" + channelID + "/view",
		"/admin/notifications/mail-groups/create",
		"/admin/notifications/mail-groups/" + groupID + "/edit",
		"/admin/notifications/mail-groups/" + groupID + "/view",
		"/admin/settings",
		"/admin/settings/config-settings",
		"/admin/settings/users/1/edit",
		"/subscribe/email/confirm",
		"/unsubscribe?token=whatever",
	} {
		sweepFaults(t, app, http.MethodGet, path, nil)
	}

	for path, form := range map[string]url.Values{
		"/admin/settings":                        {"name": {"Renamed"}},
		"/admin/settings/config":                 {"config": {fullConfig}},
		"/admin/settings/secrets":                {"action": {"encrypt"}, "input": {"x"}},
		"/admin/settings/users/invite":           {},
		"/admin/settings/users/1/edit":           {"username": {"admin"}, "password": {"retain"}},
		"/admin/settings/cancel-domain":          {},
		"/admin/settings/config-settings":        {"config-file": {"on"}},
		"/admin/services/create":                 {"name": {"Another"}},
		"/admin/services/" + serviceID + "/edit": {"name": {"Web"}, "helper": {"the site"}},
		"/admin/notifications/create": {
			"type": {"slack"}, "display-name": {"Chat2"},
			"webhook-url": {"https://hooks.example.com/y"},
		},
		"/admin/notifications/mail-groups/create": {
			"name": {"Ops2"}, "members": {"ops2@example.com"},
		},
		"/admin/monitors/create": {
			"name": {"API2"}, "url": {"https://example.com/health"}, "method": {"GET"},
			"frequency": {"60"}, "timeout": {"5"}, "attempts": {"2"},
		},
		"/admin/alerts/create": {
			"title": {"Second"}, "message": {"looking"}, "services": {serviceID},
			"type": {"incident"}, "severity": {"red"},
		},
		"/admin/alerts/" + alertID + "/messages":  {"message": {"an update"}},
		"/admin/alerts/" + alertID + "/resolve":   {},
		"/admin/alerts/" + alertID + "/unresolve": {},
		"/admin/resolve":                          {},
		"/unsubscribe":                            {"token": {"whatever"}},
		"/resubscribe":                            {"token": {"whatever"}},
		"/subscribe/email":                        {"email": {"sub@example.com"}},
		"/github-config-webhook":                  {},
	} {
		sweepFaults(t, app, http.MethodPost, path, form)
	}

	for _, path := range []string{
		"/admin/services/" + serviceID,
		"/admin/monitors/" + monitorID,
		"/admin/notifications/" + channelID,
		"/admin/notifications/mail-groups/" + groupID,
		"/admin/alerts/" + alertID + "/messages/" + messageID,
		"/admin/alerts/" + alertID,
		"/admin/settings/users/1",
	} {
		sweepFaults(t, app, http.MethodDelete, path, nil)
	}

	// The pool is still usable afterwards: a failed statement must not have
	// left a transaction open on the single writer connection.
	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/admin/alerts").status)
}
