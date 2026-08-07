package main

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

// The month filter moved from strftime("%Y-%m", created_at) = ? to a range on
// created_at, which is only equivalent if the driver's stored timestamps
// compare correctly against a bound time.Time. Proven here against a real
// database rather than assumed: the two forms must select the same rows.
func TestGetAlertHistoryMatchesTheOldMonthFilter(t *testing.T) {
	withTestDB(t)

	rows := []struct {
		title     string
		createdAt time.Time
	}{
		{"before", time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC)},
		{"first instant", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"middle", time.Date(2026, 3, 15, 12, 30, 0, 0, time.UTC)},
		{"last instant", time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)},
		{"after", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, r := range rows {
		_, err := rwDB.Exec(`insert into alert(title, type, severity, created_at) values(?, 'incident', 'red', ?)`, r.title, r.createdAt)
		require.Nil(t, err)

	}

	tx, err := rwDB.Begin()
	require.Nil(t, err)

	defer tx.Rollback()

	// What the old query returned.
	var wantTitles []string
	old, err := tx.Query(
		`select title from alert where strftime("%Y-%m", created_at) = ? order by created_at`,
		"2026-03",
	)
	require.Nil(t, err)

	for old.Next() {
		var title string
		require.NoError(t, old.Scan(&title))

		wantTitles = append(wantTitles, title)
	}
	old.Close()
	require.NoError(t, old.Err())

	require.Equal(t, 3, len(wantTitles))

	got, err := getAlertHistory(tx, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	require.Nil(t, err)

	gotTitles := map[string]bool{}
	for _, a := range got {
		gotTitles[a.Title] = true
	}

	require.Equal(t, len(wantTitles), len(gotTitles))

	for _, title := range wantTitles {
		assert.True(t, gotTitles[title])

	}
}
