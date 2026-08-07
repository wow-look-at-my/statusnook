package main

import (
	"net/url"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// A config file lives in a git repository, so anything sensitive in it is
// stored as a secret_ ciphertext the instance decrypts on apply. If that
// decryption ever stopped happening the instance would try to authenticate
// with the ciphertext itself.
func TestConfigSecretsAreDecryptedOnApply(t *testing.T) {
	app := withTestApp(t)

	password := app.encryptSecret("smtp-password")
	webhook := app.encryptSecret("https://hooks.example.com/x")

	resp := app.post("/admin/settings/config", url.Values{"config": {`
general-settings:
  name: Test Status
notification-channels:
  mail:
    type: smtp
    name: Mail
    host: smtp.example.com
    port: 587
    username: statusnook
    password: ` + password + `
    from: status@example.com
  chat:
    type: slack
    name: Chat
    webhook-url: ` + webhook + `
`}})
	require.Less(t, resp.status, 400, resp.body)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	mail, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)
	smtp, ok := mail.Details.(SMTPNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "smtp-password", smtp.Password, "the ciphertext reached the database")

	chat, err := getNotificationChannelBySlug(tx, "chat")
	require.NoError(t, err)
	slack, ok := chat.Details.(SlackNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "https://hooks.example.com/x", slack.WebhookURL)
}

// Every channel type has a fixed set of properties, and a key that is not one
// of them is a typo -- silently ignoring it would leave the operator believing
// they had configured something.
func TestConfigRejectsUnknownChannelProperties(t *testing.T) {
	tx := beginConfigTx(t)

	msgs := applyTestConfig(t, tx, `
general-settings:
  name: Test Status
notification-channels:
  chat:
    type: slack
    name: Chat
    webhook-url: https://hooks.example.com/x
    host: smtp.example.com
  mail:
    type: smtp
    name: Mail
    host: smtp.example.com
    port: 587
    username: statusnook
    password: shh
    from: status@example.com
    webhook-url: https://hooks.example.com/y
`)

	joined := ""
	for _, m := range msgs {
		joined += m + "\n"
	}

	require.Contains(t, joined, "host is an invalid property for a slack notification channel")
	require.Contains(t, joined, "webhook-url is an unknown property for an SMTP notification channel")
}

// Postmark is the one SMTP host with extra required fields, because its two
// message streams decide whether a send is transactional or broadcast.
func TestConfigRequiresPostmarkStreams(t *testing.T) {
	tx := beginConfigTx(t)

	postmark := func(extra string) string {
		return `
general-settings:
  name: Test Status
notification-channels:
  mail:
    type: smtp
    name: Mail
    host: smtp.postmarkapp.com
    port: 587
    username: statusnook
    password: shh
    from: status@example.com
` + extra
	}

	require.NotEmpty(t, applyTestConfig(t, tx, postmark("")))

	require.Empty(t, applyTestConfig(t, tx, postmark(`    misc:
      pm-transactional: outbound
      pm-broadcast: broadcast
`)))

	channel, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)
	details, ok := channel.Details.(SMTPNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "outbound", details.Misc["pm-transactional"])
}

// A monitor's method, body and headers are what actually goes on the wire, so
// the config has to carry all three through unchanged.
func TestConfigCarriesMonitorRequestDetails(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, `
general-settings:
  name: Test Status
monitors:
  ingest:
    name: Ingest
    url: https://example.com/ingest
    method: POST
    frequency: 60
    timeout: 5
    attempts: 1
    headers:
      Content-Type: application/json
      X-Trace: on
    body: '{"ping":1}'
`))

	monitors, err := listMonitors(tx)
	require.NoError(t, err)
	require.Len(t, monitors, 1)
	require.Equal(t, "POST", monitors[0].Method)
	require.Equal(t, "application/json", monitors[0].RequestHeaders["Content-Type"])
	require.Equal(t, `{"ping":1}`, monitors[0].Body.String)

	// A method the app does not send is a message, not a stored value.
	msgs := applyTestConfig(t, tx, `
general-settings:
  name: Test Status
monitors:
  ingest:
    name: Ingest
    url: https://example.com/ingest
    method: TRACE
    frequency: 60
    timeout: 5
    attempts: 1
`)
	require.NotEmpty(t, msgs)
}

// alert-notification-settings names a channel by slug, so a slug that is not
// there has to be reported rather than leaving alerts silently unsendable.
func TestConfigReportsAMissingAlertNotificationChannel(t *testing.T) {
	tx := beginConfigTx(t)

	msgs := applyTestConfig(t, tx, `
general-settings:
  name: Test Status
alert-notification-settings:
  email-notification-channel: nowhere
`)
	require.NotEmpty(t, msgs)
}

func (a *testApp) encryptSecret(plaintext string) string {
	a.t.Helper()

	resp := a.post("/admin/settings/secrets",
		url.Values{"action": {"encrypt"}, "input": {plaintext}})
	require.Less(a.t, resp.status, 400, resp.body)

	match := regexp.MustCompile(`value="([^"]+)"`).FindStringSubmatch(resp.body)
	require.Len(a.t, match, 2, "no ciphertext in the response: %s", resp.body)

	// postSecret already prefixes the ciphertext with secret_.
	return match[1]
}
