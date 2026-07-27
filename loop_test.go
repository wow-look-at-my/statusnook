package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitFor polls until check passes, so a test does not depend on a loop's tick
// landing at a particular moment.
func waitFor(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	return false
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()

	count := 0
	err := db.QueryRow(query, args...).Scan(&count)
	require.NoError(t, err)

	return count
}

// TestMonitorLoopRecordsChecks runs the real monitor loop against local
// endpoints and checks that results are recorded, which is the product's core
// job. Two monitors are checked in the same pass: a busy or slow monitor used
// to abort the whole pass, starving every monitor after it in the list.
func TestMonitorLoopRecordsChecks(t *testing.T) {
	useTestDBs(t)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer healthy.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer slow.Close()

	tx, err := rwDB.Begin()
	require.NoError(t, err)

	// The slow monitor is created first, so it is checked first.
	for _, monitor := range []struct{ slug, url string }{
		{"slow", slow.URL},
		{"healthy", healthy.URL},
		{"broken", broken.URL},
	} {
		_, err := createMonitor(
			tx, monitor.slug, monitor.slug, monitor.url, "GET", 10, 5, 1,
			nullString(""), nullString(""), nullString(""),
		)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go monitorLoop(ctx, &wg)

	require.True(t, waitFor(t, 10*time.Second, func() bool {
		return countRows(t, `
			select count(*) from monitor_log
			join monitor on monitor.id = monitor_log.monitor_id
			where monitor.slug = 'healthy' and result = 'success'
		`) > 0
	}), "the healthy monitor was never checked")

	require.True(t, waitFor(t, 10*time.Second, func() bool {
		return countRows(t, `
			select count(*) from monitor_log
			join monitor on monitor.id = monitor_log.monitor_id
			where monitor.slug = 'broken' and result = 'error'
		`) > 0
	}), "the failing monitor was never checked")

	assert.Equal(t, 200, lastResponseCode(t, "healthy"))
	assert.Equal(t, 500, lastResponseCode(t, "broken"))

	assert.Positive(t, countRows(t, "select count(*) from monitor_log_last_checked"))

	cancel()
	wg.Wait()
}

// lastResponseCode returns the status code of a monitor's latest check.
func lastResponseCode(t *testing.T, slug string) int {
	t.Helper()

	code := 0
	err := db.QueryRow(`
		select monitor_log.response_code
		from monitor_log_last_checked
		join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
		join monitor on monitor.id = monitor_log_last_checked.monitor_id
		where monitor.slug = ?
	`, slug).Scan(&code)
	require.NoError(t, err)

	return code
}

// TestMonitorLoopSkipsNotificationOnFirstCheck pins the fix for a monitor that
// announced a recovery the first time it was ever checked.
func TestMonitorLoopSkipsNotificationOnFirstCheck(t *testing.T) {
	useTestDBs(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	smtpServer := startFakeSMTP(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)

	monitorID, err := createMonitor(
		tx, "upstream", "Upstream", upstream.URL, "GET", 10, 5, 1,
		nullString(""), nullString(""), nullString(""),
	)
	require.NoError(t, err)

	details, err := json.Marshal(SMTPNotificationDetails{
		Host: strings.Split(smtpServer.addr(), ":")[0],
		Port: smtpPort(t, smtpServer.addr()),
		From: "status@example.com",
	})
	require.NoError(t, err)

	require.NoError(t, createNotification(tx, "mail", "Mail", "smtp", string(details)))

	channel, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)
	require.NoError(t, updateMonitorNotificationChannels(tx, monitorID, []int{channel.ID}))

	mailGroupID, err := createMailGroup(tx, "core", "Core", "")
	require.NoError(t, err)
	require.NoError(t, updateMailGroupMembers(tx, mailGroupID, []string{"admin@example.com"}))
	require.NoError(t, updateMonitorMailGroups(tx, monitorID, []int{mailGroupID}))
	require.NoError(t, tx.Commit())

	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go monitorLoop(ctx, &wg)

	require.True(t, waitFor(t, 5*time.Second, func() bool {
		return countRows(t, "select count(*) from monitor_log where monitor_id = ?", monitorID) > 0
	}), "no check was recorded")

	cancel()
	wg.Wait()

	smtpServer.mu.Lock()
	defer smtpServer.mu.Unlock()
	assert.Empty(
		t, smtpServer.body,
		"a monitor's first successful check must not send a recovery notification",
	)
}

// TestConfigAppliesNotificationChannels covers the notification-channel section
// of applyConfig, including the secret placeholder substitution.
func TestConfigAppliesNotificationChannels(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	const config = `general-settings:
  name: Channels
mail-groups:
  core:
    name: Core
    members:
      - core@example.com
notification-channels:
  mail:
    name: Mail
    type: smtp
    host: smtp.example.com
    port: 587
    username: mailer
    password: plaintext-password
    from: status@example.com
  chat:
    name: Chat
    type: slack
    webhook-url: https://hooks.slack.com/services/T/B/C
services:
  website:
    name: Website
    description: example.com
monitors:
  homepage:
    name: Homepage
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
    notification-channels:
      - mail
    mail-groups:
      - core
alert-notification-settings:
  email-notification-channel: mail
  managed-subscriptions: true
`

	tx, err := rwDB.Begin()
	require.NoError(t, err)

	msgs, err := applyConfig(tx, []byte(config))
	require.NoError(t, err)
	assert.Empty(t, msgs)
	require.NoError(t, tx.Commit())

	read, err := db.Begin()
	require.NoError(t, err)
	defer read.Rollback()

	channels, err := listNotificationChannels(read, listNotificationsOptions{})
	require.NoError(t, err)
	require.Len(t, channels, 2)

	byMonitor, err := listNotificationChannelsByMonitorID(read, 1)
	require.NoError(t, err)
	require.Len(t, byMonitor, 1)
	assert.Equal(t, "Mail", byMonitor[0].Name)

	emails, err := listMailGroupMembersEmailsByMonitorID(read, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"core@example.com"}, emails)

	// A config that refers to a channel that does not exist reports the problem
	// instead of failing the whole apply.
	tx, err = rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	msgs, err = applyConfig(tx, []byte(strings.Replace(
		config, "      - mail\n", "      - missing\n", 1,
	)))
	require.NoError(t, err)
	assert.NotEmpty(t, msgs)
}

