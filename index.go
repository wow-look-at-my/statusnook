package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func getOngoingAlerts(tx *sql.Tx) ([]AlertDetail, error) {
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
			ended_at is null
		order by case 
			when severity = 'red' then 1
			when severity = 'amber' then 2
			else 3
		end asc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query: %w", err)
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
			return alerts, fmt.Errorf("getOngoingAlerts.Scan: %w", err)
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
		return alerts, fmt.Errorf("getOngoingAlerts.Query2: %w", err)
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
			return alerts, fmt.Errorf("getOngoingAlerts.Scan2: %w", err)
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
		return alerts, fmt.Errorf("getOngoingAlerts.Query3: %w", err)
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
			return alerts, fmt.Errorf("getOngoingAlerts.Scan3: %w", err)
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

func index(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("index.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("index.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("index.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("index.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("index.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	err = tx.Commit()
	if err != nil {
		log.Printf("index.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}{{.Ctx.Name}} Status{{end}}
		{{define "body"}}
			<div class="index-container">
				<div class="services-list">
					{{range $service := .Services}}
						<div class="service-row">
							<div>
								<span>{{$service.Name}}</span>
								<span>{{$service.HelperText}}</span>
							</div>
							<div>
								{{if eq (index $.ServiceStatuses $service.ID) "red"}}
									<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="#F84B37">
										<path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m9-.75a9 9 0 11-18 0 9 9 0 0118 0zm-9 3.75h.008v.008H12v-.008z" />
									</svg>
								{{else if eq (index $.ServiceStatuses $service.ID) "amber"}}
									<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="1.5" stroke="#E5B773" class="w-6 h-6">
  										<path stroke-linecap="round" stroke-linejoin="round" d="M12 9v3.75m-9.303 3.376c-.866 1.5.217 3.374 1.948 3.374h14.71c1.73 0 2.813-1.874 1.948-3.374L13.949 3.378c-.866-1.5-3.032-1.5-3.898 0L2.697 16.126zM12 15.75h.007v.008H12v-.008z" />
									</svg>
								{{else}}
									<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
										<path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 01.143 1.052l-8 10.5a.75.75 0 01-1.127.075l-4.5-4.5a.75.75 0 011.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 011.05-.143z" clip-rule="evenodd" />
									</svg>
								{{end}}
							</div>
						</div>
					{{end}}
				</div>

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

				
				
				<a class="index-link" href="/history" hx-boost="true">View full history</a>
				{{if not .Ctx.Auth.ID}}
					<a class="index-link index-link--secondary" href="/login" hx-boost="true">Manage this page</a>
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

				<dialog class="slack-success-modal success-modal">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z" clip-rule="evenodd" />
							</svg>
						</div>
						<span>Updates will appear in Slack</span>

						<button onclick="document.querySelector('.slack-success-modal').close();">Dismiss</button>
					</div>
				</dialog>
				<dialog class="email-confirmation-success-modal success-modal">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z" clip-rule="evenodd" />
							</svg>
						</div>
						<span>You've successfully subscribed to receive updates via email</span>

						<button onclick="document.querySelector('.email-confirmation-success-modal').close();">Dismiss</button>
					</div>
				</dialog>

				<script>
					(() => {
						const query = new URLSearchParams(window.location.search);
						if (query.get("slack_app_installed")) {
							document.querySelector(".slack-success-modal").showModal();
							history.replaceState(null, "", "/");
						}
	
						if (query.get("email_subscribed")) {
							document.querySelector(".email-confirmation-success-modal").showModal();
							history.replaceState(null, "", "/");
						}	
					})();
				</script>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("index", markup)
	if err != nil {
		log.Printf("index.parseTmpl: %s", err)
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
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04",
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

	serviceStatuses := make(map[int]string, len(services))
	for _, service := range services {
		serviceStatuses[service.ID] = ""
	}
	for _, alert := range alerts {
		for _, service := range alert.Services {
			if serviceStatuses[service.ID] != "red" {
				serviceStatuses[service.ID] = alert.Severity
			}
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Services             []service
			IncidentAlerts       []FormattedAlertDetail
			ServiceStatuses      map[int]string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			Services:             services,
			IncidentAlerts:       formattedAlerts,
			ServiceStatuses:      serviceStatuses,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("index.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook")
}

// crossAuthStore holds the one-time tokens that hand a session from the
// apex domain to the configured custom domain. It is guarded by a mutex:
// handlers run concurrently, and an unsynchronised map here used to be able to
// take the whole process down with a concurrent map write.
type crossAuthStore struct {
	mu     sync.Mutex
	tokens map[string]crossAuthToken
}

type crossAuthToken struct {
	userID    int
	expiresAt time.Time
}

// crossAuthTokenLifetime is deliberately short: the token travels in a URL and
// is redeemed by an immediate redirect.
const crossAuthTokenLifetime = time.Minute

var crossAuthTokens = &crossAuthStore{tokens: map[string]crossAuthToken{}}

func (s *crossAuthStore) issue(token string, userID int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for key, value := range s.tokens {
		if now.After(value.expiresAt) {
			delete(s.tokens, key)
		}
	}

	s.tokens[token] = crossAuthToken{
		userID:    userID,
		expiresAt: now.Add(crossAuthTokenLifetime),
	}
}

// redeem consumes a token, returning the user it authenticates.
func (s *crossAuthStore) redeem(token string, now time.Time) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	value, ok := s.tokens[token]
	delete(s.tokens, token)

	if !ok || now.After(value.expiresAt) {
		return 0, false
	}

	return value.userID, true
}

func postResolve(w http.ResponseWriter, r *http.Request) {
	authCtx := getAuthCtx(r)
	if authCtx.ID == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postResolve.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.URLEncoding.EncodeToString(tokenBytes)

	crossAuthTokens.issue(token, authCtx.ID, time.Now().UTC())

	w.Write([]byte(token))
}

func getCrossAuth(w http.ResponseWriter, r *http.Request) {
	redirectURL := "https://" + metaDomain + safeRedirectPath(r.URL.Query().Get("after"))

	auth := getAuthCtx(r)
	if auth.ID != 0 {
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	tokenParam := r.URL.Query().Get("token")
	if tokenParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userID, ok := crossAuthTokens.redeem(tokenParam, time.Now().UTC())
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	token, csrfToken, err := newSessionTokens()
	if err != nil {
		log.Printf("getCrossAuth.newSessionTokens: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("getCrossAuth.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if err = createSession(tx, token, csrfToken, userID); err != nil {
		log.Printf("getCrossAuth.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getCrossAuth.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, sessionCookie(r, token))

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// safeRedirectPath keeps an attacker-supplied "after" parameter from turning
// the cross-auth redirect into an open redirect. "//evil.com" is a
// protocol-relative URL and "@evil.com" makes the domain a userinfo field, so
// only a single-slash path is accepted.
func safeRedirectPath(after string) string {
	if after == "" || !strings.HasPrefix(after, "/") || strings.HasPrefix(after, "//") {
		return "/"
	}

	parsed, err := url.Parse(after)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return "/"
	}

	return parsed.EscapedPath() + optionalQuery(parsed.RawQuery)
}

func optionalQuery(raw string) string {
	if raw == "" {
		return ""
	}

	return "?" + raw
}
