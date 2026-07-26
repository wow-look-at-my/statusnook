package main

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// testDB returns an empty in-memory database with the production schema.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	require.Nil(t, err)

	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(sqlSchema)
	require.Nil(t, err)

	return db
}

// TestCreateMonitorLogLastCheckedRecords covers the upsert that records a
// monitor's latest check, including the second-write path that decides whether
// an up/down notification fires.
func TestCreateMonitorLogLastCheckedRecords(t *testing.T) {
	db := testDB(t)

	tx, err := db.Begin()
	require.Nil(t, err)

	defer tx.Rollback()

	monitorID, err := createMonitor(tx, "example", "Example", "https://example.com", "GET",
		60, 5, 1, sql.NullString{}, sql.NullString{}, sql.NullString{})
	require.Nil(t, err)

	now := time.Now().UTC().Truncate(time.Second)

	logID, err := createMonitorLog(tx, now, now, 200, sql.NullString{}, 1, "success", monitorID)
	require.Nil(t, err)

	require.NoError(t, createMonitorLogLastChecked(tx, now, monitorID, logID))

	lastChecked, err := getMonitorLogLastChecked(tx, monitorID)
	require.Nil(t, err)

	assert.Equal(t, monitorID, lastChecked.ID)

	require.True(t, lastChecked.ResponseCode.Valid)
	assert.Equal(t, int32(200), lastChecked.ResponseCode.Int32)

	// A second check for the same monitor must replace the row, not fail the
	// unique constraint.
	later := now.Add(time.Minute)
	secondLogID, err := createMonitorLog(
		tx, later, later, 500, sql.NullString{}, 1, "error", monitorID,
	)
	require.Nil(t, err)

	require.NoError(t, createMonitorLogLastChecked(tx, later, monitorID, secondLogID))

	lastChecked, err = getMonitorLogLastChecked(tx, monitorID)
	require.Nil(t, err)

	assert.Equal(t, int32(500), lastChecked.ResponseCode.Int32)

}

// TestSessionExpiry checks that a session older than sessionLifetime stops
// authenticating and gets collected.
func TestSessionExpiry(t *testing.T) {
	db := testDB(t)

	tx, err := db.Begin()
	require.Nil(t, err)

	defer tx.Rollback()

	userID, err := createUser(tx, "admin", "hash")
	require.Nil(t, err)

	require.NoError(t, createSession(tx, "fresh", "csrf-fresh", userID))

	_, _, err = validateSession(tx, "fresh")
	require.Nil(t, err)

	stale := time.Now().UTC().Add(-sessionLifetime - time.Hour)
	_, err = tx.Exec("insert into session(token, csrf_token, user_id, created_at) values(?, ?, ?, ?)", "stale", "csrf-stale", userID, stale)
	require.Nil(t, err)

	_, _, err = validateSession(tx, "stale")
	assert.NotNil(t, err)

	deleted, err := deleteExpiredSessions(tx)
	require.Nil(t, err)

	assert.Equal(t, int64(1), deleted)

	_, _, err = validateSession(tx, "fresh")
	assert.Nil(t, err)

}
