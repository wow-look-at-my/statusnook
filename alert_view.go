package main

import (
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func getAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, id)
	if err != nil {
		log.Printf("getAlert.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_alert.html")
	if err != nil {
		log.Printf("getAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  []FormattedAlertDetailService
	}

	formattedAlert := FormattedAlertDetail{}
	formattedAlert.ID = alert.ID
	formattedAlert.Title = alert.Title
	formattedAlert.AlertType = alert.AlertType
	formattedAlert.Severity = alert.Severity
	formattedAlert.CreatedAt = alert.CreatedAt.Format("02/01/2006 15:04 MST")
	if alert.EndedAt != nil {
		formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
	}

	for _, message := range alert.Messages {
		createdAt := message.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
		if message.CreatedAt.Year() == time.Now().UTC().Year() {
			createdAt = message.CreatedAt.Format("Jan 2 • 15:04 MST")
		}

		formattedMessage := FormattedAlertDetailMessage{
			ID:        message.ID,
			Content:   message.Content,
			CreatedAt: createdAt,
		}
		if message.LastUpdatedAt != nil {
			formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlert.Messages = append(
			formattedAlert.Messages,
			formattedMessage,
		)
	}

	serviceNames := make([]string, 0, len(alert.Services))
	for _, service := range alert.Services {
		serviceNames = append(serviceNames, service.Name)
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert    FormattedAlertDetail
			Services string
			Ctx      pageCtx
		}{
			Alert:    formattedAlert,
			Services: strings.Join(serviceNames, " • "),
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func deleteAlertByID(tx *sql.Tx, id int) error {
	const query = `
		delete from alert where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteAlertByID.Exec: %w", err)
	}

	return nil
}

func deleteAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteAlertByID(tx, id)
	if err != nil {
		log.Printf("deleteAlert.deleteAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}
