package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for monitor and its join tables. SQL in sql/monitors.sql.
//
// request_headers is stored as a JSON blob, so the wrappers decode it into the
// map the rest of the app works with rather than pushing that on every caller.

func decodeRequestHeaders(serialized sql.NullString) (map[string]string, error) {
	headers := map[string]string{}
	if !serialized.Valid {
		return headers, nil
	}

	if err := json.Unmarshal([]byte(serialized.String), &headers); err != nil {
		return headers, err
	}

	return headers, nil
}

func createMonitorLog(
	tx *sql.Tx,
	startedAt time.Time,
	endedAt time.Time,
	responseCode int64,
	errorMessage sql.NullString,
	attempts int,
	result string,
	monitorID int,
) (int, error) {
	id, err := sqlcgen.New(tx).CreateMonitorLog(
		context.Background(),
		sqlcgen.CreateMonitorLogParams{
			StartedAt:    startedAt,
			EndedAt:      endedAt,
			ResponseCode: sql.NullInt64{Int64: responseCode, Valid: responseCode != 0},
			ErrorMessage: errorMessage,
			Attempts:     int64(attempts),
			Result:       result,
			MonitorID:    int64(monitorID),
		},
	)
	if err != nil {
		return int(id), fmt.Errorf("createMonitorLog.Scan: %w", err)
	}

	return int(id), nil
}

func createMonitorLogLastChecked(
	tx *sql.Tx,
	checkedAt time.Time,
	monitorID int,
	monitorLogID int,
) error {
	err := sqlcgen.New(tx).CreateMonitorLogLastChecked(
		context.Background(),
		sqlcgen.CreateMonitorLogLastCheckedParams{
			CheckedAt:    checkedAt,
			MonitorID:    int64(monitorID),
			MonitorLogID: int64(monitorLogID),
		},
	)
	if err != nil {
		return fmt.Errorf("createMonitorLogLastChecked.Exec: %w", err)
	}

	return nil
}

func getMonitorLogLastChecked(tx *sql.Tx, monitorID int) (monitorLogLastChecked, error) {
	var lastChecked monitorLogLastChecked

	row, err := sqlcgen.New(tx).GetMonitorLogLastChecked(context.Background(), int64(monitorID))
	if err != nil {
		// A monitor with no previous check is not an error: monitorLoop reads
		// the zero ID as "first check ever".
		if errors.Is(err, sql.ErrNoRows) {
			return lastChecked, nil
		}
		return lastChecked, fmt.Errorf("getMonitorLogLastChecked.QueryRow: %w", err)
	}

	if row.MonitorID.Valid {
		lastChecked.ID = int(row.MonitorID.Int64)
	}
	lastChecked.CheckedAt = row.CheckedAt
	if row.ResponseCode.Valid {
		lastChecked.ResponseCode = sql.NullInt32{
			Int32: int32(row.ResponseCode.Int64),
			Valid: true,
		}
	}

	return lastChecked, nil
}

func listAllMonitorLogLastChecked(tx *sql.Tx) ([]monitorLogLastChecked, error) {
	allLastChecked := []monitorLogLastChecked{}

	rows, err := sqlcgen.New(tx).ListAllMonitorLogLastChecked(context.Background())
	if err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Query: %w", err)
	}

	for _, r := range rows {
		lastChecked := monitorLogLastChecked{CheckedAt: r.CheckedAt}
		if r.MonitorID.Valid {
			lastChecked.ID = int(r.MonitorID.Int64)
		}
		if r.ResponseCode.Valid {
			lastChecked.ResponseCode = sql.NullInt32{
				Int32: int32(r.ResponseCode.Int64),
				Valid: true,
			}
		}
		allLastChecked = append(allLastChecked, lastChecked)
	}

	return allLastChecked, nil
}

func listMonitors(tx *sql.Tx) ([]Monitor, error) {
	monitorListings := []Monitor{}

	rows, err := sqlcgen.New(tx).ListMonitors(context.Background())
	if err != nil {
		return monitorListings, fmt.Errorf("listMonitors.Query: %w", err)
	}

	for _, r := range rows {
		headers, err := decodeRequestHeaders(r.RequestHeaders)
		if err != nil {
			return monitorListings, fmt.Errorf("listMonitors.Unmarshal: %w", err)
		}

		monitorListings = append(monitorListings, Monitor{
			ID:             int(r.ID),
			Slug:           r.Slug,
			Name:           r.Name,
			URL:            r.Url,
			Method:         r.Method,
			Frequency:      int(r.Frequency),
			Timeout:        int(r.Timeout),
			Attempts:       int(r.Attempts),
			RequestHeaders: headers,
			BodyFormat:     r.BodyFormat,
			Body:           r.Body,
		})
	}

	return monitorListings, nil
}

