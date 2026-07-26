package main

import (
	"database/sql"
	"testing"
	"time"
)

// testDB returns an empty in-memory database with the production schema.
func testDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(sqlSchema); err != nil {
		t.Fatalf("schema: %s", err)
	}

	return db
}

// TestCreateMonitorLogLastCheckedRecords covers the bug where the upsert was
// called with one argument more than the statement had placeholders. Every
// check failed, which rolled back the monitor log written in the same
// transaction, so no check ever landed and no up/down notification was sent.
func TestCreateMonitorLogLastCheckedRecords(t *testing.T) {
	db := testDB(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %s", err)
	}
	defer tx.Rollback()

	monitorID, err := createMonitor(tx, "example", "Example", "https://example.com", "GET",
		60, 5, 1, sql.NullString{}, sql.NullString{}, sql.NullString{})
	if err != nil {
		t.Fatalf("createMonitor: %s", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	logID, err := createMonitorLog(tx, now, now, 200, sql.NullString{}, 1, "success", monitorID)
	if err != nil {
		t.Fatalf("createMonitorLog: %s", err)
	}

	if err := createMonitorLogLastChecked(tx, now, monitorID, logID); err != nil {
		t.Fatalf("createMonitorLogLastChecked: %s", err)
	}

	lastChecked, err := getMonitorLogLastChecked(tx, monitorID)
	if err != nil {
		t.Fatalf("getMonitorLogLastChecked: %s", err)
	}

	if lastChecked.ID != monitorID {
		t.Errorf("last checked monitor id = %d, want %d", lastChecked.ID, monitorID)
	}
	if !lastChecked.ResponseCode.Valid || lastChecked.ResponseCode.Int32 != 200 {
		t.Errorf("last checked response code = %+v, want 200", lastChecked.ResponseCode)
	}

	// A second check for the same monitor must replace the row, not fail the
	// unique constraint.
	later := now.Add(time.Minute)
	secondLogID, err := createMonitorLog(
		tx, later, later, 500, sql.NullString{}, 1, "error", monitorID,
	)
	if err != nil {
		t.Fatalf("createMonitorLog second: %s", err)
	}

	if err := createMonitorLogLastChecked(tx, later, monitorID, secondLogID); err != nil {
		t.Fatalf("createMonitorLogLastChecked second: %s", err)
	}

	lastChecked, err = getMonitorLogLastChecked(tx, monitorID)
	if err != nil {
		t.Fatalf("getMonitorLogLastChecked second: %s", err)
	}
	if lastChecked.ResponseCode.Int32 != 500 {
		t.Errorf("last checked response code = %d, want 500", lastChecked.ResponseCode.Int32)
	}
}

// TestSessionExpiry checks that a session older than sessionLifetime stops
// authenticating and gets collected.
func TestSessionExpiry(t *testing.T) {
	db := testDB(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %s", err)
	}
	defer tx.Rollback()

	userID, err := createUser(tx, "admin", "hash")
	if err != nil {
		t.Fatalf("createUser: %s", err)
	}

	if err := createSession(tx, "fresh", "csrf-fresh", userID); err != nil {
		t.Fatalf("createSession: %s", err)
	}

	if _, _, err := validateSession(tx, "fresh"); err != nil {
		t.Fatalf("validateSession fresh: %s", err)
	}

	stale := time.Now().UTC().Add(-sessionLifetime - time.Hour)
	if _, err := tx.Exec(
		"insert into session(token, csrf_token, user_id, created_at) values(?, ?, ?, ?)",
		"stale", "csrf-stale", userID, stale,
	); err != nil {
		t.Fatalf("insert stale: %s", err)
	}

	if _, _, err := validateSession(tx, "stale"); err == nil {
		t.Error("validateSession accepted an expired session")
	}

	deleted, err := deleteExpiredSessions(tx)
	if err != nil {
		t.Fatalf("deleteExpiredSessions: %s", err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d sessions, want 1", deleted)
	}

	if _, _, err := validateSession(tx, "fresh"); err != nil {
		t.Errorf("cleanup removed a live session: %s", err)
	}
}
