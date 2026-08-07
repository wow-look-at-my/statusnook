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
