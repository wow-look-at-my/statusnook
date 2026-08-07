package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/goksan/statusnook/internal/sqlcgen"
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
	ctx := context.Background()

	// Both are fed by public endpoints and neither was ever deleted from:
	// pending subscriptions accumulate one row per attempted address, and
	// invitations outlive the 24h at which they stop being accepted.
	prune("pending_email_alert_subscription", now.Add(-pendingSubscriptionLifetime),
		func(q *sqlcgen.Queries, before time.Time) (int64, error) {
			return q.PrunePendingEmailAlertSubscriptions(
				ctx,
				sqlcgen.PrunePendingEmailAlertSubscriptionsParams{
					CreatedAt: before,
					Limit:     retentionBatchSize,
				},
			)
		})

	prune("user_invitation", now.Add(-userInvitationLifetime),
		func(q *sqlcgen.Queries, before time.Time) (int64, error) {
			return q.PruneUserInvitations(ctx, sqlcgen.PruneUserInvitationsParams{
				CreatedAt: before,
				Limit:     retentionBatchSize,
			})
		})

	// A session past its lifetime no longer validates, so the row is dead
	// weight; nothing ever deleted it and the table grew a row per login.
	prune("session", now.Add(-sessionLifetime),
		func(q *sqlcgen.Queries, before time.Time) (int64, error) {
			return q.PruneSessions(ctx, sqlcgen.PruneSessionsParams{
				CreatedAt: before,
				Limit:     retentionBatchSize,
			})
		})

	prune("monitor_log", now.Add(-monitorLogRetention),
		func(q *sqlcgen.Queries, before time.Time) (int64, error) {
			return q.PruneMonitorLogs(ctx, sqlcgen.PruneMonitorLogsParams{
				StartedAt: before,
				Limit:     retentionBatchSize,
			})
		})

	// Only notifications that were actually delivered. A null sent_at is
	// still queued, and deleting it would drop the alert silently.
	prune("alert_notification", now.Add(-alertNotificationRetention),
		func(q *sqlcgen.Queries, before time.Time) (int64, error) {
			return q.PruneAlertNotifications(ctx, sqlcgen.PruneAlertNotificationsParams{
				SentAt: sql.NullTime{Time: before, Valid: true},
				Limit:  retentionBatchSize,
			})
		})
}

func prune(
	table string,
	before time.Time,
	del func(*sqlcgen.Queries, time.Time) (int64, error),
) {
	total := int64(0)

	for {
		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("prune.Begin %s: %s", table, err)
			return
		}

		affected, err := del(sqlcgen.New(tx), before)
		if err != nil {
			tx.Rollback()
			log.Printf("prune.Exec %s: %s", table, err)
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
