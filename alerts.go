package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type UnsentAlertNotification struct {
	AlertNotificationID int
	Destination         string
	Content             string
	Type                string
	AlertMessageID      int
	AlertTitle          string
	AlertType           string
	AlertSeverity       string
	AlertServices       string
}

func listUnsentAlertNotifications(tx *sql.Tx) ([]UnsentAlertNotification, error) {
	const query = `
		select 
			alert_notification.id, alert_subscription.destination, alert_message.content, 
			alert_subscription.type, alert_message.id, alert.title, alert.type, alert.severity, 
			group_concat(service.name, " • ")
		from alert_notification
		left join alert_subscription on alert_subscription.id = alert_subscription_id
		left join alert_message on alert_message.id = alert_message_id
		left join alert on alert.id = alert_message.alert_id
		left join alert_service on alert_service.alert_id = alert_message.alert_id
		left join service on service.id = alert_service.service_id
		where alert_notification.sent_at is null
		group by alert_notification.id
		order by alert_message.created_at asc
	`

	notifications := []UnsentAlertNotification{}

	rows, err := tx.Query(query)
	if err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		notification := UnsentAlertNotification{}
		err := rows.Scan(
			&notification.AlertNotificationID,
			&notification.Destination,
			&notification.Content,
			&notification.Type,
			&notification.AlertMessageID,
			&notification.AlertTitle,
			&notification.AlertType,
			&notification.AlertSeverity,
			&notification.AlertServices,
		)
		if err != nil {
			return notifications, fmt.Errorf("listUnsentAlertNotifications.Scan: %w", err)
		}

		notifications = append(notifications, notification)
	}

	if err := rows.Err(); err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.RowsErr: %w", err)
	}

	return notifications, nil
}

type StatusnookConfigAlertNotificationSettings struct {
	EmailNotificationChannel string `json:"email-notification-channel,omitempty" yaml:"email-notification-channel,omitempty"`
	ManagedSubscriptions     bool   `json:"managed-subscriptions,omitempty" yaml:"managed-subscriptions,omitempty"`
	SlackClientSecret        string `json:"slack-client-secret,omitempty" yaml:"slack-client-secret,omitempty"`
	SlackInstallURL          string `json:"slack-install-url,omitempty" yaml:"slack-install-url,omitempty"`
}

type AlertSubscription struct {
	ID          int
	Type        string
	Destination string
	Meta        string
	Active      bool
}

func listActiveAlertEmailSubscriptions(tx *sql.Tx) ([]AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and active = true
	`

	var subs []AlertSubscription

	rows, err := tx.Query(query)
	if err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sub AlertSubscription

		err := rows.Scan(
			&sub.ID,
			&sub.Type,
			&sub.Destination,
			&sub.Meta,
			&sub.Active,
		)
		if err != nil {
			return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Scan: %w", err)
		}

		subs = append(subs, sub)
	}

	if err := rows.Err(); err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.RowsErr: %w", err)
	}

	return subs, nil
}

func deleteAlertSubscriptionByMeta(tx *sql.Tx, meta string) error {
	const query = `
		delete from alert_subscription where meta = ?
	`

	_, err := tx.Exec(query, meta)
	if err != nil {
		return fmt.Errorf("deleteAlertSubscriptionByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByMeta(tx *sql.Tx, meta string, active bool) error {
	const query = `
		update alert_subscription set active = ? where meta = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, meta)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByEmail(tx *sql.Tx, email string, active bool) error {
	const query = `
		update alert_subscription set active = ? where destination = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, email)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByEmail.Exec: %w", err)
	}

	return nil
}

func createAlertSubscription(tx *sql.Tx, subscriptionType string, destination string, meta string) error {
	const query = `
		insert into alert_subscription(type, destination, meta) values(?, ?, nullif(?, ''))
		on conflict(type, destination) do update set active = true
	`

	_, err := tx.Exec(query, subscriptionType, destination, meta)
	if err != nil {
		return fmt.Errorf("createAlertSubscription.Exec: %w", err)
	}

	return nil
}

func checkHasRecentPendingEmailAlertSubscription(tx *sql.Tx, email string, now time.Time) (bool, error) {
	const query = `
		select exists(
			select 1 from pending_email_alert_subscription where email = ?
			and created_at > datetime(?, '-10 minutes')
		)
	`

	var hasRecent bool

	err := tx.QueryRow(query, email, now).Scan(&hasRecent)
	if err != nil {
		return hasRecent, fmt.Errorf("checkHasRecentPendingEmailAlertSubscription.Exec: %w", err)
	}

	return hasRecent, nil
}

func createPendingEmailAlertSubscription(tx *sql.Tx, token string, email string, createdAt time.Time) error {
	const query = `
		insert into pending_email_alert_subscription(token, email, created_at)
		values(?, ?, ?)
	`

	_, err := tx.Exec(query, token, email, createdAt)
	if err != nil {
		return fmt.Errorf("createPendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func updatePendingEmailAlertSubscription(tx *sql.Tx, confirmedAt time.Time, token string) error {
	const query = `
		update pending_email_alert_subscription set confirmed_at = ? where token = ?
	`

	_, err := tx.Exec(query, confirmedAt, token)
	if err != nil {
		return fmt.Errorf("updatePendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func getAlertSubscriptionByEmail(tx *sql.Tx, email string) (AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and destination = ?
	`

	var sub AlertSubscription
	err := tx.QueryRow(query, email).Scan(
		&sub.ID,
		&sub.Type,
		&sub.Destination,
		&sub.Meta,
		&sub.Active,
	)
	if err != nil {
		return sub, fmt.Errorf("getAlertSubscriptionByEmail.Scan: %w", err)
	}

	return sub, nil
}

