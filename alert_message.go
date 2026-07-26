package main

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getAddAlertMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAddAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("getAddAlertMessage.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getAddAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}{{.Alert.Title}} - add message{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/alerts/{{.Alert.ID}}" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
						{{if not .Alert.EndedAt }}
							{{if eq .Alert.AlertType "incident"}}
								<div class="live">LIVE</div>
							{{else}}
								<div class="active">ACTIVE</div>
							{{end}}
						{{end}}
						<h2>{{.Alert.Title}}</h2>
					</div>
				</div>

				<form hx-post hx-swap="none">
					<label>
						Message
						<textarea name="message" required></textarea>
					</label>

					<div>
						<button type="submit">Publish</button>
					</div>
				</form>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getAddAlertMessage", markup)
	if err != nil {
		log.Printf("getAddAlertMessage.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert AlertDetail
			Ctx   pageCtx
		}{
			Alert: alert,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getAddAlertMessage.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createAlertMessage(tx *sql.Tx, alertID int, content string) (int, error) {
	const query = `
		insert into
			alert_message(content, created_at, alert_id)
		values(?, ?, ?)
		returning id
	`

	var id int

	err := tx.QueryRow(query, content, time.Now().UTC(), alertID).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createAlertMessage.Scan: %w", err)
	}

	return id, nil
}

func postAddAlertMessage(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postAddAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alertMessageID, err := createAlertMessage(tx, id, message)
	if err != nil {
		log.Printf("postAddAlertMessage.createAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertMessageNotifications(tx, time.Now().UTC(), alertMessageID)
	if err != nil {
		log.Printf("postAddAlertMessage.createAlertMessageNotifications: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postAddAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}

func deleteAlertMessageByID(tx *sql.Tx, alertID int, messageID int) error {
	const query = `
		delete from alert_message where alert_id = ? and id = ?
	`

	_, err := tx.Exec(query, alertID, messageID)
	if err != nil {
		return fmt.Errorf("deleteAlertMessageByID.Exec: %w", err)
	}

	return nil
}

func deleteAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertIDParam := chi.URLParam(r, "id")
	alertID, err := strconv.Atoi(alertIDParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteAlertMessageByID(tx, alertID, messageID)
	if err != nil {
		log.Printf("deleteAlertMessage.deleteAlertMessageByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+alertIDParam)
}

func getEditAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, alertID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("getEditAlertMessage.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	message := AlertDetailMessage{}
	for _, msg := range alert.Messages {
		if msg.ID == messageID {
			message = msg
			break
		}
	}

	if message.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	const markup = `
		{{define "title"}}{{.Alert.Title}} - add message{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/alerts/{{.Alert.ID}}" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
						{{if not .Alert.EndedAt }}
							{{if eq .Alert.AlertType "incident"}}
								<div class="live">LIVE</div>
							{{else}}
								<div class="active">ACTIVE</div>
							{{end}}
						{{end}}
						<h2>{{.Alert.Title}}</h2>
					</div>
				</div>

				<form hx-post hx-swap="none">
					<label>
						Message
						<textarea name="message" required>{{.Message.Content}}</textarea>
					</label>

					<div>
						<button type="submit">Republish</button>
					</div>
				</form>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getEditAlertMessage", markup)
	if err != nil {
		log.Printf("getEditAlertMessage.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert   AlertDetail
			Message AlertDetailMessage
			Ctx     pageCtx
		}{
			Alert:   alert,
			Message: message,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditAlertMessage.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editAlertMessage(tx *sql.Tx, alertID int, messageID int, content string) error {
	const query = `
		update alert_message 
		set 
			content = ?,
			last_updated_at = ? 
		where 
			alert_id = ? and id = ?
	`

	_, err := tx.Exec(query, content, time.Now().UTC(), alertID, messageID)
	if err != nil {
		return fmt.Errorf("editAlertMessage.Exec: %w", err)
	}

	return nil
}

func postEditAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertIDParam := chi.URLParam(r, "id")

	alertID, err := strconv.Atoi(alertIDParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")
	if message == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editAlertMessage(tx, alertID, messageID, message)
	if err != nil {
		log.Printf("postEditAlertMessage.editAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+alertIDParam)
}
