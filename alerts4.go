package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
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

	tmpl, err := parseTmpl("getAddAlertMessage", getAddAlertMessageMarkup)
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

	tmpl, err := parseTmpl("getEditAlertMessage", getEditAlertMessageMarkup)
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
