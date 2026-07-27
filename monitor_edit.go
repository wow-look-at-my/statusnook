package main

import (
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func getEditMonitor(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	refreshID := r.URL.Query().Get("refresh")
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitor, err := getMonitorByID(tx, id)
	if err != nil {
		log.Printf("getEditMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("getEditMonitor.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorNotificationChannels, err := listNotificationChannelsByMonitorID(tx, monitor.ID)
	if err != nil {
		log.Printf("getEditMonitor.listNotificationChannelsByMonitorID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	monitorNotificationsMap := map[int]bool{}
	for _, v := range monitorNotificationChannels {
		monitorNotificationsMap[v.ID] = true
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("getEditMonitor.mailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	selectedMailGroups, err := listMailGroupIDsByMonitorID(tx, monitor.ID)
	if err != nil {
		log.Printf("getEditMonitor.listMailGroupIDsByMonitorID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	selectedMailGroupsMap := map[int]bool{}
	for _, v := range selectedMailGroups {
		selectedMailGroupsMap[v.ID] = true
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_edit_monitor.html")
	if err != nil {
		log.Printf("getEditMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	textBody := ""
	if monitor.BodyFormat.String == "text" {
		textBody = monitor.Body.String
	}

	formData := url.Values{}
	if monitor.BodyFormat.String == "form" {
		data, err := url.ParseQuery(monitor.Body.String)
		if err != nil {
			log.Printf("getEditMonitor.ParseQuery: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		formData = data
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitor              Monitor
			TextBody             string
			FormData             url.Values
			Notifications        []NotificationChannel
			MonitorNotifications map[int]bool
			MailGroups           []MailGroup
			SelectedMailGroups   map[int]bool
			RefreshID            string
			ReadOnly             bool
			Ctx                  pageCtx
		}{
			Monitor:              monitor,
			TextBody:             textBody,
			FormData:             formData,
			Notifications:        channels,
			MonitorNotifications: monitorNotificationsMap,
			MailGroups:           mailGroups,
			SelectedMailGroups:   selectedMailGroupsMap,
			RefreshID:            refreshID,
			ReadOnly:             readOnly,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