// TestConfigRenamesResources covers the rename section, which is the one part
// of the config format that mutates keys rather than values.
func TestConfigRenamesResources(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	const before = `general-settings:
  name: Renames
services:
  website:
    name: Website
    description: example.com
`

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	msgs, err := applyConfig(tx, []byte(before))
	require.NoError(t, err)
	assert.Empty(t, msgs)
	require.NoError(t, tx.Commit())

	const after = `rename:
  services.website: marketing-site
general-settings:
  name: Renames
services:
  marketing-site:
    name: Marketing site
    description: example.com
`

	tx, err = rwDB.Begin()
	require.NoError(t, err)
	msgs, err = applyConfig(tx, []byte(after))
	require.NoError(t, err)
	assert.Empty(t, msgs)
	require.NoError(t, tx.Commit())

	read, err := db.Begin()
	require.NoError(t, err)
	defer read.Rollback()

	services, err := listServices(read)
	require.NoError(t, err)
	require.Len(t, services, 1)
	assert.Equal(t, "marketing-site", services[0].Slug)
	assert.Equal(t, "Marketing site", services[0].Name)
}

// nullString is a small helper for the sql.NullString arguments the monitor
// helpers take.
func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func smtpPort(t *testing.T, addr string) int {
	t.Helper()

	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	number, err := strconv.Atoi(port)
	require.NoError(t, err)

	return number
}
