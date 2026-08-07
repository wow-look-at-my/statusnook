package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for alert_subscription, pending_email_alert_subscription and the
// alert notification queue. SQL in sql/alerts.sql.

// alert_subscription.active is `int not null default true`, so sqlc types it
// as an integer while the app models it as a bool.
func boolToInt64(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

func listUnsentAlertNotifications(tx *sql.Tx) ([]UnsentAlertNotification, error) {
	notifications := []UnsentAlertNotification{}

	rows, err := sqlcgen.New(tx).ListUnsentAlertNotifications(context.Background())
	if err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.Query: %w", err)
	}

	// Every joined column is nullable to sqlc because the joins are LEFT, but
	// the two join keys are non-null foreign keys, so a row always matches.
	for _, r := range rows {
		notifications = append(notifications, UnsentAlertNotification{
			AlertNotificationID: int(r.ID),
			Destination:         r.Destination.String,
			Content:             r.Content.String,
			Type:                r.Type.String,
			AlertMessageID:      int(r.AlertMessageID.Int64),
			AlertTitle:          r.Title.String,
			AlertType:           r.AlertType.String,
			AlertSeverity:       r.Severity.String,
			AlertServices:       r.AlertServices,
		})
	}

	return notifications, nil
}

func listActiveAlertEmailSubscriptions(tx *sql.Tx) ([]AlertSubscription, error) {
	var subs []AlertSubscription

	rows, err := sqlcgen.New(tx).ListActiveAlertEmailSubscriptions(context.Background())
	if err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Query: %w", err)
	}

	for _, r := range rows {
		subs = append(subs, AlertSubscription{
			ID:          int(r.ID),
			Type:        r.Type,
			Destination: r.Destination,
			Meta:        r.Meta.String,
			Active:      r.Active != 0,
		})
	}

	return subs, nil
}

