package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for notification_channel, mail_group and mail_group_member. SQL
// in sql/notifications.sql.

// The details column is JSON: a struct on the way out, already-marshalled
// bytes or a string on the way in. Anything else is a caller bug, not a value
// to coerce.
func notificationDetailsString(details any) (string, error) {
	switch v := details.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		return "", fmt.Errorf("notification details must be string or []byte, got %T", details)
	}
}

// Fills Details from the stored JSON. An unknown type leaves it nil, matching
// the check constraint's two known types.
func decodeNotificationChannel(row sqlcgen.NotificationChannel) (NotificationChannel, error) {
	channel := NotificationChannel{
		ID:   int(row.ID),
		Slug: row.Slug,
		Name: row.Name,
		Type: row.Type,
	}

	switch row.Type {
	case "smtp":
		var details SMTPNotificationDetails
		if err := json.Unmarshal([]byte(row.Details), &details); err != nil {
			return channel, fmt.Errorf("decodeNotificationChannel.UnmarshalSMTP: %w", err)
		}
		channel.Details = details
	case "slack":
		var details SlackNotificationDetails
		if err := json.Unmarshal([]byte(row.Details), &details); err != nil {
			return channel, fmt.Errorf("decodeNotificationChannel.UnmarshalSlack: %w", err)
		}
		channel.Details = details
	}

	return channel, nil
}

func listNotificationChannels(
	tx *sql.Tx,
	options listNotificationsOptions,
) ([]NotificationChannel, error) {
	var channels []NotificationChannel

	// nil means no filter: the query is `?1 is null or type = ?1`.
	var typeFilter any
	if options.Type != "" {
		typeFilter = options.Type
	}

	rows, err := sqlcgen.New(tx).ListNotificationChannels(context.Background(), typeFilter)
	if err != nil {
		return channels, fmt.Errorf("listNotificationChannels.Query: %w", err)
	}

	for _, r := range rows {
		channel, err := decodeNotificationChannel(r)
		if err != nil {
			return channels, fmt.Errorf("listNotificationChannels: %w", err)
		}

		channels = append(channels, channel)
	}

	return channels, nil
}

func createNotification(
	tx *sql.Tx,
	slug string,
	name string,
	notificationType string,
	details string,
) error {
	err := sqlcgen.New(tx).CreateNotificationChannel(
		context.Background(),
		sqlcgen.CreateNotificationChannelParams{
			Slug:    slug,
			Name:    name,
			Type:    notificationType,
			Details: details,
		},
	)
	if err != nil {
		return fmt.Errorf("createNotification.Exec: %w", err)
	}

	return nil
}

func getNotificationChannelByID(tx *sql.Tx, id int) (NotificationChannel, error) {
	row, err := sqlcgen.New(tx).GetNotificationChannelByID(context.Background(), int64(id))
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("getNotificationChannelByID.QueryRow: %w", err)
	}

	return decodeNotificationChannel(row)
}

func getNotificationChannelBySlug(tx *sql.Tx, slug string) (NotificationChannel, error) {
	row, err := sqlcgen.New(tx).GetNotificationChannelBySlug(context.Background(), slug)
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("getNotificationChannelBySlug.QueryRow: %w", err)
	}

	return decodeNotificationChannel(row)
}

func editNotificationChannel(tx *sql.Tx, channel NotificationChannel) error {
	details, err := notificationDetailsString(channel.Details)
	if err != nil {
		return fmt.Errorf("editNotificationChannel: %w", err)
	}

	err = sqlcgen.New(tx).EditNotificationChannel(
		context.Background(),
		sqlcgen.EditNotificationChannelParams{
			Name:    channel.Name,
			Details: details,
			ID:      int64(channel.ID),
		},
	)
	if err != nil {
		return fmt.Errorf("editNotificationChannel.Exec: %w", err)
	}

	return nil
}

func updateNotificationChannelSlug(tx *sql.Tx, old string, new string) (int, error) {
	id, err := sqlcgen.New(tx).UpdateNotificationChannelSlug(
		context.Background(),
		sqlcgen.UpdateNotificationChannelSlugParams{Slug: new, Slug_2: old},
	)
	if err != nil {
		// Wrapped, not replaced: applyConfig tells sql.ErrNoRows (rename source
		// missing) from a constraint error (target slug taken) through this.
		return int(id), fmt.Errorf("updateNotificationChannelSlug.QueryRow: %w", err)
	}

	return int(id), nil
}

