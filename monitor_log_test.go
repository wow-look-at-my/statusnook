package main

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

// openTestDB opens a database with the pure-Go sqlite driver the app registers.
func openTestDB(dsn string) (*sql.DB, error) {
	return sql.Open("sqlite", dsn)
}

// testDB returns an empty in-memory database with the production schema.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
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

// TestDatabasePragmas guards the settings the DSN carries. They are easy to
// lose silently: the pure-Go driver ignores the C driver's pragma syntax, and
// without foreign_keys every "on delete cascade" in the schema stops working.
func TestDatabasePragmas(t *testing.T) {
	useTestDBs(t)

	for name, handle := range map[string]*sql.DB{"read": db, "write": rwDB} {
		t.Run(name, func(t *testing.T) {
			foreignKeys := 0
			require.NoError(t, handle.QueryRow("pragma foreign_keys").Scan(&foreignKeys))
			assert.Equal(t, 1, foreignKeys, "foreign keys must be enforced")

			journalMode := ""
			require.NoError(t, handle.QueryRow("pragma journal_mode").Scan(&journalMode))
			assert.Equal(t, "wal", strings.ToLower(journalMode))

			busyTimeout := 0
			require.NoError(t, handle.QueryRow("pragma busy_timeout").Scan(&busyTimeout))
			assert.Equal(t, 5000, busyTimeout)
		})
	}
}

// TestCascadeDeletes proves the foreign keys actually cascade, which is how the
// app cleans up a deleted monitor's logs and a deleted alert's messages.
func TestCascadeDeletes(t *testing.T) {
	useTestDBs(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)

	monitorID, err := createMonitor(tx, "example", "Example", "https://example.com", "GET",
		60, 5, 1, sql.NullString{}, sql.NullString{}, sql.NullString{})
	require.NoError(t, err)

	now := time.Now().UTC()
	logID, err := createMonitorLog(tx, now, now, 200, sql.NullString{}, 1, "success", monitorID)
	require.NoError(t, err)
	require.NoError(t, createMonitorLogLastChecked(tx, now, monitorID, logID))
	require.NoError(t, deleteMonitorByID(tx, monitorID))
	require.NoError(t, tx.Commit())

	for _, table := range []string{"monitor", "monitor_log", "monitor_log_last_checked"} {
		count := 0
		require.NoError(t, db.QueryRow("select count(*) from "+table).Scan(&count))
		assert.Zero(t, count, "%s rows survived the monitor delete", table)
	}
}
