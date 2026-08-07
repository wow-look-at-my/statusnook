package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

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

	tmpl, err := parseTmpl("alerts", alertsMarkup)
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

type AlertDetailMessage struct {
	ID            int
	Content       string
	CreatedAt     *time.Time
	LastUpdatedAt *time.Time
}

type AlertDetailService struct {
	ID         int
	Name       string
	HelperText string
}

type AlertDetail struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
	Messages  []AlertDetailMessage
	Services  []AlertDetailService
}

type AlertSettings struct {
	SlackInstallURL      string
	SlackClientSecret    string
	ManagedSubscriptions bool
}

func getAlertNotifications(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAlertNotifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notifications, err := listNotificationChannels(tx, listNotificationsOptions{Type: "smtp"})
	if err != nil {
		log.Printf("getAlertNotifications.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	settings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("getAlertNotifications.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpNotificationChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getAlertNotifications.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getAlertNotifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("getAlertNotifications", getAlertNotificationsMarkup)
	if err != nil {
		log.Printf("getAlertNotifications.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, struct {
		Notifications           []NotificationChannel
		Settings                AlertSettings
		SMTPNotificationChannel int
		Domain                  string
		ConfigFileEnabled       bool
		Ctx                     pageCtx
	}{
		Notifications:           notifications,
		Settings:                settings,
		SMTPNotificationChannel: smtpNotificationChannelID,
		Domain:                  metaDomain.Load(),
		ConfigFileEnabled:       metaConfigFileEnabled.Load(),
		Ctx:                     getPageCtx(r),
	})
	if err != nil {
		log.Printf("getAlertNotifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postAlertNotifications(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	slackInstallURLParam := r.FormValue("slack-install-url")
	slackInstallURL := ""
	if slackInstallURLParam != "" {
		url, err := url.ParseRequestURI(slackInstallURLParam)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		slackInstallURL = url.String()
	}

	slackClientSecretParam := r.FormValue("slack-client-secret")

	smtpNotificationChannelIDParam := r.FormValue("smtp-notification-channel")
	smtpNotificationChannelID := 0
	if smtpNotificationChannelIDParam != "" {
		id, err := strconv.Atoi(r.FormValue("smtp-notification-channel"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		smtpNotificationChannelID = id
	}

	managedSubscriptions := false
	managedSubscriptionsParam := r.FormValue("managed-subscriptions")
	if managedSubscriptionsParam == "on" {
		managedSubscriptions = true
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postAlertNotifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, smtpNotificationChannelID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postAlertNotifications.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if channel.ID != 0 && channel.Type != "smtp" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	err = updateAlertSMTPNotificationSetting(tx, channel.ID)
	if err != nil {
		log.Printf("postAlertNotifications.updateAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateAlertSettings(tx, slackInstallURL, slackClientSecretParam, managedSubscriptions)
	if err != nil {
		log.Printf("postAlertNotifications.updateAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postAlertNotifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

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

	tmpl, err := parseTmpl("getAlert", getAlertMarkup)
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
