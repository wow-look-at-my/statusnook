package main

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// prune runs against the package-level rwDB, so point that at a throwaway
// database for the duration of the test.
func withTestDB(t *testing.T) {
	t.Helper()

	testDB, err := sql.Open("sqlite3", "file:"+t.Name()+"?mode=memory&cache=shared&_foreign_keys=on")
	require.Nil(t, err)

	testDB.SetMaxOpenConns(1)

	_, err = testDB.Exec(sqlSchema)
	require.Nil(t, err)

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

	_, err := rwDB.Exec(`insert into monitor(slug, name, url, method, frequency, timeout, attempts)
		 values('m', 'M', 'https://example.com', 'GET', 60, 5, 1)`)
	require.Nil(t, err)

	// One row either side of the boundary, plus enough old rows to force the
	// batch loop round more than once.
	for i := 0; i < retentionBatchSize+7; i++ {
		_, err := rwDB.Exec(`insert into monitor_log(started_at, ended_at, attempts, result, monitor_id)
			 values(?, ?, 1, 'success', 1)`, old, old)
		require.Nil(t, err)

	}

	_, err = rwDB.Exec(`insert into monitor_log(started_at, ended_at, attempts, result, monitor_id)
		 values(?, ?, 1, 'success', 1)`, now, now)
	require.Nil(t, err)

	pruneOnce()

	var remaining int
	require.NoError(t, rwDB.QueryRow("select count(*) from monitor_log").Scan(&remaining))

	require.Equal(t, 1, remaining)

}

func TestPruneLeavesUnsentNotifications(t *testing.T) {
	withTestDB(t)

	old := time.Now().UTC().Add(-alertNotificationRetention - time.Hour)

	for _, q := range []string{
		`insert into alert(title, type, severity, created_at) values('t','a','red',datetime('now'))`,
		`insert into alert_message(content, created_at, alert_id) values('m', datetime('now'), 1)`,
		`insert into alert_subscription(type, destination) values('email','a@example.com')`,
	} {
		_, err := rwDB.Exec(q)
		require.Nil(t, err)

	}

	// Same age; only the delivered one may go.
	_, err := rwDB.Exec(`insert into alert_notification(created_at, sent_at, alert_subscription_id, alert_message_id)
		 values(?, ?, 1, 1)`, old, old)
	require.Nil(t, err)

	_, err = rwDB.Exec(`insert into alert_notification(created_at, alert_subscription_id, alert_message_id)
		 values(?, 1, 1)`, old)
	require.Nil(t, err)

	pruneOnce()

	var remaining, unsent int
	require.NoError(t, rwDB.QueryRow("select count(*) from alert_notification").Scan(&remaining))

	require.NoError(t, rwDB.QueryRow("select count(*) from alert_notification where sent_at is null").Scan(&unsent))

	require.False(t, remaining != 1 || unsent != 1)

}
