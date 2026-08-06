package main

import (
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
		if _, err := rwDB.Exec(
			`insert into alert(title, type, severity, created_at) values(?, 'incident', 'red', ?)`,
			r.title, r.createdAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// What the old query returned.
	var wantTitles []string
	old, err := tx.Query(
		`select title from alert where strftime("%Y-%m", created_at) = ? order by created_at`,
		"2026-03",
	)
	if err != nil {
		t.Fatal(err)
	}
	for old.Next() {
		var title string
		if err := old.Scan(&title); err != nil {
			t.Fatal(err)
		}
		wantTitles = append(wantTitles, title)
	}
	old.Close()
	if err := old.Err(); err != nil {
		t.Fatal(err)
	}

	if len(wantTitles) != 3 {
		t.Fatalf("the old filter matched %v, want the three March rows -- test setup is wrong",
			wantTitles)
	}

	got, err := getAlertHistory(tx, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	gotTitles := map[string]bool{}
	for _, a := range got {
		gotTitles[a.Title] = true
	}

	if len(gotTitles) != len(wantTitles) {
		t.Fatalf("range filter returned %d alerts %v, month filter returned %d %v",
			len(gotTitles), gotTitles, len(wantTitles), wantTitles)
	}
	for _, title := range wantTitles {
		if !gotTitles[title] {
			t.Errorf("range filter missed %q, which the month filter matched", title)
		}
	}
}
