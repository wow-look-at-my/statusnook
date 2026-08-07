package main

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// schema.sql seeds a welcome alert and a "website" service, so every test here
// starts by clearing them.
func emptyAlertTables(t *testing.T, tx *sql.Tx) {
	t.Helper()

	for _, alert := range mustListAlerts(t, tx) {
		require.NoError(t, deleteAlertByID(tx, alert.ID))
	}

	services, err := listServices(tx)
	require.NoError(t, err)
	for _, s := range services {
		require.NoError(t, deleteServiceByID(tx, s.ID))
	}
}

func mustListAlerts(t *testing.T, tx *sql.Tx) []AlertListing {
	t.Helper()

	alerts, err := listAlerts(tx)
	require.NoError(t, err)

	return alerts
}

func mustCreateService(t *testing.T, tx *sql.Tx, slug string) int {
	t.Helper()

	require.NoError(t, createService(tx, slug, slug, ""))

	services, err := listServices(tx)
	require.NoError(t, err)
	for _, s := range services {
		if s.Slug == slug {
			return s.ID
		}
	}

	t.Fatalf("service %q not found after createService", slug)

	return 0
}

// getOngoingAlerts, getAlertHistory and getAlertByID all fetch their messages
// and services as separate flat queries and stitch them together by alert_id.
// The stitching is what this covers: the right message on the right alert, and
// an alert with no messages left alone.
func TestAlertDetailQueriesStitchByAlertID(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	emptyAlertTables(t, tx)

	webID := mustCreateService(t, tx, "web")
	apiID := mustCreateService(t, tx, "api")

	firstID, err := createAlert(tx, "Disk pressure", []int{webID}, "incident", "red")
	require.NoError(t, err)
	secondID, err := createAlert(tx, "Planned upgrade", []int{webID, apiID}, "maintenance", "amber")
	require.NoError(t, err)
	quietID, err := createAlert(tx, "No messages yet", []int{apiID}, "incident", "blue")
	require.NoError(t, err)

	_, err = createAlertMessage(tx, firstID, "investigating")
	require.NoError(t, err)
	secondMessageID, err := createAlertMessage(tx, secondID, "starting now")
	require.NoError(t, err)

	ongoing, err := getOngoingAlerts(tx)
	require.NoError(t, err)
	require.Len(t, ongoing, 3)

	// red before amber before everything else, per the query's severity case.
	require.Equal(t, "Disk pressure", ongoing[0].Title)
	require.Equal(t, "Planned upgrade", ongoing[1].Title)

	byID := map[int]AlertDetail{}
	for _, a := range ongoing {
		byID[a.ID] = a
	}

	require.Len(t, byID[firstID].Messages, 1)
	require.Equal(t, "investigating", byID[firstID].Messages[0].Content)
	require.Len(t, byID[firstID].Services, 1)
	require.Equal(t, "web", byID[firstID].Services[0].Name)

	require.Len(t, byID[secondID].Services, 2)
	require.Empty(t, byID[quietID].Messages)
	require.Len(t, byID[quietID].Services, 1)

	detail, err := getAlertByID(tx, secondID)
	require.NoError(t, err)
	require.Equal(t, "Planned upgrade", detail.Title)
	require.Len(t, detail.Messages, 1)
	require.Len(t, detail.Services, 2)
	require.Nil(t, detail.EndedAt)

	require.NoError(t, editAlertMessage(tx, secondID, secondMessageID, "under way"))
	detail, err = getAlertByID(tx, secondID)
	require.NoError(t, err)
	require.Equal(t, "under way", detail.Messages[0].Content)
	require.NotNil(t, detail.Messages[0].LastUpdatedAt)

	// editAlert replaces the service set outright.
	require.NoError(t, editAlert(tx, secondID, "Upgrade", []int{apiID}, "maintenance", "amber"))
	detail, err = getAlertByID(tx, secondID)
	require.NoError(t, err)
	require.Equal(t, "Upgrade", detail.Title)
	require.Len(t, detail.Services, 1)
	require.Equal(t, "api", detail.Services[0].Name)

	require.NoError(t, deleteAlertMessageByID(tx, secondID, secondMessageID))
	detail, err = getAlertByID(tx, secondID)
	require.NoError(t, err)
	require.Empty(t, detail.Messages)

	require.NoError(t, resolveAlert(tx, firstID))
	ongoing, err = getOngoingAlerts(tx)
	require.NoError(t, err)
	require.Len(t, ongoing, 2)

	detail, err = getAlertByID(tx, firstID)
	require.NoError(t, err)
	require.NotNil(t, detail.EndedAt)

	require.NoError(t, unresolveAlert(tx, firstID))
	ongoing, err = getOngoingAlerts(tx)
	require.NoError(t, err)
	require.Len(t, ongoing, 3)

	require.NoError(t, deleteAlertByID(tx, quietID))
	require.Len(t, mustListAlerts(t, tx), 2)
}

