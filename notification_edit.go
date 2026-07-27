package main

import (
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func getEditNotification(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditNotification.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, id)
	if err != nil {
		log.Printf("getEditNotification.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditNotification.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_edit_notification.html")
	if err != nil {
		log.Printf("getEditNotification.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	isPostmark := false

	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if ok {
		isPostmark = strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com")
	}

	err = tmpl.Execute(
		w,
		struct {
			Notification NotificationChannel
			IsPostmark   bool
			ReadOnly     bool
			Ctx          pageCtx
		}{
			Notification: channel,
			IsPostmark:   isPostmark,
			ReadOnly:     readOnly,
			Ctx:          getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditNotification.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