func getPendingEmailAlertSubscriptionEmailByToken(tx *sql.Tx, token string) (string, error) {
	const query = `
		select email from pending_email_alert_subscription
		where token = ? and confirmed_at is null and created_at > ?
	`

	var email string

	err := tx.QueryRow(query, token, time.Now().UTC().Add(-pendingSubscriptionLifetime)).
		Scan(&email)
	if err != nil {
		return email, fmt.Errorf("getPendingEmailAlertSubscriptionEmailByToken.Scan: %w", err)
	}

	return email, nil
}

func getOngoingAlerts(tx *sql.Tx) ([]AlertDetail, error) {
	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where
			ended_at is null
		order by case 
			when severity = 'red' then 1
			when severity = 'amber' then 2
			else 3
		end asc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertDetail{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErr: %w", err)
	}

	alertIDs := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		alertIDs = append(alertIDs, strconv.Itoa(alert.ID))
	}

	messageQuery := fmt.Sprintf(
		`
			select
				id,
				content,
				created_at,
				last_updated_at,
				alert_id
			from
				alert_message
			where
				alert_id in(%s)
			order by created_at desc
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(messageQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query2: %w", err)
	}
	defer rows.Close()

	messages := map[int][]AlertDetailMessage{}

	for rows.Next() {
		alertID := 0
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan2: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		messages[alertID] = append(messages[alertID], message)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErrMessages: %w", err)
	}

	serviceQuery := fmt.Sprintf(
		`
		select
			service.id,
			service.name,
			service.helper_text,
			alert_id
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id in(%s)
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(serviceQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query3: %w", err)
	}
	defer rows.Close()

	services := map[int][]AlertDetailService{}

	for rows.Next() {
		alertID := 0

		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan3: %w", err)
		}

		// services, not messages: the copy-paste initialised the wrong map, so
		// an alert with services but no messages was handed an empty Messages
		// slice and this guard did nothing it was meant to.
		if _, ok := services[alertID]; !ok {
			services[alertID] = []AlertDetailService{}
		}
		services[alertID] = append(services[alertID], service)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErr: %w", err)
	}

	for i, alert := range alerts {
		if _, ok := messages[alert.ID]; ok {
			alerts[i].Messages = messages[alert.ID]
		}
		if _, ok := services[alert.ID]; ok {
			alerts[i].Services = services[alert.ID]
		}
	}

	return alerts, nil
}

func getOldestAlertDate(tx *sql.Tx) (time.Time, error) {
	const query = `
		select 
			created_at
		from
			alert
		order by 
			created_at asc
		limit 1
	`

	date := time.Time{}
	err := tx.QueryRow(query).Scan(&date)
	if err != nil {
		return date, fmt.Errorf("getOldestAlertDate: %w", err)
	}

	return date, nil
}

// periodStart is the first instant of the month being shown; the query bounds
// on [periodStart, next month) rather than strftime("%Y-%m", created_at) = ?,
// which is not sargable and made this public page scan the whole alert table.
func getAlertHistory(tx *sql.Tx, periodStart time.Time) ([]AlertDetail, error) {
	periodEnd := periodStart.AddDate(0, 1, 0)

	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where 
			created_at >= ? and created_at < ?
		order by created_at desc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery, periodStart, periodEnd)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertDetail{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErr: %w", err)
	}

	alertIDs := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		alertIDs = append(alertIDs, strconv.Itoa(alert.ID))
	}

	messageQuery := fmt.Sprintf(
		`
			select
				id,
				content,
				created_at,
				last_updated_at,
				alert_id
			from
				alert_message
			where
				alert_id in(%s)
			order by created_at desc
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(messageQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query2: %w", err)
	}
	defer rows.Close()

	messages := map[int][]AlertDetailMessage{}

	for rows.Next() {
		alertID := 0
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan2: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		messages[alertID] = append(messages[alertID], message)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErrMessages: %w", err)
	}

	serviceQuery := fmt.Sprintf(
		`
		select
			service.id,
			service.name,
			service.helper_text,
			alert_id
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id in(%s)
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(serviceQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query3: %w", err)
	}
	defer rows.Close()

	services := map[int][]AlertDetailService{}

	for rows.Next() {
		alertID := 0

		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan3: %w", err)
		}

		// services, not messages: the copy-paste initialised the wrong map, so
		// an alert with services but no messages was handed an empty Messages
		// slice and this guard did nothing it was meant to.
		if _, ok := services[alertID]; !ok {
			services[alertID] = []AlertDetailService{}
		}
		services[alertID] = append(services[alertID], service)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErr: %w", err)
	}

	for i, alert := range alerts {
		if _, ok := messages[alert.ID]; ok {
			alerts[i].Messages = messages[alert.ID]
		}
		if _, ok := services[alert.ID]; ok {
			alerts[i].Services = services[alert.ID]
		}
	}

	return alerts, nil
}

type AlertListing struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
}
