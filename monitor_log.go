package main

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

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
	const query = `
		insert into 
			monitor_log(started_at, ended_at, response_code, error_message, 
				attempts, result, monitor_id)
			values(?, ?, ?, ?, ?, ?, ?)
		returning id
	`

	var id int

	err := tx.QueryRow(
		query,
		startedAt,
		endedAt,
		responseCode,
		errorMessage,
		attempts,
		result,
		monitorID,
	).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createMonitorLog.Exec: %w", err)
	}

	return id, nil
}

func createMonitorLogLastChecked(
	tx *sql.Tx,
	startedAt time.Time,
	monitorID int,
	monitorLogID int,
) error {
	const query = `
		insert into monitor_log_last_checked(checked_at, monitor_id, monitor_log_id)
			values(?, ?, ?) 
		on conflict(monitor_id) do update set 
			checked_at = excluded.checked_at,
			monitor_log_id = excluded.monitor_log_id
	`

	_, err := tx.Exec(
		query,
		startedAt,
		monitorID,
		monitorLogID,
	)
	if err != nil {
		return fmt.Errorf("createMonitorLogLastChecked.Exec: %w", err)
	}

	return nil
}

type monitorLogLastChecked struct {
	ID           int
	CheckedAt    time.Time
	ResponseCode sql.NullInt32
}

func getMonitorLogLastChecked(
	tx *sql.Tx,
	monitorID int,
) (monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
		where monitor_log_last_checked.monitor_id = ?
	`

	var lastChecked monitorLogLastChecked

	err := tx.QueryRow(query, monitorID).Scan(
		&lastChecked.ID,
		&lastChecked.CheckedAt,
		&lastChecked.ResponseCode,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lastChecked, nil
		}
		return lastChecked, fmt.Errorf("getMonitorLogLastChecked.QueryRow: %w", err)
	}

	return lastChecked, nil
}

func listAllMonitorLogLastChecked(tx *sql.Tx) ([]monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
	`

	var allLastChecked []monitorLogLastChecked

	rows, err := tx.Query(query)
	if err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var lastChecked monitorLogLastChecked

		err := rows.Scan(
			&lastChecked.ID,
			&lastChecked.CheckedAt,
			&lastChecked.ResponseCode,
		)
		if err != nil {
			return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Scan: %w", err)
		}

		allLastChecked = append(allLastChecked, lastChecked)
	}

	return allLastChecked, nil
}
