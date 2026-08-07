package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

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

// The routes that need something set up first: a pending subscription to
// confirm, an invitation to redeem, a cursor to page from, a signed webhook.
func TestPreparedRequestsSurfaceAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.useSMTPChannelForAlerts()

	monitorID := strconv.Itoa(app.createMonitor("API", "https://example.com/health"))
	channelID := strconv.Itoa(app.createSlackChannel("Chat", "https://hooks.example.com/x"))
	groupID := strconv.Itoa(app.createMailGroup("Ops", "ops@example.com"))

	logged := app.createMonitor("Logged", "https://example.com/x")
	logIDs := app.seedMonitorLogs(logged, time.Now().UTC().Truncate(24*time.Hour), 2)
	loggedID := strconv.Itoa(logged)
	date := time.Now().UTC().Format("2006-01-02")

	app.addPendingSubscription("sub@example.com", "tok")
	invitation := app.mintInvitationToken()
	crossAuth := strings.TrimSpace(app.post("/admin/resolve", nil).body)

	gh.serve(fullConfig, "sha-one")

	withFaultyDB(app)

	for _, path := range []string{
		"/login",
		"/cross-auth?token=" + crossAuth,
		"/invitation/" + invitation,
		"/admin/monitors/" + loggedID + "/all?after=" + strconv.Itoa(logIDs[1]) + "&date=" + date,
		"/admin/monitors/" + loggedID + "/poll?before=" + strconv.Itoa(logIDs[0]) + "&date=" + date,
		"/admin/monitors/" + monitorID + "/view",
		"/admin/notifications/" + channelID + "/view",
		"/admin/notifications/mail-groups/" + groupID + "/view",
	} {
		sweepFaults(t, app, http.MethodGet, path, nil)
	}

	for path, form := range map[string]url.Values{
		"/login":                             {"username": {"admin"}, "password": {"hunter2hunter2"}},
		"/logout":                            {},
		"/subscribe/email/confirm?token=tok": {},
		"/invitation/" + invitation: {
			"username": {"invited"}, "password": {"hunter2hunter2"},
			"password-confirmation": {"hunter2hunter2"},
		},
		"/admin/monitors/" + monitorID + "/edit": {
			"name": {"API"}, "url": {"https://example.com/health"}, "method": {"GET"},
			"frequency": {"60"}, "timeout": {"5"}, "attempts": {"2"},
		},
		"/admin/notifications/" + channelID + "/edit": {
			"display-name": {"Chat"}, "webhook-url": {"https://hooks.example.com/x"},
		},
		"/admin/notifications/mail-groups/" + groupID + "/edit": {
			"name": {"Ops"}, "members": {"ops@example.com"},
		},
		"/admin/alerts/notifications":                             {"managed-subscriptions": {"on"}},
		"/admin/settings/config-settings/generate-webhook-secret": {},
	} {
		sweepFaults(t, app, http.MethodPost, path, form)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/").status)
}

// The first-boot wizard writes as it goes, so a failure partway has to stop the
// instance advancing rather than leaving it half set up.
func TestSetupSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := newTestApp(t)
	withFaultyDB(app)

	// Each sweep ends with the request that finally ran clean, which is what
	// advances the wizard to the step the next one needs.
	sweepFaults(t, app, http.MethodGet, "/setup/domain", nil)
	sweepFaults(t, app, http.MethodPost, "/setup/skip-domain",
		url.Values{"domain": {"status.example.com"}})
	require.Equal(t, "account", metaSetup.Load())

	sweepFaults(t, app, http.MethodGet, "/setup/account", nil)
	sweepFaults(t, app, http.MethodPost, "/setup/account", url.Values{
		"username": {"admin"}, "password": {"hunter2hunter2"},
		"password-confirmation": {"hunter2hunter2"},
	})
	require.Equal(t, "name", metaSetup.Load())

	sweepFaults(t, app, http.MethodGet, "/setup/name", nil)
	sweepFaults(t, app, http.MethodPost, "/setup/name", url.Values{"name": {"Test Status"}})
	require.Equal(t, "done", metaSetup.Load())
}

