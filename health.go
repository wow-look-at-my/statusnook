package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// health answers container health checks. It touches the database, because a
// process that cannot read its own database is not healthy no matter how well
// it serves HTTP.
func health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Printf("health.Ping: %s", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("database unavailable\n"))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// sessionCleanupInterval is how often expired sessions are collected. Sessions
// stop authenticating the moment they expire; this only reclaims rows.
const sessionCleanupInterval = time.Hour

func sessionCleanupLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()

	for {
		cleanupSessions()

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func cleanupSessions() {
	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("cleanupSessions.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	deleted, err := deleteExpiredSessions(tx)
	if err != nil {
		log.Printf("cleanupSessions.deleteExpiredSessions: %s", err)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("cleanupSessions.Commit: %s", err)
		return
	}

	if deleted > 0 {
		log.Printf("removed %d expired session(s)", deleted)
	}
}

// branchLabel names the branch being watched for a log line.
func branchLabel(branch string) string {
	if branch == "" {
		return "default branch"
	}

	return "branch " + branch
}
