package main

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// prune runs against the package-level rwDB, so point that at a throwaway
// database for the duration of the test.
func withTestDB(t *testing.T) {
	t.Helper()

	testDB, err := sql.Open("sqlite3", "file:"+t.Name()+"?mode=memory&cache=shared&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	testDB.SetMaxOpenConns(1)

	if _, err := testDB.Exec(sqlSchema); err != nil {
		t.Fatal(err)
	}

	previous := rwDB
	rwDB = testDB
	t.Cleanup(func() {
		rwDB = previous
		testDB.Close()
	})
}

func TestPruneMonitorLogKeepsRecentRows(t *testing.T) {
	withTestDB(t)

	now := time.Now().UTC()
	old := now.Add(-monitorLogRetention - time.Hour)

	if _, err := rwDB.Exec(
		`insert into monitor(slug, name, url, method, frequency, timeout, attempts)
		 values('m', 'M', 'https://example.com', 'GET', 60, 5, 1)`,
	); err != nil {
		t.Fatal(err)
	}

	// One row either side of the boundary, plus enough old rows to force the
	// batch loop round more than once.
	for i := 0; i < retentionBatchSize+7; i++ {
		if _, err := rwDB.Exec(
			`insert into monitor_log(started_at, ended_at, attempts, result, monitor_id)
			 values(?, ?, 1, 'success', 1)`, old, old,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rwDB.Exec(
		`insert into monitor_log(started_at, ended_at, attempts, result, monitor_id)
		 values(?, ?, 1, 'success', 1)`, now, now,
	); err != nil {
		t.Fatal(err)
	}

	pruneOnce()

	var remaining int
	if err := rwDB.QueryRow("select count(*) from monitor_log").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("monitor_log has %d rows after pruning, want 1", remaining)
	}
}

func TestPruneLeavesUnsentNotifications(t *testing.T) {
	withTestDB(t)

	old := time.Now().UTC().Add(-alertNotificationRetention - time.Hour)

	for _, q := range []string{
		`insert into alert(title, type, severity, created_at) values('t','a','red',datetime('now'))`,
		`insert into alert_message(content, created_at, alert_id) values('m', datetime('now'), 1)`,
		`insert into alert_subscription(type, destination) values('email','a@example.com')`,
	} {
		if _, err := rwDB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	// Same age; only the delivered one may go.
	if _, err := rwDB.Exec(
		`insert into alert_notification(created_at, sent_at, alert_subscription_id, alert_message_id)
		 values(?, ?, 1, 1)`, old, old,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := rwDB.Exec(
		`insert into alert_notification(created_at, alert_subscription_id, alert_message_id)
		 values(?, 1, 1)`, old,
	); err != nil {
		t.Fatal(err)
	}

	pruneOnce()

	var remaining, unsent int
	if err := rwDB.QueryRow("select count(*) from alert_notification").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := rwDB.QueryRow(
		"select count(*) from alert_notification where sent_at is null",
	).Scan(&unsent); err != nil {
		t.Fatal(err)
	}

	if remaining != 1 || unsent != 1 {
		t.Fatalf("alert_notification has %d rows (%d unsent) after pruning, want 1 and 1",
			remaining, unsent)
	}
}