func deleteAlertSubscriptionByMeta(tx *sql.Tx, meta string) error {
	err := sqlcgen.New(tx).DeleteAlertSubscriptionByMeta(
		context.Background(),
		sql.NullString{String: meta, Valid: true},
	)
	if err != nil {
		return fmt.Errorf("deleteAlertSubscriptionByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByMeta(tx *sql.Tx, meta string, active bool) error {
	err := sqlcgen.New(tx).UpdateEmailAlertSubscriptionActiveByMeta(
		context.Background(),
		sqlcgen.UpdateEmailAlertSubscriptionActiveByMetaParams{
			Active: boolToInt64(active),
			Meta:   sql.NullString{String: meta, Valid: true},
		},
	)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByEmail(tx *sql.Tx, email string, active bool) error {
	err := sqlcgen.New(tx).UpdateEmailAlertSubscriptionActiveByEmail(
		context.Background(),
		sqlcgen.UpdateEmailAlertSubscriptionActiveByEmailParams{
			Active:      boolToInt64(active),
			Destination: email,
		},
	)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByEmail.Exec: %w", err)
	}

	return nil
}

func createAlertSubscription(
	tx *sql.Tx,
	subscriptionType string,
	destination string,
	meta string,
) error {
	err := sqlcgen.New(tx).CreateAlertSubscription(
		context.Background(),
		sqlcgen.CreateAlertSubscriptionParams{
			Type:        subscriptionType,
			Destination: destination,
			// nullif(?, '') in the query: an empty meta is stored as null so it
			// does not collide with other subscriptions carrying no meta.
			NULLIF: meta,
		},
	)
	if err != nil {
		return fmt.Errorf("createAlertSubscription.Exec: %w", err)
	}

	return nil
}

func checkHasRecentPendingEmailAlertSubscription(
	tx *sql.Tx,
	email string,
	now time.Time,
) (bool, error) {
	found, err := sqlcgen.New(tx).CheckHasRecentPendingEmailAlertSubscription(
		context.Background(),
		sqlcgen.CheckHasRecentPendingEmailAlertSubscriptionParams{Email: email, Datetime: now},
	)
	if err != nil {
		return false, fmt.Errorf("checkHasRecentPendingEmailAlertSubscription.Exec: %w", err)
	}

	return found != 0, nil
}

func createPendingEmailAlertSubscription(
	tx *sql.Tx,
	token string,
	email string,
	createdAt time.Time,
) error {
	err := sqlcgen.New(tx).CreatePendingEmailAlertSubscription(
		context.Background(),
		sqlcgen.CreatePendingEmailAlertSubscriptionParams{
			Token:     token,
			Email:     email,
			CreatedAt: createdAt,
		},
	)
	if err != nil {
		return fmt.Errorf("createPendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func updatePendingEmailAlertSubscription(tx *sql.Tx, confirmedAt time.Time, token string) error {
	err := sqlcgen.New(tx).UpdatePendingEmailAlertSubscription(
		context.Background(),
		sqlcgen.UpdatePendingEmailAlertSubscriptionParams{
			ConfirmedAt: sql.NullTime{Time: confirmedAt, Valid: true},
			Token:       token,
		},
	)
	if err != nil {
		return fmt.Errorf("updatePendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func getAlertSubscriptionByEmail(tx *sql.Tx, email string) (AlertSubscription, error) {
	row, err := sqlcgen.New(tx).GetAlertSubscriptionByEmail(context.Background(), email)
	if err != nil {
		// Wrapped: callers use errors.Is to read sql.ErrNoRows as "not
		// subscribed", which is the ordinary path.
		return AlertSubscription{}, fmt.Errorf("getAlertSubscriptionByEmail.Scan: %w", err)
	}

	return AlertSubscription{
		ID:          int(row.ID),
		Type:        row.Type,
		Destination: row.Destination,
		Meta:        row.Meta.String,
		Active:      row.Active != 0,
	}, nil
}

func getPendingEmailAlertSubscriptionEmailByToken(tx *sql.Tx, token string) (string, error) {
	email, err := sqlcgen.New(tx).GetPendingEmailAlertSubscriptionEmailByToken(
		context.Background(),
		sqlcgen.GetPendingEmailAlertSubscriptionEmailByTokenParams{
			Token:     token,
			CreatedAt: time.Now().UTC().Add(-pendingSubscriptionLifetime),
		},
	)
	if err != nil {
		return email, fmt.Errorf("getPendingEmailAlertSubscriptionEmailByToken.Scan: %w", err)
	}

	return email, nil
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func nullTimePtr(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}

	return &nt.Time
}

// The three service queries select the same columns, so their generated row
// types are convertible to this one and the grouping below is written once.
type alertServiceRow = sqlcgen.ListOngoingAlertServicesRow

func alertDetailsFromRows(rows []sqlcgen.Alert) []AlertDetail {
	alerts := make([]AlertDetail, 0, len(rows))

	for _, r := range rows {
		alerts = append(alerts, AlertDetail{
			ID:        int(r.ID),
			Title:     r.Title,
			AlertType: r.Type,
			Severity:  r.Severity,
			CreatedAt: timePtr(r.CreatedAt),
			EndedAt:   nullTimePtr(r.EndedAt),
		})
	}

	return alerts
}

// Messages and services arrive as flat lists covering every alert in the view,
// so they are grouped by alert_id here rather than queried per alert.
func attachAlertDetails(
	alerts []AlertDetail,
	messageRows []sqlcgen.AlertMessage,
	serviceRows []alertServiceRow,
) {
	messages := map[int][]AlertDetailMessage{}
	for _, r := range messageRows {
		messages[int(r.AlertID)] = append(messages[int(r.AlertID)], AlertDetailMessage{
			ID:            int(r.ID),
			Content:       r.Content,
			CreatedAt:     timePtr(r.CreatedAt),
			LastUpdatedAt: nullTimePtr(r.LastUpdatedAt),
		})
	}

	// The join to service is LEFT, so a row whose service has been deleted
	// carries nulls rather than disappearing.
	services := map[int][]AlertDetailService{}
	for _, r := range serviceRows {
		services[int(r.AlertID)] = append(services[int(r.AlertID)], AlertDetailService{
			ID:         int(r.ID.Int64),
			Name:       r.Name.String,
			HelperText: r.HelperText.String,
		})
	}

	for i, alert := range alerts {
		if m, ok := messages[alert.ID]; ok {
			alerts[i].Messages = m
		}
		if s, ok := services[alert.ID]; ok {
			alerts[i].Services = s
		}
	}
}

func listAlerts(tx *sql.Tx) ([]AlertListing, error) {
	alerts := []AlertListing{}

	rows, err := sqlcgen.New(tx).ListAlerts(context.Background())
	if err != nil {
		return alerts, fmt.Errorf("listAlerts.Query: %w", err)
	}

	for _, r := range rows {
		alerts = append(alerts, AlertListing{
			ID:        int(r.ID),
			Title:     r.Title,
			AlertType: r.Type,
			Severity:  r.Severity,
			CreatedAt: timePtr(r.CreatedAt),
			EndedAt:   nullTimePtr(r.EndedAt),
		})
	}

	return alerts, nil
}

func getOngoingAlerts(tx *sql.Tx) ([]AlertDetail, error) {
	q := sqlcgen.New(tx)

	alertRows, err := q.ListOngoingAlerts(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getOngoingAlerts.Query: %w", err)
	}

	messageRows, err := q.ListOngoingAlertMessages(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getOngoingAlerts.Query2: %w", err)
	}

	serviceRows, err := q.ListOngoingAlertServices(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getOngoingAlerts.Query3: %w", err)
	}

	alerts := alertDetailsFromRows(alertRows)
	attachAlertDetails(alerts, messageRows, serviceRows)

	return alerts, nil
}

// periodStart is the first instant of the month being shown; the queries bound
// on [periodStart, next month) rather than strftime("%Y-%m", created_at) = ?,
// which is not sargable and made this public page scan the whole alert table.
func getAlertHistory(tx *sql.Tx, periodStart time.Time) ([]AlertDetail, error) {
	periodEnd := periodStart.AddDate(0, 1, 0)
	q := sqlcgen.New(tx)

	alertRows, err := q.ListAlertsInPeriod(context.Background(), sqlcgen.ListAlertsInPeriodParams{
		CreatedAt:   periodStart,
		CreatedAt_2: periodEnd,
	})
	if err != nil {
		return nil, fmt.Errorf("getAlertHistory.Query: %w", err)
	}

	messageRows, err := q.ListAlertMessagesInPeriod(
		context.Background(),
		sqlcgen.ListAlertMessagesInPeriodParams{CreatedAt: periodStart, CreatedAt_2: periodEnd},
	)
	if err != nil {
		return nil, fmt.Errorf("getAlertHistory.Query2: %w", err)
	}

	serviceRows, err := q.ListAlertServicesInPeriod(
		context.Background(),
		sqlcgen.ListAlertServicesInPeriodParams{CreatedAt: periodStart, CreatedAt_2: periodEnd},
	)
	if err != nil {
		return nil, fmt.Errorf("getAlertHistory.Query3: %w", err)
	}

	alerts := alertDetailsFromRows(alertRows)
	converted := make([]alertServiceRow, 0, len(serviceRows))
	for _, r := range serviceRows {
		converted = append(converted, alertServiceRow(r))
	}
	attachAlertDetails(alerts, messageRows, converted)

	return alerts, nil
}

func getAlertByID(tx *sql.Tx, id int) (AlertDetail, error) {
	q := sqlcgen.New(tx)

	alertRow, err := q.GetAlertByID(context.Background(), int64(id))
	if err != nil {
		return AlertDetail{}, fmt.Errorf("getAlertByID.QueryRow: %w", err)
	}

	messageRows, err := q.ListAlertMessagesByAlertID(context.Background(), int64(id))
	if err != nil {
		return AlertDetail{}, fmt.Errorf("getAlertByID.Query: %w", err)
	}

	serviceRows, err := q.ListAlertServicesByAlertID(context.Background(), int64(id))
	if err != nil {
		return AlertDetail{}, fmt.Errorf("getAlertByID.Query2: %w", err)
	}

	alerts := alertDetailsFromRows([]sqlcgen.Alert{alertRow})
	converted := make([]alertServiceRow, 0, len(serviceRows))
	for _, r := range serviceRows {
		converted = append(converted, alertServiceRow(r))
	}
	attachAlertDetails(alerts, messageRows, converted)

	return alerts[0], nil
}

func getOldestAlertDate(tx *sql.Tx) (time.Time, error) {
	date, err := sqlcgen.New(tx).GetOldestAlertDate(context.Background())
	if err != nil {
		return date, fmt.Errorf("getOldestAlertDate: %w", err)
	}

	return date, nil
}

func getAlertSettings(tx *sql.Tx) (AlertSettings, error) {
	settings := AlertSettings{}

	rows, err := sqlcgen.New(tx).ListAlertSettings(context.Background())
	if err != nil {
		return settings, fmt.Errorf("getAlertSettings.Query: %w", err)
	}

	for _, r := range rows {
		switch r.Name {
		case "slack-install-url":
			settings.SlackInstallURL = r.Value
		case "slack-client-secret":
			settings.SlackClientSecret = r.Value
		case "managed-subscriptions":
			parsed, err := strconv.ParseBool(r.Value)
			if err != nil {
				return settings, fmt.Errorf("getAlertSettings.ParseBool: %w", err)
			}
			settings.ManagedSubscriptions = parsed
		}
	}

	return settings, nil
}

func getAlertSMTPNotificationSetting(tx *sql.Tx) (int, error) {
	id, err := sqlcgen.New(tx).GetAlertSMTPNotificationSetting(context.Background())
	if err != nil {
		return int(id), fmt.Errorf("getAlertSMTPNotificationSetting.QueryRow: %w", err)
	}

	return int(id), nil
}

func updateAlertSMTPNotificationSetting(tx *sql.Tx, notificationID int) error {
	q := sqlcgen.New(tx)

	if err := q.DeleteAlertSMTPNotificationSetting(context.Background()); err != nil {
		return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecDelete: %w", err)
	}

	// Zero means "no channel": the row stays deleted rather than pointing at a
	// notification_channel id that cannot exist.
	if notificationID == 0 {
		return nil
	}

	err := q.CreateAlertSMTPNotificationSetting(context.Background(), int64(notificationID))
	if err != nil {
		return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecInsert: %w", err)
	}

	return nil
}

func deleteAlertByID(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteAlertByID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteAlertByID.Exec: %w", err)
	}

	return nil
}

func createAlertMessageNotifications(tx *sql.Tx, createdAt time.Time, alertMessageID int) error {
	err := sqlcgen.New(tx).CreateAlertMessageNotifications(
		context.Background(),
		sqlcgen.CreateAlertMessageNotificationsParams{
			CreatedAt:      createdAt,
			AlertMessageID: int64(alertMessageID),
		},
	)
	if err != nil {
		return fmt.Errorf("createAlertMessageNotifications.Exec: %w", err)
	}

	return nil
}

func updateAlertSentAtByID(tx *sql.Tx, now time.Time, ids []int) error {
	q := sqlcgen.New(tx)

	for _, id := range ids {
		err := q.UpdateAlertSentAtByID(context.Background(), sqlcgen.UpdateAlertSentAtByIDParams{
			SentAt: sql.NullTime{Time: now, Valid: true},
			ID:     int64(id),
		})
		if err != nil {
			return fmt.Errorf("updateAlertSentAtByID.Exec: %w", err)
		}
	}

	return nil
}

func resolveAlert(tx *sql.Tx, id int) error {
	err := sqlcgen.New(tx).ResolveAlert(context.Background(), sqlcgen.ResolveAlertParams{
		EndedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
		ID:      int64(id),
	})
	if err != nil {
		return fmt.Errorf("resolveAlert.Exec: %w", err)
	}

	return nil
}

func unresolveAlert(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).UnresolveAlert(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("unresolveAlert.Exec: %w", err)
	}

	return nil
}

func createAlert(
	tx *sql.Tx,
	title string,
	services []int,
	alertType string,
	severity string,
) (int, error) {
	q := sqlcgen.New(tx)

	alertID, err := q.CreateAlert(context.Background(), sqlcgen.CreateAlertParams{
		Title:     title,
		Type:      alertType,
		Severity:  severity,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return int(alertID), fmt.Errorf("createAlert.Scan: %w", err)
	}

	if err := addAlertServices(q, int(alertID), services); err != nil {
		return int(alertID), fmt.Errorf("createAlert.Exec: %w", err)
	}

	return int(alertID), nil
}

func addAlertServices(q *sqlcgen.Queries, alertID int, services []int) error {
	for _, serviceID := range services {
		err := q.AddAlertService(context.Background(), sqlcgen.AddAlertServiceParams{
			AlertID:   int64(alertID),
			ServiceID: int64(serviceID),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func editAlert(
	tx *sql.Tx,
	id int,
	title string,
	services []int,
	alertType string,
	severity string,
) error {
	q := sqlcgen.New(tx)

	err := q.EditAlert(context.Background(), sqlcgen.EditAlertParams{
		Title:    title,
		Type:     alertType,
		Severity: severity,
		ID:       int64(id),
	})
	if err != nil {
		return fmt.Errorf("editAlert.Exec: %w", err)
	}

	if err := q.DeleteAlertServicesByAlertID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("editAlert.Exec2: %w", err)
	}

	if err := addAlertServices(q, id, services); err != nil {
		return fmt.Errorf("editAlert.Exec3: %w", err)
	}

	return nil
}

func createAlertMessage(tx *sql.Tx, alertID int, content string) (int, error) {
	id, err := sqlcgen.New(tx).CreateAlertMessage(
		context.Background(),
		sqlcgen.CreateAlertMessageParams{
			Content:   content,
			CreatedAt: time.Now().UTC(),
			AlertID:   int64(alertID),
		},
	)
	if err != nil {
		return int(id), fmt.Errorf("createAlertMessage.Scan: %w", err)
	}

	return int(id), nil
}

func deleteAlertMessageByID(tx *sql.Tx, alertID int, messageID int) error {
	err := sqlcgen.New(tx).DeleteAlertMessageByID(
		context.Background(),
		sqlcgen.DeleteAlertMessageByIDParams{AlertID: int64(alertID), ID: int64(messageID)},
	)
	if err != nil {
		return fmt.Errorf("deleteAlertMessageByID.Exec: %w", err)
	}

	return nil
}

func editAlertMessage(tx *sql.Tx, alertID int, messageID int, content string) error {
	err := sqlcgen.New(tx).EditAlertMessage(context.Background(), sqlcgen.EditAlertMessageParams{
		Content:       content,
		LastUpdatedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
		AlertID:       int64(alertID),
		ID:            int64(messageID),
	})
	if err != nil {
		return fmt.Errorf("editAlertMessage.Exec: %w", err)
	}

	return nil
}

func updateAlertSettings(
	tx *sql.Tx,
	slackInstallURL string,
	slackClientSecret string,
	managedSubscriptions bool,
) error {
	q := sqlcgen.New(tx)

	settings := []sqlcgen.UpsertAlertSettingParams{
		{Name: "slack-install-url", Value: slackInstallURL},
		{Name: "slack-client-secret", Value: slackClientSecret},
		// getAlertSettings reads this back with strconv.ParseBool, which takes
		// "true"/"false" as well as the "1"/"0" older rows hold.
		{Name: "managed-subscriptions", Value: strconv.FormatBool(managedSubscriptions)},
	}

	for _, setting := range settings {
		if err := q.UpsertAlertSetting(context.Background(), setting); err != nil {
			return fmt.Errorf("updateAlertSettings.Exec: %w", err)
		}
	}

	return nil
}