func getMonitorByID(tx *sql.Tx, id int) (Monitor, error) {
	monitor := Monitor{}

	r, err := sqlcgen.New(tx).GetMonitorByID(context.Background(), int64(id))
	if err != nil {
		return monitor, fmt.Errorf("getMonitorByID.Scan: %w", err)
	}

	headers, err := decodeRequestHeaders(r.RequestHeaders)
	if err != nil {
		return monitor, fmt.Errorf("getMonitorByID.Unmarshal: %w", err)
	}

	return Monitor{
		ID:             int(r.ID),
		Name:           r.Name,
		URL:            r.Url,
		Method:         r.Method,
		Frequency:      int(r.Frequency),
		Timeout:        int(r.Timeout),
		Attempts:       int(r.Attempts),
		RequestHeaders: headers,
		BodyFormat:     r.BodyFormat,
		Body:           r.Body,
	}, nil
}

func editMonitor(
	tx *sql.Tx,
	id int,
	name string,
	url string,
	method string,
	frequency int,
	timeout int,
	attempts int,
	requestHeaders sql.NullString,
	bodyFormat sql.NullString,
	body sql.NullString,
) (int, error) {
	err := sqlcgen.New(tx).EditMonitor(context.Background(), sqlcgen.EditMonitorParams{
		Name:           name,
		Url:            url,
		Method:         method,
		Frequency:      int64(frequency),
		Timeout:        int64(timeout),
		Attempts:       int64(attempts),
		RequestHeaders: requestHeaders,
		BodyFormat:     bodyFormat,
		Body:           body,
		ID:             int64(id),
	})
	if err != nil {
		return id, fmt.Errorf("editMonitor.Exec: %w", err)
	}

	return id, nil
}

func updateMonitorSlug(tx *sql.Tx, old string, new string) (int, error) {
	id, err := sqlcgen.New(tx).UpdateMonitorSlug(
		context.Background(),
		sqlcgen.UpdateMonitorSlugParams{Slug: new, Slug_2: old},
	)
	if err != nil {
		// Wrapped: applyConfig tells ErrNoRows from a constraint error here.
		return int(id), fmt.Errorf("updateMonitorSlug.Exec: %w", err)
	}

	return int(id), nil
}

func deleteMonitorByID(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteMonitorByID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteMonitorByID.Exec: %w", err)
	}

	return nil
}

func createMonitor(
	tx *sql.Tx,
	slug string,
	name string,
	url string,
	method string,
	frequency int,
	timeout int,
	attempts int,
	requestHeaders sql.NullString,
	bodyFormat sql.NullString,
	body sql.NullString,
) (int, error) {
	id, err := sqlcgen.New(tx).CreateMonitor(context.Background(), sqlcgen.CreateMonitorParams{
		Slug:           slug,
		Name:           name,
		Url:            url,
		Method:         method,
		Frequency:      int64(frequency),
		Timeout:        int64(timeout),
		Attempts:       int64(attempts),
		RequestHeaders: requestHeaders,
		BodyFormat:     bodyFormat,
		Body:           body,
	})
	if err != nil {
		return int(id), fmt.Errorf("createMonitor.Scan: %w", err)
	}

	return int(id), nil
}

// The replace-all shape is a delete plus one insert per id. It was a single
// multi-row insert built by string concatenation; a loop over a generated
// statement says the same thing and the sets here are a handful of rows.
func updateMonitorNotificationChannels(tx *sql.Tx, monitorID int, channelIDs []int) error {
	q := sqlcgen.New(tx)

	err := q.DeleteMonitorNotificationChannels(context.Background(), int64(monitorID))
	if err != nil {
		return fmt.Errorf("updateMonitorNotificationChannels.DeleteExec: %w", err)
	}

	for _, channelID := range channelIDs {
		err := q.CreateMonitorNotificationChannel(
			context.Background(),
			sqlcgen.CreateMonitorNotificationChannelParams{
				MonitorID:             int64(monitorID),
				NotificationChannelID: int64(channelID),
			},
		)
		if err != nil {
			return fmt.Errorf("updateMonitorNotificationChannels.InsertExec: %w", err)
		}
	}

	return nil
}

