package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The slug migration renames every table it touches to old__*, recreates it
// with a slug column, copies the rows across and backfills a slug from each
// name. It runs exactly once per installation, on data nobody can reproduce
// afterwards, so the only honest way to test it is against the schema an
// installation actually had -- testdata/pre_slug_schema.sql is that file,
// verbatim from the commit before the migration landed.
func TestSlugMigrationUpgradesARealPreSlugDatabase(t *testing.T) {
	oldSchema, err := os.ReadFile("testdata/pre_slug_schema.sql")
	require.NoError(t, err)

	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(wd)) })

	require.NoError(t, os.Mkdir("statusnook-data", 0700))

	old, err := sql.Open("sqlite3", "file:statusnook-data/app.db?_foreign_keys=on&_journal_mode=wal")
	require.NoError(t, err)

	_, err = old.Exec(string(oldSchema))
	require.NoError(t, err)

	// The migration table comes with that schema; what it holds is everything
	// applied before the slug migration, so initDB has exactly that one plus
	// the newer ones left to run.
	_, err = old.Exec(
		`insert into migration(name, skipped) values('1714828244_add_user_invitation', false)`,
	)
	require.NoError(t, err)

	// Rows in the tables that gain a slug, plus one in a table that is only
	// copied, so both halves of the migration have something to lose.
	_, err = old.Exec(`
		insert into service(name, helper_text) values('Ingest', 'ingest.example.com');
		insert into service(name, helper_text) values('API Gateway', 'api.example.com');
		insert into monitor(name, url, method, frequency, timeout, attempts)
			values('API health', 'https://example.com/health', 'GET', 60, 5, 2);
		insert into notification_channel(name, type, details)
			values('Chat', 'slack', '{"webhookURL":"https://hooks.example.com/x"}');
		insert into mail_group(name, description) values('Core', 'the rota');
		insert into mail_group_member(email_address, mail_group_id) values('core@example.com', 1);
		insert into monitor_notification_channel(monitor_id, notification_channel_id) values(1, 1);
		insert into monitor_log(started_at, ended_at, response_code, attempts, result, monitor_id)
			values(datetime(), datetime(), 200, 1, 'success', 1);
	`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	previousDB, previousRWDB := db, rwDB
	db = initDB(false)
	rwDB = initDB(true)
	rwDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		db.Close()
		rwDB.Close()
		db, rwDB = previousDB, previousRWDB
	})

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	services, err := listServices(tx)
	require.NoError(t, err)
	// Two inserted here, plus the one that schema seeded.
	require.Len(t, services, 3, "the rows have to survive the table swap")

	bySlug := map[string]service{}
	for _, s := range services {
		bySlug[s.Slug] = s
	}

	// The backfill is derived from the name: lower-cased, spaces to hyphens.
	require.Contains(t, bySlug, "ingest")
	require.Equal(t, "ingest.example.com", bySlug["ingest"].HelperText)
	require.Contains(t, bySlug, "api-gateway")
	require.Contains(t, bySlug, "website")

	monitors, err := listMonitors(tx)
	require.NoError(t, err)
	require.Len(t, monitors, 1)
	require.Equal(t, "api-health", monitors[0].Slug)

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, "chat", channels[0].Slug)

	groups, err := listMailGroups(tx)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "core", groups[0].Slug)

	// A table with no slug is copied wholesale, references included.
	members, err := listMailGroupMembersByID(tx, groups[0].ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "core@example.com", members[0].EmailAddress)

	monitorChannels, err := listNotificationChannelsByMonitorID(tx, monitors[0].ID)
	require.NoError(t, err)
	require.Len(t, monitorChannels, 1, "the join row survived with both ids intact")

	// Nothing is left behind: an old__ table still present would mean a
	// half-finished migration that the next startup would not retry.
	var leftovers int
	require.NoError(t, tx.QueryRow(
		`select count(*) from sqlite_schema where type = 'table' and name like 'old__%'`,
	).Scan(&leftovers))
	require.Zero(t, leftovers)

	// Every migration is recorded, so a restart re-runs none of them.
	var applied int
	require.NoError(t, tx.QueryRow(
		`select count(*) from migration where name = '1715019045_add_slug_columns'`,
	).Scan(&applied))
	require.Equal(t, 1, applied)

	require.NoError(t, tx.Rollback())

	// The whole point of recording them: a second initDB must be a no-op.
	rwDB.Close()
	rwDB = initDB(true)

	tx, err = db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	services, err = listServices(tx)
	require.NoError(t, err)
	require.Len(t, services, 3, "a restart must not re-run the migration")
}

// A fresh install runs schema.sql and records every migration as skipped, so
// the migration files never touch a database that was created with them
// already in it.
func TestFreshInstallSkipsEveryMigration(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(wd)) })

	previousDB, previousRWDB := db, rwDB
	db = initDB(false)
	rwDB = initDB(true)
	t.Cleanup(func() {
		db.Close()
		rwDB.Close()
		db, rwDB = previousDB, previousRWDB
	})

	files, err := os.ReadDir(filepath.Join(wd, "migrations"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	var skipped int
	require.NoError(t, tx.QueryRow("select count(*) from migration where skipped").Scan(&skipped))
	require.Equal(t, len(files), skipped,
		"every migration has to be recorded as skipped, or it runs against the new schema")
}
