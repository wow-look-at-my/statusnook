package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func getOldestAlertDate(tx *sql.Tx) (time.Time, error) {
	const query = `
		select 
			created_at
		from
			alert
		order by 
			created_at asc
		limit 1
	`

	date := time.Time{}
	err := tx.QueryRow(query).Scan(&date)
	if err != nil {
		return date, fmt.Errorf("getOldestAlertDate: %w", err)
	}

	return date, nil
}

func getAlertHistory(tx *sql.Tx, period string) ([]AlertDetail, error) {
	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where 
			strftime("%Y-%m", created_at) = ?
		order by created_at desc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery, period)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertDetail{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	alertIDs := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		alertIDs = append(alertIDs, strconv.Itoa(alert.ID))
	}

	messageQuery := fmt.Sprintf(
		`
			select
				id,
				content,
				created_at,
				last_updated_at,
				alert_id
			from
				alert_message
			where
				alert_id in(%s)
			order by created_at desc
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(messageQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query2: %w", err)
	}
	defer rows.Close()

	messages := map[int][]AlertDetailMessage{}

	for rows.Next() {
		alertID := 0
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan2: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		messages[alertID] = append(messages[alertID], message)
	}

	serviceQuery := fmt.Sprintf(
		`
		select
			service.id,
			service.name,
			service.helper_text,
			alert_id
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id in(%s)
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(serviceQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query3: %w", err)
	}
	defer rows.Close()

	services := map[int][]AlertDetailService{}

	for rows.Next() {
		alertID := 0

		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan3: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		services[alertID] = append(services[alertID], service)
	}

	for i, alert := range alerts {
		if _, ok := messages[alert.ID]; ok {
			alerts[i].Messages = messages[alert.ID]
		}
		if _, ok := services[alert.ID]; ok {
			alerts[i].Services = services[alert.ID]
		}
	}

	return alerts, nil
}

func history(w http.ResponseWriter, r *http.Request) {
	periodParam := r.URL.Query().Get("period")

	if len(periodParam) == 0 {
		periodParam = time.Now().UTC().Format("2006-01")
	}

	if len(periodParam) != 7 {
		http.Redirect(w, r, "/history", http.StatusFound)
		return
	}

	periodDate, err := time.Parse("2006-01", periodParam)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("history.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	oldestAlertDate, err := getOldestAlertDate(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getOldestAlertDate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getAlertHistory(tx, periodParam)
	if err != nil {
		log.Printf("history.listAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("history.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("history.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("history.html")
	if err != nil {
		log.Printf("history.parseTmpl: %s", err)
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
		Services  string
	}

	formattedAlerts := make([]FormattedAlertDetail, 0, len(alerts))

	for _, alert := range alerts {
		messages := make([]FormattedAlertDetailMessage, 0, len(alert.Messages))
		for _, message := range alert.Messages {
			formattedMessage := FormattedAlertDetailMessage{
				ID:        message.ID,
				Content:   message.Content,
				CreatedAt: message.CreatedAt.Format("Jan 2 • 15:04 MST"),
			}
			if message.LastUpdatedAt != nil {
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04 MST",
				)
			}

			messages = append(messages, formattedMessage)
		}

		serviceNames := make([]string, 0, len(alert.Services))
		for _, service := range alert.Services {
			serviceNames = append(serviceNames, service.Name)
		}

		formattedAlert := FormattedAlertDetail{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: alert.CreatedAt.Format("02/01/2006 15:04 MST"),
			Messages:  messages,
			Services:  strings.Join(serviceNames, " • "),
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	previousPeriodDate := periodDate.AddDate(0, -1, 0)
	nextPeriodDate := periodDate.AddDate(0, 1, 0)

	previousPeriodStr := previousPeriodDate.Format("2006-01")
	nextPeriodStr := nextPeriodDate.Format("2006-01")

	now := time.Now().UTC()

	if oldestAlertDate.IsZero() ||
		time.Date(previousPeriodDate.Year(), previousPeriodDate.Month(), 1, 0, 0, 0, 0, now.Location()).
			Before(time.Date(oldestAlertDate.Year(), oldestAlertDate.Month(), 1, 0, 0, 0, 0, now.Location())) {
		previousPeriodStr = ""
	}

	if nextPeriodDate.After(time.Now().UTC()) {
		nextPeriodStr = ""
	}

	err = tmpl.Execute(
		w,
		struct {
			IncidentAlerts       []FormattedAlertDetail
			MaintenanceAlerts    []FormattedAlertDetail
			PeriodText           string
			PreviousPeriod       string
			NextPeriod           string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			IncidentAlerts:       formattedAlerts,
			PeriodText:           periodDate.Format("Jan 2006"),
			PreviousPeriod:       previousPeriodStr,
			NextPeriod:           nextPeriodStr,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("history.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
