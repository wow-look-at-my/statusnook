package main

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The monitor helpers moved onto sqlc. monitorLoop leans on two behaviours
// here that a rewrite could quietly lose: request_headers round-tripping
// through its JSON blob, and getMonitorLogLastChecked returning a zero-value
// struct with no error when a monitor has never been checked -- that zero ID
// is what makes the first check count as a transition and alert.
func TestMonitorQueries(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	id, err := createMonitor(tx, "api", "API", "https://example.com", "GET",
		60, 5, 2,
		sql.NullString{String: `{"Range":"bytes=0-0"}`, Valid: true},
		sql.NullString{}, sql.NullString{})
	require.NoError(t, err)
	require.NotZero(t, id)

	monitors, err := listMonitors(tx)
	require.NoError(t, err)
	require.Len(t, monitors, 1)
	require.Equal(t, "bytes=0-0", monitors[0].RequestHeaders["Range"],
		"request_headers did not survive the JSON round trip")
	require.Equal(t, 60, monitors[0].Frequency)

	got, err := getMonitorByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "API", got.Name)
	require.Equal(t, "bytes=0-0", got.RequestHeaders["Range"])

	// Never checked: zero struct, no error.
	last, err := getMonitorLogLastChecked(tx, id)
	require.NoError(t, err, "an unchecked monitor must not be an error")
	require.Zero(t, last.ID, "unchecked monitor must report a zero ID")

	now := time.Now().UTC()
	logID, err := createMonitorLog(tx, now, now, 200, sql.NullString{}, 1, "success", id)
	require.NoError(t, err)
	require.NoError(t, createMonitorLogLastChecked(tx, now, id, logID))

	last, err = getMonitorLogLastChecked(tx, id)
	require.NoError(t, err)
	require.Equal(t, id, last.ID)
	require.True(t, last.ResponseCode.Valid)
	require.Equal(t, int32(200), last.ResponseCode.Int32)

	all, err := listAllMonitorLogLastChecked(tx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, id, all[0].ID)

	// The join-table replace-all shape.
	require.NoError(t, updateMonitorNotificationChannels(tx, id, []int{}))
	require.NoError(t, updateMonitorMailGroups(tx, id, []int{}))

	_, err = editMonitor(tx, id, "API v2", "https://example.com/v2", "POST",
		30, 10, 3, sql.NullString{}, sql.NullString{}, sql.NullString{})
	require.NoError(t, err)

	got, err = getMonitorByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "API v2", got.Name)
	require.Equal(t, 30, got.Frequency)
	require.Empty(t, got.RequestHeaders, "cleared headers should decode to an empty map")

	require.NoError(t, deleteMonitorByID(tx, id))
	monitors, err = listMonitors(tx)
	require.NoError(t, err)
	require.Empty(t, monitors)
}

// listMonitorLogs was a hand-assembled query string with optional cursor
// clauses. On sqlc the clauses are always present and a nil parameter means
// unbounded, so what needs proving is that the bounds still bound: the day
// window, the after/before cursors, and limit 0 meaning no limit.
func TestListMonitorLogsBounds(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	monitorID, err := createMonitor(tx, "api", "API", "https://example.com", "GET",
		60, 5, 1, sql.NullString{}, sql.NullString{}, sql.NullString{})
	require.NoError(t, err)

	day := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)

	ids := []int{}
	for i := range 5 {
		id, err := createMonitorLog(tx, day.Add(time.Duration(i)*time.Hour),
			day.Add(time.Duration(i)*time.Hour), 200, sql.NullString{}, 1, "success", monitorID)
		require.NoError(t, err)
		ids = append(ids, id)
	}

	// The day before and the day after must not leak in.
	_, err = createMonitorLog(tx, day.Add(-time.Hour), day.Add(-time.Hour),
		200, sql.NullString{}, 1, "success", monitorID)
	require.NoError(t, err)
	_, err = createMonitorLog(tx, day.Add(24*time.Hour), day.Add(24*time.Hour),
		200, sql.NullString{}, 1, "success", monitorID)
	require.NoError(t, err)

	// limit 0 is "no limit", not "no rows".
	logs, err := listMonitorLogs(tx, monitorID, 0, 0, 0, day)
	require.NoError(t, err)
	require.Len(t, logs, 5)
	require.Equal(t, ids[4], logs[0].ID, "newest first")

	logs, err = listMonitorLogs(tx, monitorID, 2, 0, 0, day)
	require.NoError(t, err)
	require.Len(t, logs, 2)

	// after is an exclusive upper bound on id: the next page down.
	logs, err = listMonitorLogs(tx, monitorID, 0, ids[2], 0, day)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	require.Equal(t, ids[1], logs[0].ID)

	// before is an inclusive lower bound: everything at or above it.
	logs, err = listMonitorLogs(tx, monitorID, 0, 0, ids[3], day)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	require.Equal(t, ids[4], logs[0].ID)

	// Another monitor's rows are never included.
	otherID, err := createMonitor(tx, "web", "Web", "https://example.org", "GET",
		60, 5, 1, sql.NullString{}, sql.NullString{}, sql.NullString{})
	require.NoError(t, err)
	logs, err = listMonitorLogs(tx, otherID, 0, 0, 0, day)
	require.NoError(t, err)
	require.Empty(t, logs)
}
