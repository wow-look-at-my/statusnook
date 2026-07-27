package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"
)

type AlertListing struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
}

func listAlerts(tx *sql.Tx) ([]AlertListing, error) {
	const query = `
		select id, title, type, severity, created_at, ended_at from alert
		order by created_at desc
	`

	alerts := []AlertListing{}

	rows, err := tx.Query(query)
	if err != nil {
		return alerts, fmt.Errorf("listAlerts.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertListing{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("listAlerts.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	return alerts, nil
}

func alerts(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("alerts.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alerts, err := listAlerts(tx)
	if err != nil {
		log.Printf("alerts.listAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("alerts.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("alerts.html")
	if err != nil {
		log.Printf("alerts.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlert struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
	}

	formattedAlerts := make([]FormattedAlert, 0, len(alerts))
	for _, alert := range alerts {
		createdAt := alert.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
		if alert.CreatedAt.Year() == time.Now().UTC().Year() {
			createdAt = alert.CreatedAt.Format("Jan 2 • 15:04 MST")
		}

		formattedAlert := FormattedAlert{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: createdAt,
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	err = tmpl.Execute(
		w,
		struct {
			Alerts []FormattedAlert
			Ctx    pageCtx
		}{

			Alerts: formattedAlerts,
			Ctx:    getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("alerts.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
