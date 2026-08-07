package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Values nobody would type. What every form owes here is the same thing: an
// answer. A panic drops the connection without one -- that is how the secrets
// tool's GCM nonce crash was found -- and the client cannot tell that apart
// from the instance being gone.
func hostileValues() []string {
	return []string{
		"",
		" ",
		strings.Repeat("a", 100000),
		"\x00\x01\x02",
		"../../etc/passwd",
		"<script>alert(1)</script>",
		"' or 1=1 --",
		"%00%01",
		"-1",
		"99999999999999999999999999",
		"\u202e\u0000 unicode",
		"secret_.",
		"secret_AAAA.AAAA",
	}
}

func TestFormsAnswerHostileInput(t *testing.T) {
	app := withTestApp(t)

	serviceID := strconv.Itoa(app.createService("Web", "the site"))
	monitorID := strconv.Itoa(app.createMonitor("API", "https://example.com/health"))
	channelID := strconv.Itoa(app.createSlackChannel("Chat", "https://hooks.example.com/x"))
	groupID := strconv.Itoa(app.createMailGroup("Ops", "ops@example.com"))

	alertNumber := app.createAlert("Outage", app.createService("Api", "the api"), "looking")
	alertID := strconv.Itoa(alertNumber)
	messageID := strconv.Itoa(app.alert(alertNumber).Messages[0].ID)

	forms := map[string]url.Values{
		"/admin/services/create":                                {"name": {"x"}, "helper": {"x"}},
		"/admin/services/" + serviceID + "/edit":                {"name": {"x"}, "helper": {"x"}},
		"/admin/notifications/create":                           {"type": {"slack"}, "display-name": {"x"}, "webhook-url": {"https://hooks.example.com/x"}},
		"/admin/notifications/" + channelID + "/edit":           {"display-name": {"x"}, "webhook-url": {"https://hooks.example.com/x"}},
		"/admin/notifications/mail-groups/create":               {"name": {"x"}, "description": {"x"}, "members": {"a@example.com"}},
		"/admin/notifications/mail-groups/" + groupID + "/edit": {"name": {"x"}, "members": {"a@example.com"}},
		"/admin/monitors/create": {
			"name": {"x"}, "url": {"https://example.com"}, "method": {"GET"},
			"frequency": {"60"}, "timeout": {"5"}, "attempts": {"2"},
			"header-key": {"X"}, "header-value": {"y"}, "body": {"b"}, "format": {"json"},
		},
		"/admin/monitors/" + monitorID + "/edit": {
			"name": {"x"}, "url": {"https://example.com"}, "method": {"GET"},
			"frequency": {"60"}, "timeout": {"5"}, "attempts": {"2"},
		},
		"/admin/alerts/create": {
			"title": {"x"}, "message": {"m"}, "services": {serviceID},
			"type": {"incident"}, "severity": {"red"},
		},
		"/admin/alerts/" + alertID + "/edit": {
			"title": {"x"}, "services": {serviceID}, "type": {"incident"}, "severity": {"red"},
		},
		"/admin/alerts/" + alertID + "/messages":              {"message": {"m"}},
		"/admin/alerts/" + alertID + "/messages/" + messageID: {"message": {"m"}},
		"/admin/settings":                 {"name": {"x"}, "domain": {""}},
		"/admin/settings/users/1/edit":    {"username": {"x"}, "password": {"retain"}},
		"/admin/settings/secrets":         {"action": {"decrypt"}, "input": {"x"}},
		"/admin/settings/config":          {"config": {fullConfig}},
		"/admin/settings/config-settings": {"config-file": {""}},
		"/admin/alerts/notifications":     {"slack-install-url": {""}, "smtp-notification-channel": {""}},
		"/subscribe/email":                {"email": {"a@example.com"}},
		"/unsubscribe":                    {"token": {"t"}},
		"/resubscribe":                    {"token": {"t"}},
		"/login":                          {"username": {"admin"}, "password": {"hunter2hunter2"}},
	}

	for path, template := range forms {
		for field := range template {
			for _, value := range hostileValues() {
				form := url.Values{}
				for k, v := range template {
					form[k] = append([]string(nil), v...)
				}
				form.Set(field, value)

				// app.post fails the test if the connection drops, which is
				// what a panicking handler looks like from out here.
				resp := app.post(path, form)
				require.Less(t, resp.status, 600,
					"POST %s with %s=%.40q answered %d", path, field, value, resp.status)
			}
		}
	}
}
