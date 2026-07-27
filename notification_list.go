package main

import (
	"log"
	"net/http"
)

func notifications(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("notifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("notifications.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("notifications.listMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("notifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("notifications.html")
	if err != nil {
		log.Printf("notifications.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Notifications []NotificationChannel
			MailGroups    []MailGroup
			Ctx           pageCtx
		}{
			Notifications: channels,
			MailGroups:    mailGroups,
			Ctx:           getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("notifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getCreateNotification(w http.ResponseWriter, r *http.Request) {
	// The config file owns these resources when text-based config is on, so the
	// form that would create one out of band stays closed, like the edit form.
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tmpl, err := parseTmpl("get_create_notification.html")
	if err != nil {
		log.Printf("getCreateNotification.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateNotification.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