func (a *testApp) mintInvitationToken() string {
	a.t.Helper()

	resp := a.post("/admin/settings/users/invite", nil)
	require.Less(a.t, resp.status, 400, resp.body)

	token := a.invitationToken()
	require.NotEmpty(a.t, token)

	return token
}

// The webhook needs its own function rather than a place in the sweep above:
// getting past its gate means turning GitHub sync on, and that sets
// metaConfigFileEnabled, which makes every mutating admin handler answer 400
// before it runs a single statement.
func TestTheWebhookSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)
	app.enableGitHubSync(t)

	// A one-key config on purpose. The sweep re-runs the whole request once per
	// statement, so an apply that touches four tables would put the deep
	// branches -- the ones after applyConfig returns -- hundreds of iterations
	// out of reach.
	gh.serve("general-settings:\n  name: Pushed\n", "sha-one")

	withFaultyDB(app)

	for depth := int64(1); depth <= 120; depth++ {
		failAtStatement(depth)
		resp := app.deliverSignedWebhook(`{"ref":"refs/heads/master"}`)
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"the webhook answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/").status)
}

// The suppression sync is a second transaction the subscribe handler only opens
// when the alert channel is Postmark and statusnook is not managing the list
// itself, so none of it is reachable from the sweep above.
func TestSubscribeSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	pm := newFakePostmark(t)
	app.usePostmarkForAlerts(smtp)

	// Already subscribed, so the handler answers right after the sync instead
	// of going on to send mail.
	app.addPendingSubscription("asking@example.com", "tok")
	require.Equal(t, http.StatusFound, app.post("/subscribe/email/confirm?token=tok", nil).status)

	// One suppressed address with a subscription to deactivate and one without,
	// so the loop runs both of its branches.
	app.addPendingSubscription("bounced@example.com", "tok-bounced")
	require.Equal(t, http.StatusFound,
		app.post("/subscribe/email/confirm?token=tok-bounced", nil).status)
	pm.suppress("bounced@example.com", "stranger@example.com")

	withFaultyDB(app)

	for depth := int64(1); depth <= 40; depth++ {
		// Without this only the first iteration runs the sync; the rest are
		// inside its ten-second window and skip straight past it.
		allowSuppressionSync()

		failAtStatement(depth)
		resp := app.post("/subscribe/email", url.Values{"email": {"asking@example.com"}})
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"subscribe answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/").status)
}

// A fresh address every iteration, so each one runs the whole path rather than
// short-circuiting on the pending subscription the previous one left behind.
func TestSubscribingANewAddressSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	app.useFakeSMTPForAlerts(smtp, true)

	withFaultyDB(app)

	for depth := int64(1); depth <= 40; depth++ {
		failAtStatement(depth)
		resp := app.post("/subscribe/email",
			url.Values{"email": {fmt.Sprintf("sub%d@example.com", depth)}})
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"subscribe answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/").status)
}

// Saving the GitHub half of the config settings talks to the API between its
// reads and its writes, so its second transaction sits deeper than the sweep's
// cap and needs a loop of its own.
func TestConfigSettingsSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)
	newFakeGitHub(t)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	form := url.Values{
		"config-file":           {"on"},
		"github-managed":        {"on"},
		"github-repo-url":       {"https://github.com/example/status"},
		"github-branch":         {"master"},
		"github-config-path":    {"config.yaml"},
		"github-token":          {"ghtoken"},
		"github-webhook-secret": {webhookSecret},
	}

	withFaultyDB(app)

	for depth := int64(1); depth <= 80; depth++ {
		failAtStatement(depth)
		resp := app.post("/admin/settings/config-settings", form)
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"config settings answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/admin/settings/config-settings").status)

	// Both settings pages grow a second half once sync is on -- the stored
	// repository, branch, path and last-applied sha -- which is why they are
	// swept here rather than with the rest of the GETs.
	sweepFaults(t, app, http.MethodGet, "/admin/settings", nil)
	sweepFaults(t, app, http.MethodGet, "/admin/settings/config-settings", nil)
}
