package main

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// applyConfig is the whole config file in one call: it creates what the file
// names, updates what it changed, and DELETES what it stopped naming. These
// walk a config through a full life so the delete-by-omission half is exercised
// as deliberately as the create half.
const fullConfig = `
general-settings:
  name: Test Status
mail-groups:
  core:
    name: Core
    description: the on-call rota
    members:
      - core1@example.com
      - core2@example.com
notification-channels:
  mail:
    type: smtp
    name: Mail
    host: smtp.example.com
    port: 587
    username: statusnook
    password: shh
    from: status@example.com
  chat:
    type: slack
    name: Chat
    webhook-url: https://hooks.example.com/x
monitors:
  api:
    name: API
    url: https://example.com/health
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
    notification-channels:
      - chat
    mail-groups:
      - core
services:
  website:
    name: Website
    description: example.com
alert-notification-settings:
  email-notification-channel: mail
  managed-subscriptions: true
`

// applyConfig decrypts secret_ values with the instance's key, so the meta row
// holding it has to exist before a config can be applied at all.
func beginConfigTx(t *testing.T) *sql.Tx {
	t.Helper()

	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { tx.Rollback() })

	require.NoError(t, loadMetaState(tx, "false"))

	return tx
}

func applyTestConfig(t *testing.T, tx *sql.Tx, config string) []string {
	t.Helper()

	msgs, err := applyConfig(tx, []byte(config))
	require.NoError(t, err)

	return msgs
}

func TestApplyConfigCreatesEverythingItNames(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	services, err := listServices(tx)
	require.NoError(t, err)
	require.Len(t, services, 1, "the seeded service is not in the file, so it goes")
	require.Equal(t, "website", services[0].Slug)

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.Len(t, channels, 2)

	smtp, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)
	details, ok := smtp.Details.(SMTPNotificationDetails)
	require.True(t, ok)
	require.Equal(t, "smtp.example.com", details.Host)
	require.Equal(t, 587, details.Port)

	groups, err := listMailGroups(tx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "the on-call rota", groups[0].Description)

	members, err := listMailGroupMembersByID(tx, groups[0].ID)
	require.NoError(t, err)
	require.Len(t, members, 2)

	monitors, err := listMonitors(tx)
	require.NoError(t, err)
	require.Len(t, monitors, 1)

	// The references are what make a monitor able to tell anyone it is down.
	monitorChannels, err := listNotificationChannelsByMonitorID(tx, monitors[0].ID)
	require.NoError(t, err)
	require.Len(t, monitorChannels, 1)
	require.Equal(t, "chat", monitorChannels[0].Slug)

	monitorGroups, err := listMailGroupIDsByMonitorID(tx, monitors[0].ID)
	require.NoError(t, err)
	require.Len(t, monitorGroups, 1)
	require.Equal(t, "core", monitorGroups[0].Slug)

	// alert-notification-settings points at a channel by slug, not by id.
	smtpSetting, err := getAlertSMTPNotificationSetting(tx)
	require.NoError(t, err)
	require.Equal(t, smtp.ID, smtpSetting)

	settings, err := getAlertSettings(tx)
	require.NoError(t, err)
	require.True(t, settings.ManagedSubscriptions)
}

// A second apply of a file that no longer mentions something deletes it. That
// is the documented behaviour and the reason a bad push is dangerous.
func TestApplyConfigDeletesWhatTheFileStopsNaming(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	require.Empty(t, applyTestConfig(t, tx, `
general-settings:
  name: Test Status
services:
  website:
    name: Website
    description: example.com
`))

	monitors, err := listMonitors(tx)
	require.NoError(t, err)
	require.Empty(t, monitors)

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.Empty(t, channels)

	groups, err := listMailGroups(tx)
	require.NoError(t, err)
	require.Empty(t, groups)

	services, err := listServices(tx)
	require.NoError(t, err)
	require.Len(t, services, 1, "the one service the file still names survives")
}

// A rename keeps the resource and its history; without the rename block the
// same file would delete the old key and create a new resource.
func TestApplyConfigRenamesInPlace(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	before, err := listServices(tx)
	require.NoError(t, err)
	require.Len(t, before, 1)

	require.Empty(t, applyTestConfig(t, tx, `
general-settings:
  name: Test Status
rename:
  services.website: site
  mail-groups.core: on-call
  notification-channels.chat: slack
  monitors.api: api-health
mail-groups:
  on-call:
    name: Core
    members:
      - core1@example.com
notification-channels:
  slack:
    type: slack
    name: Chat
    webhook-url: https://hooks.example.com/x
monitors:
  api-health:
    name: API
    url: https://example.com/health
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
    notification-channels:
      - slack
services:
  site:
    name: Website
    description: example.com
`))

	after, err := listServices(tx)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, before[0].ID, after[0].ID, "a rename must keep the row, not replace it")
	require.Equal(t, "site", after[0].Slug)

	groups, err := listMailGroups(tx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "on-call", groups[0].Slug)

	_, err = getNotificationChannelBySlug(tx, "slack")
	require.NoError(t, err)
}

// The message list is the only feedback a pushed config gets, so the wording
// and the coverage of it matter more than usual.
func TestApplyConfigReportsEveryKindOfProblem(t *testing.T) {
	tx := beginConfigTx(t)

	msgs := applyTestConfig(t, tx, `
general-settings:
  name: Test Status
mail-groups:
  Bad_Slug:
    name: Nope
    members:
      - core@example.com
  dupes:
    name: Dupes
    members:
      - a@example.com
      - a@example.com
      - not an email
  nameless:
    members:
      - b@example.com
notification-channels:
  wrong-type:
    type: carrier-pigeon
    name: Pigeon
monitors:
  no-url:
    name: No URL
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
  dangling:
    name: Dangling
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
    notification-channels:
      - does-not-exist
    mail-groups:
      - also-missing
services:
  nameless-service:
    description: no name
`)

	joined := ""
	for _, m := range msgs {
		joined += m + "\n"
	}

	require.Contains(t, joined, "mail-groups.Bad_Slug")
	require.Contains(t, joined, "email is invalid")
	require.Contains(t, joined, "member is duplicated")
	require.Contains(t, joined, "mail-groups.nameless: name is required")
	require.Contains(t, joined, "notification-channels.wrong-type")
	require.Contains(t, joined, "monitors.no-url")
	require.Contains(t, joined, "does-not-exist")
	require.Contains(t, joined, "also-missing")
	require.Contains(t, joined, "services.nameless-service")
}

// A rename whose source does not exist, and one whose target is taken, are
// different failures and both have to be reported rather than silently applied.
func TestApplyConfigRejectsImpossibleRenames(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	msgs := applyTestConfig(t, tx, `
general-settings:
  name: Test Status
rename:
  services.nothing-here: site
services:
  site:
    name: Website
`)
	require.NotEmpty(t, msgs, "renaming a key that does not exist must be reported")
}
