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

	const markup = `
		{{define "title"}}Admin home{{end}}
		{{define "body"}}
			<div class="history-container">
				<div class="admin-nav-header">
					<div>
						<a href="/" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
						<h2>History</h2>
					</div>
					<div>
						<a {{if .PreviousPeriod}}href="/history?period={{.PreviousPeriod}}"{{end}} hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
								<path fill-rule="evenodd" d="M12.79 5.23a.75.75 0 01-.02 1.06L8.832 10l3.938 3.71a.75.75 0 11-1.04 1.08l-4.5-4.25a.75.75 0 010-1.08l4.5-4.25a.75.75 0 011.06.02z" clip-rule="evenodd" />
							</svg>
						</a>
						<span>{{.PeriodText}}</span>
						<a {{if .NextPeriod}}href="/history?period={{.NextPeriod}}"{{end}} hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
								<path fill-rule="evenodd" d="M7.21 14.77a.75.75 0 01.02-1.06L11.168 10 7.23 6.29a.75.75 0 111.04-1.08l4.5 4.25a.75.75 0 010 1.08l-4.5 4.25a.75.75 0 01-1.06-.02z" clip-rule="evenodd" />
							</svg>
						</a>
					</div>
				</div>
				{{if and (not (len .IncidentAlerts)) (not (len .MaintenanceAlerts))}}
					<div class="entity-empty-state">
						<div class="icon">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M8.485 2.495c.673-1.167 2.357-1.167 3.03 0l6.28 10.875c.673 1.167-.17 2.625-1.516 2.625H3.72c-1.347 0-2.189-1.458-1.515-2.625L8.485 2.495zM10 5a.75.75 0 01.75.75v3.5a.75.75 0 01-1.5 0v-3.5A.75.75 0 0110 5zm0 9a1 1 0 100-2 1 1 0 000 2z" clip-rule="evenodd" />
							</svg>
						</div>
						<span>No alerts for this period</span>
					</div>
				{{end}}
				{{if len .IncidentAlerts}}
					<div>
						<div class="index-alert-container">
							{{range $alert := .IncidentAlerts}}
								<div>
									<div>
										<div class="index-alert-container__header">
											<div>
												{{if eq $alert.Severity "red"}}
													<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="#F84B37">
														<path fill-rule="evenodd" d="M5.636 4.575a.75.75 0 0 1 0 1.061 9 9 0 0 0 0 12.728.75.75 0 1 1-1.06 1.06c-4.101-4.1-4.101-10.748 0-14.849a.75.75 0 0 1 1.06 0Zm12.728 0a.75.75 0 0 1 1.06 0c4.101 4.1 4.101 10.75 0 14.85a.75.75 0 1 1-1.06-1.061 9 9 0 0 0 0-12.728.75.75 0 0 1 0-1.06ZM7.757 6.697a.75.75 0 0 1 0 1.06 6 6 0 0 0 0 8.486.75.75 0 0 1-1.06 1.06 7.5 7.5 0 0 1 0-10.606.75.75 0 0 1 1.06 0Zm8.486 0a.75.75 0 0 1 1.06 0 7.5 7.5 0 0 1 0 10.606.75.75 0 0 1-1.06-1.06 6 6 0 0 0 0-8.486.75.75 0 0 1 0-1.06ZM9.879 8.818a.75.75 0 0 1 0 1.06 3 3 0 0 0 0 4.243.75.75 0 1 1-1.061 1.061 4.5 4.5 0 0 1 0-6.364.75.75 0 0 1 1.06 0Zm4.242 0a.75.75 0 0 1 1.061 0 4.5 4.5 0 0 1 0 6.364.75.75 0 0 1-1.06-1.06 3 3 0 0 0 0-4.243.75.75 0 0 1 0-1.061ZM10.875 12a1.125 1.125 0 1 1 2.25 0 1.125 1.125 0 0 1-2.25 0Z" clip-rule="evenodd" />
													</svg>
												{{else if eq $alert.Severity "amber"}}
													<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="#E5B773" class="w-6 h-6">
														<path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z" />
													</svg>
												{{else}}
													<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="#379BF8">
														<path stroke-linecap="round" stroke-linejoin="round" d="m11.25 11.25.041-.02a.75.75 0 0 1 1.063.852l-.708 2.836a.75.75 0 0 0 1.063.853l.041-.021M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9-3.75h.008v.008H12V8.25Z" />
													</svg>
												{{end}}
												<span>{{$alert.Title}}</span>
											</div>
											<span>{{$alert.Services}}</span>
										</div>
									</div>
									<div>
										{{range $message := $alert.Messages}}
											<div class="index-alert-container__row">
												<span>{{$message.CreatedAt}}</span>
												<span>{{$message.Content}}</span>
											</div>
										{{end}}
									</div>
								</div>
								<hr>
							{{end}}
						</div>
					</div>
				{{end}}

				<dialog class="email-updates-modal">
					<div>
						<span>Get email updates</span>
						<button onclick="document.querySelector('.email-updates-modal').close();">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path d="M6.28 5.22a.75.75 0 0 0-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 1 0 1.06 1.06L10 11.06l3.72 3.72a.75.75 0 1 0 1.06-1.06L11.06 10l3.72-3.72a.75.75 0 0 0-1.06-1.06L10 8.94 6.28 5.22Z" />
							</svg>
						</button>
					</div>
					<form hx-post="/subscribe/email" hx-swap="none">
						<label>
							Email address
							<input type="email" name="email" placeholder="example@example.com" required autofocus>
						</label>

						<button type="submit">Confirm</button>
					</form>
				</dialog>

				<dialog id="email-success-modal"></dialog>
				<dialog id="email-already-subscribed-modal"></dialog>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("history", markup)
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