func updateMonitorMailGroups(tx *sql.Tx, monitorID int, mailGroupIDs []int) error {
	q := sqlcgen.New(tx)

	if err := q.DeleteMailGroupMonitors(context.Background(), int64(monitorID)); err != nil {
		return fmt.Errorf("updateMonitorMailGroups.DeleteExec: %w", err)
	}

	for _, mailGroupID := range mailGroupIDs {
		err := q.CreateMailGroupMonitor(
			context.Background(),
			sqlcgen.CreateMailGroupMonitorParams{
				MonitorID:   int64(monitorID),
				MailGroupID: int64(mailGroupID),
			},
		)
		if err != nil {
			return fmt.Errorf("updateMonitorMailGroups.InsertExec: %w", err)
		}
	}

	return nil
}

func listNotificationChannelsByMonitorID(
	tx *sql.Tx,
	monitorID int,
) ([]NotificationChannel, error) {
	notifications := []NotificationChannel{}

	rows, err := sqlcgen.New(tx).ListNotificationChannelsByMonitorID(
		context.Background(),
		int64(monitorID),
	)
	if err != nil {
		return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.Query: %w", err)
	}

	for _, r := range rows {
		channel := NotificationChannel{}
		if r.ID.Valid {
			channel.ID = int(r.ID.Int64)
		}
		channel.Slug = r.Slug.String
		channel.Name = r.Name.String
		channel.Type = r.Type.String

		details, err := decodeNotificationChannelDetails(r.Type.String, r.Details.String)
		if err != nil {
			return notifications, err
		}
		channel.Details = details

		notifications = append(notifications, channel)
	}

	return notifications, nil
}

func listMailGroupIDsByMonitorID(tx *sql.Tx, monitorID int) ([]MailGroupIDs, error) {
	allIds := []MailGroupIDs{}

	rows, err := sqlcgen.New(tx).ListMailGroupIDsByMonitorID(context.Background(), int64(monitorID))
	if err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Query: %w", err)
	}

	for _, r := range rows {
		ids := MailGroupIDs{Slug: r.Slug.String}
		if r.MailGroupID != 0 {
			ids.ID = int(r.MailGroupID)
		}
		allIds = append(allIds, ids)
	}

	return allIds, nil
}

func listMailGroupMembersEmailsByMonitorID(tx *sql.Tx, id int) ([]string, error) {
	emails := []string{}

	rows, err := sqlcgen.New(tx).ListMailGroupMembersEmailsByMonitorID(
		context.Background(),
		int64(id),
	)
	if err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Query: %w", err)
	}

	return append(emails, rows...), nil
}

func getSeverity(tx *sql.Tx) (string, error) {
	severity, err := sqlcgen.New(tx).GetSeverity(context.Background())
	if err != nil {
		return severity, fmt.Errorf("getSeverity.Scan: %w", err)
	}

	return severity, nil
}

func updateSeverity(tx *sql.Tx, severity string) error {
	if err := sqlcgen.New(tx).UpdateSeverity(context.Background(), severity); err != nil {
		return fmt.Errorf("updateSeverity.Exec: %w", err)
	}

	return nil
}

// decodeNotificationChannelDetails turns the stored JSON blob into the typed
// details for the channel's kind. An unknown kind yields nil Details, which is
// what the scan-based version did too.
func decodeNotificationChannelDetails(channelType string, detailsStr string) (any, error) {
	switch channelType {
	case "smtp":
		var details SMTPNotificationDetails
		if err := json.Unmarshal([]byte(detailsStr), &details); err != nil {
			return nil, fmt.Errorf("decodeNotificationChannelDetails.UnmarshalSMTP: %w", err)
		}
		return details, nil
	case "slack":
		var details SlackNotificationDetails
		if err := json.Unmarshal([]byte(detailsStr), &details); err != nil {
			return nil, fmt.Errorf("decodeNotificationChannelDetails.UnmarshalSlack: %w", err)
		}
		return details, nil
	}

	return nil, nil
}
