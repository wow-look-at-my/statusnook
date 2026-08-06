package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Nothing ever deleted from monitor_log or alert_notification. At the minimum
// frequency of 10s one monitor writes 8,640 rows a day -- roughly 464 MB per
// monitor-year with its index -- into a SQLite file in a container volume,
// and every unsent-notification scan pays for the rows that outlived their
// alert.
const (
	monitorLogRetention        = 90 * 24 * time.Hour
	alertNotificationRetention = 90 * 24 * time.Hour

	// Deleted in chunks: rwDB is one connection, so an unbounded DELETE on a
	// table this size would hold the write lock away from the monitor loop
	// for as long as it took.
	retentionBatchSize = 5000

	retentionInterval = 6 * time.Hour
)

func retentionLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()

	// Once at startup: an instance that is restarted more often than the
	// interval would otherwise never prune.
	pruneOnce()

	for {
		select {
		case <-ticker.C:
			pruneOnce()
		case <-ctx.Done():
			return
		}
	}
}

func pruneOnce() {
	now := time.Now().UTC()

	// A session past its lifetime no longer validates, so the row is dead
	// weight; nothing ever deleted it and the table grew a row per login.
	prune(
		"session",
		`delete from session where id in (
			select id from session where created_at < ? limit ?
		)`,
		now.Add(-sessionLifetime),
	)

	prune(
		"monitor_log",
		`delete from monitor_log where id in (
			select id from monitor_log where started_at < ? limit ?
		)`,
		now.Add(-monitorLogRetention),
	)

	// Only notifications that were actually delivered. A null sent_at is
	// still queued, and deleting it would drop the alert silently.
	prune(
		"alert_notification",
		`delete from alert_notification where id in (
			select id from alert_notification
			where sent_at is not null and sent_at < ? limit ?
		)`,
		now.Add(-alertNotificationRetention),
	)
}

func prune(table string, query string, before time.Time) {
	total := int64(0)

	for {
		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("prune.Begin %s: %s", table, err)
			return
		}

		result, err := tx.Exec(query, before, retentionBatchSize)
		if err != nil {
			tx.Rollback()
			log.Printf("prune.Exec %s: %s", table, err)
			return
		}

		affected, err := result.RowsAffected()
		if err != nil {
			tx.Rollback()
			log.Printf("prune.RowsAffected %s: %s", table, err)
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("prune.Commit %s: %s", table, err)
			return
		}

		total += affected

		if affected < retentionBatchSize {
			break
		}
	}

	if total > 0 {
		log.Printf("prune: removed %d rows from %s older than %s", total, table, before)
	}
}
