package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestSettingsRenameTheInstance(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings", url.Values{"name": {"Prod Status"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "Prod Status", metaName.Load())
	require.Contains(t, app.get("/").body, "Prod Status")

	// Under a config file the instance's name comes from the file, so the form
	// must not be able to overwrite it.
	metaConfigFileEnabled.Store(true)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	resp = app.post("/admin/settings", url.Values{"name": {"Renamed Anyway"}})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Equal(t, "Prod Status", metaName.Load())
}

func TestUserEditAndDelete(t *testing.T) {
	app := withTestApp(t)

	second := app.createSecondUser("second")

	resp := app.post("/admin/settings/users/"+strconv.Itoa(second)+"/edit", url.Values{
		"username": {"renamed"},
		"password": {"anotherpassword"},
	})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.usernames(), "renamed")

	// The new password is the one that works now.
	require.True(t, app.credentialsWork("renamed", "anotherpassword"))

	resp = app.delete("/admin/settings/users/" + strconv.Itoa(second))
	require.Less(t, resp.status, 400, resp.body)
	require.NotContains(t, app.usernames(), "renamed")
}

// The webhook secret is generated server-side; the form only ever displays it.
func TestConfigSettingsAndWebhookSecret(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/config-settings/generate-webhook-secret", nil)
	require.Less(t, resp.status, 400, resp.body)

	secret := regexp.MustCompile(`value="([^"]+)"`).FindStringSubmatch(resp.body)
	require.Len(t, secret, 2, "no generated secret in the response: %s", resp.body)
	require.NotEmpty(t, secret[1])

	// Turning the config file on locks the admin forms, which is the whole
	// point of the flag.
	resp = app.post("/admin/settings/config-settings", url.Values{"config-file": {"on"}})
	require.Less(t, resp.status, 400, resp.body)
	require.True(t, metaConfigFileEnabled.Load())
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/services/create", url.Values{"name": {"Nope"}}).status)

	resp = app.post("/admin/settings/config-settings", url.Values{})
	require.Less(t, resp.status, 400, resp.body)
	require.False(t, metaConfigFileEnabled.Load())
}

// The admin config editor runs the same applyConfig the GitHub webhook does.
func TestPostConfigAppliesAndReportsErrors(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/config", url.Values{"config": {`
general-settings:
  name: Test Status
services:
  ingest:
    name: Ingest
    description: the pipeline
monitors:
  api:
    name: API
    url: https://example.com/health
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
`}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, app.serviceNames(), "Ingest")
	require.Contains(t, app.monitorNames(), "API")

	// Malformed YAML comes back as a 400 with the parser's own message, not a
	// 500, and must leave the previous config in place.
	resp = app.post("/admin/settings/config", url.Values{"config": {"services: [oh: no"}})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, app.serviceNames(), "Ingest")

	// A structurally valid file with an unusable value reports the specific
	// problem, and the transaction it half-applied is discarded.
	resp = app.post("/admin/settings/config", url.Values{"config": {`
general-settings:
  name: Test Status
monitors:
  api:
    name: API
    url: not a url
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
`}})
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, app.serviceNames(), "Ingest",
		"a rejected config must not have deleted what the old one declared")
}

func TestSecretsRoundTripThroughTheSettingsForm(t *testing.T) {
	app := withTestApp(t)

	resp := app.post("/admin/settings/secrets",
		url.Values{"action": {"encrypt"}, "input": {"hunter2"}})
	require.Less(t, resp.status, 400, resp.body)

	match := regexp.MustCompile(`value="([^"]+)"`).FindStringSubmatch(resp.body)
	require.Len(t, match, 2, "no ciphertext in the response: %s", resp.body)
	ciphertext := match[1]
	require.Contains(t, ciphertext, ".", "ciphertext carries its nonce after a dot")

	resp = app.post("/admin/settings/secrets",
		url.Values{"action": {"decrypt"}, "input": {ciphertext}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, resp.body, "hunter2")

	// Neither half is optional and no other action exists.
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/settings/secrets", url.Values{"action": {"encrypt"}}).status)
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/settings/secrets", url.Values{"input": {"x"}}).status)
	require.Equal(t, http.StatusBadRequest, app.post("/admin/settings/secrets",
		url.Values{"action": {"sign"}, "input": {"x"}}).status)
}

func TestAlertNotificationSettings(t *testing.T) {
	app := withTestApp(t)

	channelID := app.createSMTPChannel("Mail")

	resp := app.post("/admin/alerts/notifications", url.Values{
		"slack-install-url":         {"https://slack.example.com/install"},
		"slack-client-secret":       {"shh"},
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		"managed-subscriptions":     {"on"},
	})
	require.Less(t, resp.status, 400, resp.body)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	settings, err := getAlertSettings(tx)
	require.NoError(t, err)
	require.Equal(t, "https://slack.example.com/install", settings.SlackInstallURL)
	require.True(t, settings.ManagedSubscriptions)

	got, err := getAlertSMTPNotificationSetting(tx)
	require.NoError(t, err)
	require.Equal(t, channelID, got)

	// A URL that is not a URL is a 400, not a stored broken value.
	require.Equal(t, http.StatusBadRequest, app.post("/admin/alerts/notifications",
		url.Values{"slack-install-url": {"not a url"}}).status)
}

func (a *testApp) createSecondUser(username string) int {
	a.t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2hunter2"), bcrypt.MinCost)
	require.NoError(a.t, err)

	tx, err := rwDB.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	id, err := createUser(tx, username, string(hash))
	require.NoError(a.t, err)
	require.NoError(a.t, tx.Commit())

	return id
}

func (a *testApp) credentialsWork(username string, password string) bool {
	a.t.Helper()

	tx, err := db.Begin()
	require.NoError(a.t, err)
	defer tx.Rollback()

	hash, _, err := getPasswordHash(tx, username)
	require.NoError(a.t, err)

	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func (a *testApp) createSMTPChannel(name string) int {
	a.t.Helper()

	before := a.channelIDs()
	resp := a.post("/admin/notifications/create", url.Values{
		"type":         {"smtp"},
		"display-name": {name},
		"host":         {"smtp.example.com"},
		"port":         {"587"},
		"username":     {"statusnook"},
		"password":     {"shh"},
		"from":         {"status@example.com"},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	return onlyNewID(a.t, before, a.channelIDs())
}

// A config the app itself produced has to be one the app accepts: generateConfig
// feeds the editor, and a round trip that drifted would delete resources.
func TestGeneratedConfigRoundTrips(t *testing.T) {
	app := withTestApp(t)

	app.createService("Ingest", "the pipeline")
	app.createMonitor("API", "https://example.com/health")

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	config, err := generateConfig(tx)
	require.NoError(t, err)
	require.Contains(t, config, "Ingest")

	msgs, err := applyConfig(tx, []byte(config))
	require.NoError(t, err)
	require.Empty(t, msgs, "the app's own config must apply without complaint")

	names, err := listServices(tx)
	require.NoError(t, err)

	found := []string{}
	for _, s := range names {
		found = append(found, s.Name)
	}
	require.Contains(t, found, "Ingest")
	require.NotContains(t, strings.Join(found, ","), "\x00")
}
