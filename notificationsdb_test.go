package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// The notification and mail-group helpers moved onto sqlc. Three things have to
// survive the move beyond the happy path: the type filter, the two errors
// applyConfig branches on when renaming, and the JSON round-trip through the
// details column.
func TestNotificationChannelQueriesRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	smtp, err := json.Marshal(SMTPNotificationDetails{
		Host:     "smtp.example.com",
		Port:     587,
		Username: "statusnook",
		From:     "status@example.com",
		Headers:  map[string]string{"X-Mailer": "statusnook"},
	})
	require.NoError(t, err)

	slack, err := json.Marshal(SlackNotificationDetails{WebhookURL: "https://hooks.example.com/x"})
	require.NoError(t, err)

	require.NoError(t, createNotification(tx, "mail", "Mail", "smtp", string(smtp)))
	require.NoError(t, createNotification(tx, "chat", "Chat", "slack", string(slack)))

	all, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.Len(t, all, 2)

	// An empty Type must not filter; a set one must.
	onlySMTP, err := listNotificationChannels(tx, listNotificationsOptions{Type: "smtp"})
	require.NoError(t, err)
	require.Len(t, onlySMTP, 1)
	require.Equal(t, "mail", onlySMTP[0].Slug)

	details, ok := onlySMTP[0].Details.(SMTPNotificationDetails)
	require.True(t, ok, "smtp details decoded to %T", onlySMTP[0].Details)
	require.Equal(t, 587, details.Port)
	require.Equal(t, "statusnook", details.Headers["X-Mailer"])

	bySlug, err := getNotificationChannelBySlug(tx, "chat")
	require.NoError(t, err)
	slackDetails, ok := bySlug.Details.(SlackNotificationDetails)
	require.True(t, ok, "slack details decoded to %T", bySlug.Details)
	require.Equal(t, "https://hooks.example.com/x", slackDetails.WebhookURL)

	byID, err := getNotificationChannelByID(tx, bySlug.ID)
	require.NoError(t, err)
	require.Equal(t, "Chat", byID.Name)

	// editNotificationChannel takes Details as an interface: the config path
	// hands it a string, the form handler already-marshalled bytes.
	require.NoError(t, editNotificationChannel(tx, NotificationChannel{
		ID:      bySlug.ID,
		Name:    "Chatter",
		Details: slack,
	}))
	byID, err = getNotificationChannelByID(tx, bySlug.ID)
	require.NoError(t, err)
	require.Equal(t, "Chatter", byID.Name)

	require.Error(t, editNotificationChannel(tx, NotificationChannel{
		ID:      bySlug.ID,
		Details: SlackNotificationDetails{},
	}), "an unmarshalled struct is a caller bug and must not be coerced")

	id, err := updateNotificationChannelSlug(tx, "chat", "slack")
	require.NoError(t, err)
	require.Equal(t, bySlug.ID, id)

	_, err = updateNotificationChannelSlug(tx, "nope", "whatever")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	_, err = updateNotificationChannelSlug(tx, "slack", "mail")
	var sqliteErr sqlite3.Error
	require.True(t, errors.As(err, &sqliteErr), "want sqlite3.Error, got %v", err)
	require.Equal(t, sqlite3.ErrConstraint, sqliteErr.Code)

	require.NoError(t, deleteNotificationChannelByID(tx, bySlug.ID))
	all, err = listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestMailGroupQueriesRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	id, err := createMailGroup(tx, "ops", "Ops", "on call")
	require.NoError(t, err)
	otherID, err := createMailGroup(tx, "devs", "Devs", "")
	require.NoError(t, err)

	groups, err := listMailGroups(tx)
	require.NoError(t, err)
	require.Len(t, groups, 2)

	group, err := getMailGroupByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "Ops", group.Name)
	require.Equal(t, "on call", group.Description)

	require.NoError(t, updateMailGroup(tx, id, "Operations", "rota"))
	group, err = getMailGroupByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "Operations", group.Name)
	require.Equal(t, "rota", group.Description)

	// Membership is replace-all, and the duplicate must be absorbed by the
	// unique constraint rather than failing the write.
	require.NoError(t, updateMailGroupMembers(tx, id, []string{"a@example.com", "b@example.com"}))
	require.NoError(t, updateMailGroupMembers(tx, id, []string{"b@example.com", "b@example.com"}))

	members, err := listMailGroupMembersByID(tx, id)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "b@example.com", members[0].EmailAddress)

	renamed, err := updateMailGroupSlug(tx, "ops", "operations")
	require.NoError(t, err)
	require.Equal(t, id, renamed)

	_, err = updateMailGroupSlug(tx, "nope", "whatever")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	_, err = updateMailGroupSlug(tx, "operations", "devs")
	var sqliteErr sqlite3.Error
	require.True(t, errors.As(err, &sqliteErr), "want sqlite3.Error, got %v", err)
	require.Equal(t, sqlite3.ErrConstraint, sqliteErr.Code)

	require.NoError(t, deleteMailGroupByID(tx, otherID))
	groups, err = listMailGroups(tx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
}