func deleteNotificationChannelByID(tx *sql.Tx, id int) error {
	err := sqlcgen.New(tx).DeleteNotificationChannelByID(context.Background(), int64(id))
	if err != nil {
		return fmt.Errorf("deleteNotificationChannelByID.Exec: %w", err)
	}

	return nil
}

func listMailGroups(tx *sql.Tx) ([]MailGroup, error) {
	mailGroups := []MailGroup{}

	rows, err := sqlcgen.New(tx).ListMailGroups(context.Background())
	if err != nil {
		return mailGroups, fmt.Errorf("listMailGroups.Query: %w", err)
	}

	for _, r := range rows {
		mailGroups = append(mailGroups, MailGroup{
			ID:          int(r.ID),
			Slug:        r.Slug,
			Name:        r.Name,
			Description: r.Description.String,
		})
	}

	return mailGroups, nil
}

func listMailGroupMembersByID(tx *sql.Tx, id int) ([]MailGroupMember, error) {
	members := []MailGroupMember{}

	rows, err := sqlcgen.New(tx).ListMailGroupMembersByID(context.Background(), int64(id))
	if err != nil {
		return members, fmt.Errorf("listMailGroupMembersByID.Query: %w", err)
	}

	for _, r := range rows {
		members = append(members, MailGroupMember{
			ID:           int(r.ID),
			EmailAddress: r.EmailAddress,
		})
	}

	return members, nil
}

func getMailGroupByID(tx *sql.Tx, id int) (MailGroup, error) {
	row, err := sqlcgen.New(tx).GetMailGroupByID(context.Background(), int64(id))
	if err != nil {
		return MailGroup{}, fmt.Errorf("getMailGroupByID.QueryRow: %w", err)
	}

	// Slug is deliberately not selected, matching the query this replaced.
	return MailGroup{
		ID:          int(row.ID),
		Name:        row.Name,
		Description: row.Description.String,
	}, nil
}

func createMailGroup(tx *sql.Tx, slug string, name string, description string) (int, error) {
	id, err := sqlcgen.New(tx).CreateMailGroup(context.Background(), sqlcgen.CreateMailGroupParams{
		Slug:        slug,
		Name:        name,
		Description: sql.NullString{String: description, Valid: true},
	})
	if err != nil {
		return int(id), fmt.Errorf("createMailGroup.QueryRow: %w", err)
	}

	return int(id), nil
}

func updateMailGroup(tx *sql.Tx, id int, name string, description string) error {
	err := sqlcgen.New(tx).UpdateMailGroup(context.Background(), sqlcgen.UpdateMailGroupParams{
		Name:        name,
		Description: sql.NullString{String: description, Valid: true},
		ID:          int64(id),
	})
	if err != nil {
		return fmt.Errorf("updateMailGroup.Exec: %w", err)
	}

	return nil
}

func updateMailGroupSlug(tx *sql.Tx, old string, new string) (int, error) {
	id, err := sqlcgen.New(tx).UpdateMailGroupSlug(
		context.Background(),
		sqlcgen.UpdateMailGroupSlugParams{Slug: new, Slug_2: old},
	)
	if err != nil {
		return int(id), fmt.Errorf("updateMailGroupSlug.QueryRow: %w", err)
	}

	return int(id), nil
}

func updateMailGroupMembers(tx *sql.Tx, id int, members []string) error {
	q := sqlcgen.New(tx)

	err := q.DeleteMailGroupMembersByGroupID(context.Background(), int64(id))
	if err != nil {
		return fmt.Errorf("updateMailGroupMembers.ExecDelete: %w", err)
	}

	for _, member := range members {
		err := q.AddMailGroupMember(context.Background(), sqlcgen.AddMailGroupMemberParams{
			EmailAddress: member,
			MailGroupID:  int64(id),
		})
		if err != nil {
			return fmt.Errorf("updateMailGroupMembers.ExecInsert: %w", err)
		}
	}

	return nil
}

func deleteMailGroupByID(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteMailGroupByID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteMailGroupByID.Exec: %w", err)
	}

	return nil
}
