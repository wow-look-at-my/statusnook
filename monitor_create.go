package main

import (
	"log"
	"net/http"
)

func getCreateMonitor(w http.ResponseWriter, r *http.Request) {
	// The config file owns these resources when text-based config is on, so the
	// form that would create one out of band stays closed, like the edit form.
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	refreshID := r.URL.Query().Get("refresh")

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notifications, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("getCreateMonitor.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("getEditMonitor.mailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getCreateMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_create_monitor.html")
	if err != nil {
		log.Printf("getCreateMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Notifications []NotificationChannel
			MailGroups    []MailGroup
			RefreshID     string
			Ctx           pageCtx
		}{
			Notifications: notifications,
			MailGroups:    mailGroups,
			RefreshID:     refreshID,
			Ctx:           getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