// getAlertHistory bounds on [periodStart, next month), and its message and
// service queries have to bound on the same window or a neighbouring month's
// rows land on the wrong page.
func TestGetAlertHistoryBoundsThePeriod(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	emptyAlertTables(t, tx)

	webID := mustCreateService(t, tx, "web")
	insideID, err := createAlert(tx, "Inside", []int{webID}, "incident", "red")
	require.NoError(t, err)
	_, err = createAlertMessage(tx, insideID, "in period")
	require.NoError(t, err)

	now := time.Now().UTC()
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	history, err := getAlertHistory(tx, thisMonth)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Len(t, history[0].Messages, 1)
	require.Len(t, history[0].Services, 1)

	// The month before holds nothing, messages and services included.
	previous, err := getAlertHistory(tx, thisMonth.AddDate(0, -1, 0))
	require.NoError(t, err)
	require.Empty(t, previous)

	oldest, err := getOldestAlertDate(tx)
	require.NoError(t, err)
	require.False(t, oldest.IsZero())
}

// The notification queue joins an alert's services with group_concat. Deleting
// the last service an alert names leaves that concat null, which is why the
// query coalesces -- without it the sender fails on every queued row forever.
func TestListUnsentAlertNotificationsSurvivesServicelessAlert(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	emptyAlertTables(t, tx)

	webID := mustCreateService(t, tx, "web")
	alertID, err := createAlert(tx, "Outage", []int{webID}, "incident", "red")
	require.NoError(t, err)

	require.NoError(t, createAlertSubscription(tx, "email", "sub@example.com", ""))

	messageID, err := createAlertMessage(tx, alertID, "we are on it")
	require.NoError(t, err)
	require.NoError(t, createAlertMessageNotifications(tx, time.Now().UTC(), messageID))

	unsent, err := listUnsentAlertNotifications(tx)
	require.NoError(t, err)
	require.Len(t, unsent, 1)
	require.Equal(t, "sub@example.com", unsent[0].Destination)
	require.Equal(t, "we are on it", unsent[0].Content)
	require.Equal(t, "web", unsent[0].AlertServices)

	// alert_service cascades from service, so this leaves the alert with none.
	require.NoError(t, deleteServiceByID(tx, webID))

	unsent, err = listUnsentAlertNotifications(tx)
	require.NoError(t, err)
	require.Len(t, unsent, 1)
	require.Empty(t, unsent[0].AlertServices)

	require.NoError(t, updateAlertSentAtByID(tx, time.Now().UTC(), []int{unsent[0].AlertNotificationID}))
	unsent, err = listUnsentAlertNotifications(tx)
	require.NoError(t, err)
	require.Empty(t, unsent)
}

func TestAlertSubscriptionQueriesRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	require.NoError(t, createAlertSubscription(tx, "email", "a@example.com", ""))
	require.NoError(t, createAlertSubscription(tx, "slack", "https://hooks/x", "T123"))

	active, err := listActiveAlertEmailSubscriptions(tx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "a@example.com", active[0].Destination)
	require.True(t, active[0].Active)
	// nullif(?, '') stored the empty meta as null, which reads back as "".
	require.Empty(t, active[0].Meta)

	require.NoError(t, updateEmailAlertSubscriptionActiveByEmail(tx, "a@example.com", false))
	active, err = listActiveAlertEmailSubscriptions(tx)
	require.NoError(t, err)
	require.Empty(t, active)

	sub, err := getAlertSubscriptionByEmail(tx, "a@example.com")
	require.NoError(t, err)
	require.False(t, sub.Active)

	// Re-subscribing is an upsert that flips active back on rather than a
	// second row, which the unique(type, destination) index would reject.
	require.NoError(t, createAlertSubscription(tx, "email", "a@example.com", ""))
	sub, err = getAlertSubscriptionByEmail(tx, "a@example.com")
	require.NoError(t, err)
	require.True(t, sub.Active)

	_, err = getAlertSubscriptionByEmail(tx, "nobody@example.com")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	require.NoError(t, updateEmailAlertSubscriptionActiveByMeta(tx, "T123", false))
	require.NoError(t, deleteAlertSubscriptionByMeta(tx, "T123"))

	// The slack row is gone; the email one, whose meta is null, is untouched.
	_, err = getAlertSubscriptionByEmail(tx, "a@example.com")
	require.NoError(t, err)
}

func TestPendingEmailAlertSubscriptionQueries(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	now := time.Now().UTC()

	recent, err := checkHasRecentPendingEmailAlertSubscription(tx, "p@example.com", now)
	require.NoError(t, err)
	require.False(t, recent)

	require.NoError(t, createPendingEmailAlertSubscription(tx, "tok", "p@example.com", now))

	recent, err = checkHasRecentPendingEmailAlertSubscription(tx, "p@example.com", now)
	require.NoError(t, err)
	require.True(t, recent, "a request one second ago is inside the 10 minute window")

	email, err := getPendingEmailAlertSubscriptionEmailByToken(tx, "tok")
	require.NoError(t, err)
	require.Equal(t, "p@example.com", email)

	// Confirming retires the token: a replayed confirmation must not resolve.
	require.NoError(t, updatePendingEmailAlertSubscription(tx, now, "tok"))
	_, err = getPendingEmailAlertSubscriptionEmailByToken(tx, "tok")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	// An hour old is outside the window even though the row is still there.
	require.NoError(t, createPendingEmailAlertSubscription(
		tx, "old", "q@example.com", now.Add(-time.Hour),
	))
	recent, err = checkHasRecentPendingEmailAlertSubscription(tx, "q@example.com", now)
	require.NoError(t, err)
	require.False(t, recent)
}

func TestAlertSettingsRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	require.NoError(t, updateAlertSettings(tx, "https://slack/install", "shh", true))

	settings, err := getAlertSettings(tx)
	require.NoError(t, err)
	require.Equal(t, "https://slack/install", settings.SlackInstallURL)
	require.Equal(t, "shh", settings.SlackClientSecret)
	require.True(t, settings.ManagedSubscriptions)

	// Upsert, not insert: schema.sql already seeds managed-subscriptions.
	require.NoError(t, updateAlertSettings(tx, "", "", false))
	settings, err = getAlertSettings(tx)
	require.NoError(t, err)
	require.False(t, settings.ManagedSubscriptions)
	require.Empty(t, settings.SlackInstallURL)

	require.NoError(t, createNotification(tx, "mail", "Mail", "smtp", "{}"))
	channel, err := getNotificationChannelBySlug(tx, "mail")
	require.NoError(t, err)

	require.NoError(t, updateAlertSMTPNotificationSetting(tx, channel.ID))
	got, err := getAlertSMTPNotificationSetting(tx)
	require.NoError(t, err)
	require.Equal(t, channel.ID, got)

	// Zero means "none": the row is removed rather than pointed at id 0, which
	// no notification_channel can have.
	require.NoError(t, updateAlertSMTPNotificationSetting(tx, 0))
	_, err = getAlertSMTPNotificationSetting(tx)
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)
}
