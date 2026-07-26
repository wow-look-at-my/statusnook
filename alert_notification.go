package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

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

func getAlertByID(tx *sql.Tx, id int) (AlertDetail, error) {
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
			id = ?
	`

	alert := AlertDetail{}

	err := tx.QueryRow(alertQuery, id).Scan(
		&alert.ID,
		&alert.Title,
		&alert.AlertType,
		&alert.Severity,
		&alert.CreatedAt,
		&alert.EndedAt,
	)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.QueryRow: %w", err)
	}

	const messageQuery = `
		select
			id,
			content,
			created_at,
			last_updated_at
		from
			alert_message
		where
			alert_id = ?
		order by created_at desc
	`

	rows, err := tx.Query(messageQuery, id)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
		)
		if err != nil {
			return alert, fmt.Errorf("getAlertByID.Scan: %w", err)
		}

		alert.Messages = append(alert.Messages, message)
	}

	const serviceQuery = `
		select
			service.id,
			service.name,
			service.helper_text
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id = ?
	`

	rows, err = tx.Query(serviceQuery, id)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.Query2: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
		)
		if err != nil {
			return alert, fmt.Errorf("getAlertByID.Scan2: %w", err)
		}

		alert.Services = append(alert.Services, service)
	}

	return alert, nil
}

type AlertSettings struct {
	SlackInstallURL      string
	SlackClientSecret    string
	ManagedSubscriptions bool
}

func getAlertSettings(tx *sql.Tx) (AlertSettings, error) {
	const query = `
		select name, value from alert_setting
	`

	settings := AlertSettings{}

	rows, err := tx.Query(query)
	if err != nil {
		return settings, fmt.Errorf("getAlertSettings.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string

		err = rows.Scan(&k, &v)
		if err != nil {
			return settings, fmt.Errorf("getAlertSettings.Scan: %w", err)
		}

		if k == "slack-install-url" {
			settings.SlackInstallURL = v
		} else if k == "slack-client-secret" {
			settings.SlackClientSecret = v
		} else if k == "managed-subscriptions" {
			parsedV, err := strconv.ParseBool(v)
			if err != nil {
				return settings, fmt.Errorf("getAlertSettings.ParseBool: %w", err)
			}
			settings.ManagedSubscriptions = parsedV
		}
	}

	return settings, nil
}

func getAlertSMTPNotificationSetting(tx *sql.Tx) (int, error) {
	const query = `
		select notification_channel_id from alert_setting_smtp_notification limit 1
	`

	v := 0

	err := tx.QueryRow(query).Scan(&v)
	if err != nil {
		return v, fmt.Errorf("getAlertSMTPNotificationSetting.QueryRow: %w", err)
	}

	return v, nil
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

	const markup = `
		{{define "title"}}Alert notifications{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/alerts" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
				
						<h2>Alert notifications</h2>
					</div>
				</div>

				<p style="font-size: 1.6rem; margin-bottom: 4.6rem;">Configure which options appear on your status page for visitors to receive alert updates</p>
				
				<form hx-post hx-swap="none" autocomplete="off">
					<div class="notification-channels">
						<label>
							Email notification channel
							<span class="subtext">
								Choose a notification channel to use for status page email alerts
							</span>
						</label>

						<div 
							id="notification-channels-group"
							class="notification-channels-group"
							{{if and .Ctx.HXRequest (not .Ctx.HXBoosted)}}hx-swap-oob="true"{{end}}
						>
							{{if not (len .Notifications)}}
								<div
									class="entity-empty-state {{if not .Ctx.ConfigFile}}entity-empty-state--secondary{{end}}" 
									style="width: 100%; margin-top: 1.0rem;"
								>
									<div class="icon">
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
											<path d="M3 4a2 2 0 0 0-2 2v1.161l8.441 4.221a1.25 1.25 0 0 0 1.118 0L19 7.162V6a2 2 0 0 0-2-2H3Z" />
											<path d="m19 8.839-7.77 3.885a2.75 2.75 0 0 1-2.46 0L1 8.839V14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V8.839Z" />
										</svg>  
									</div>
									<span>No email notification channels found</span>
									<div class="actions">
										{{if not .Ctx.ConfigFile}}
											<a 
												class="action"
												href="/admin/notifications/create"
												target="_blank"
												hx-boost="true"
												hx-swap="outerHTML"
											>
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
													<path d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z" />
													<path d="M3.5 6.75c0-.69.56-1.25 1.25-1.25H7A.75.75 0 0 0 7 4H4.75A2.75 2.75 0 0 0 2 6.75v4.5A2.75 2.75 0 0 0 4.75 14h4.5A2.75 2.75 0 0 0 12 11.25V9a.75.75 0 0 0-1.5 0v2.25c0 .69-.56 1.25-1.25 1.25h-4.5c-.69 0-1.25-.56-1.25-1.25v-4.5Z" />
												</svg>
												Add channel
											</a>
											<button type="button" class="empty-state-refresh" hx-get="">
												<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
													<path fill-rule="evenodd" d="M13.836 2.477a.75.75 0 0 1 .75.75v3.182a.75.75 0 0 1-.75.75h-3.182a.75.75 0 0 1 0-1.5h1.37l-.84-.841a4.5 4.5 0 0 0-7.08.932.75.75 0 0 1-1.3-.75 6 6 0 0 1 9.44-1.242l.842.84V3.227a.75.75 0 0 1 .75-.75Zm-.911 7.5A.75.75 0 0 1 13.199 11a6 6 0 0 1-9.44 1.241l-.84-.84v1.371a.75.75 0 0 1-1.5 0V9.591a.75.75 0 0 1 .75-.75H5.35a.75.75 0 0 1 0 1.5H3.98l.841.841a4.5 4.5 0 0 0 7.08-.932.75.75 0 0 1 1.025-.273Z" clip-rule="evenodd" />
												</svg>
												Refresh
											</button>
										{{else}}
											<a 
												class="action"
												href="/admin/settings#config-form"
												target="_blank"
												hx-boost="true"
												hx-swap="outerHTML"
											>
												Go to settings
											</a>
										{{end}}
									</div>
								</div>
							{{end}}
							{{range $notification := .Notifications}}
								<label>
									<input
										type="checkbox"
										name="smtp-notification-channel"
										value="{{$notification.ID}}"
										onchange="onChangeChannel(this)"
										{{if eq $notification.ID $.SMTPNotificationChannel}}checked{{end}}
									/>
									<span>
										{{$notification.Name}}
									</span>
								</label>
							{{end}}
						</div>
					</div>

					<label>
						Statusnook managed unsubscribe
						<span class="subtext">
							Disable this option if you want to use your mail providers unsubscribe links
						</span>
						<span class="subtext">
							We currently recommend that only Postmark users turn this off
						</span>
					</label>

					<div class="checkbox-group">
						<label>
							<input 
								name="managed-subscriptions"
								type="checkbox"
								{{if $.Settings.ManagedSubscriptions}}checked{{end}}
							/>
						</label>
					</div>


					<hr style="border: 1px solid #F6F6F6; margin-top: 0; margin-bottom: 3.6rem;">

					<label>
						Slack client secret
						<span class="subtext">
							Paste the client secret for your Slack app
						</span>
						<input name="slack-client-secret" type="password" value="{{$.Settings.SlackClientSecret}}">
					</label>

					<label>
						Slack install URL
						<span class="subtext">
							Paste the shareable URL for your Slack app
						</span>
						<input name="slack-install-url" type="url" value="{{$.Settings.SlackInstallURL}}">
					</label>

					<div class="slack-container" style="display: block;">

						<a 
							class="help"
							href="https://api.slack.com/apps"
							target="_blank"
						>
							Go to your Slack apps
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
								<path d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z" />
								<path d="M3.5 6.75c0-.69.56-1.25 1.25-1.25H7A.75.75 0 0 0 7 4H4.75A2.75 2.75 0 0 0 2 6.75v4.5A2.75 2.75 0 0 0 4.75 14h4.5A2.75 2.75 0 0 0 12 11.25V9a.75.75 0 0 0-1.5 0v2.25c0 .69-.56 1.25-1.25 1.25h-4.5c-.69 0-1.25-.56-1.25-1.25v-4.5Z" />
							</svg>

						</a>

						<button 
							type="button"
							class="help"
							onclick="document.querySelector('.slack-tutorial').classList.toggle('slack-tutorial--visible');"
						>
							How do I create a Slack app?
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
								<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
							</svg>
						</button>

						<div class="slack-tutorial">
							<a href="https://api.slack.com/apps/new" target="_blank">
								<svg viewBox="0 0 124 124" fill="none" xmlns="http://www.w3.org/2000/svg">
									<path d="M26.3996 78.2003C26.3996 85.3003 20.5996 91.1003 13.4996 91.1003C6.39961 91.1003 0.599609 85.3003 0.599609 78.2003C0.599609 71.1003 6.39961 65.3003 13.4996 65.3003H26.3996V78.2003Z" fill="#E01E5A"/>
									<path d="M32.9004 78.2003C32.9004 71.1003 38.7004 65.3003 45.8004 65.3003C52.9004 65.3003 58.7004 71.1003 58.7004 78.2003V110.5C58.7004 117.6 52.9004 123.4 45.8004 123.4C38.7004 123.4 32.9004 117.6 32.9004 110.5V78.2003Z" fill="#E01E5A"/>
									<path d="M45.8004 26.4001C38.7004 26.4001 32.9004 20.6001 32.9004 13.5001C32.9004 6.4001 38.7004 0.600098 45.8004 0.600098C52.9004 0.600098 58.7004 6.4001 58.7004 13.5001V26.4001H45.8004Z" fill="#36C5F0"/>
									<path d="M45.7996 32.8999C52.8996 32.8999 58.6996 38.6999 58.6996 45.7999C58.6996 52.8999 52.8996 58.6999 45.7996 58.6999H13.4996C6.39961 58.6999 0.599609 52.8999 0.599609 45.7999C0.599609 38.6999 6.39961 32.8999 13.4996 32.8999H45.7996Z" fill="#36C5F0"/>
									<path d="M97.5996 45.7999C97.5996 38.6999 103.4 32.8999 110.5 32.8999C117.6 32.8999 123.4 38.6999 123.4 45.7999C123.4 52.8999 117.6 58.6999 110.5 58.6999H97.5996V45.7999Z" fill="#2EB67D"/>
									<path d="M91.0988 45.8001C91.0988 52.9001 85.2988 58.7001 78.1988 58.7001C71.0988 58.7001 65.2988 52.9001 65.2988 45.8001V13.5001C65.2988 6.4001 71.0988 0.600098 78.1988 0.600098C85.2988 0.600098 91.0988 6.4001 91.0988 13.5001V45.8001Z" fill="#2EB67D"/>
									<path d="M78.1988 97.6001C85.2988 97.6001 91.0988 103.4 91.0988 110.5C91.0988 117.6 85.2988 123.4 78.1988 123.4C71.0988 123.4 65.2988 117.6 65.2988 110.5V97.6001H78.1988Z" fill="#ECB22E"/>
									<path d="M78.1988 91.1003C71.0988 91.1003 65.2988 85.3003 65.2988 78.2003C65.2988 71.1003 71.0988 65.3003 78.1988 65.3003H110.499C117.599 65.3003 123.399 71.1003 123.399 78.2003C123.399 85.3003 117.599 91.1003 110.499 91.1003H78.1988Z" fill="#ECB22E"/>
								</svg>
								Create new Slack app
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
									<path d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z" />
									<path d="M3.5 6.75c0-.69.56-1.25 1.25-1.25H7A.75.75 0 0 0 7 4H4.75A2.75 2.75 0 0 0 2 6.75v4.5A2.75 2.75 0 0 0 4.75 14h4.5A2.75 2.75 0 0 0 12 11.25V9a.75.75 0 0 0-1.5 0v2.25c0 .69-.56 1.25-1.25 1.25h-4.5c-.69 0-1.25-.56-1.25-1.25v-4.5Z" />
								</svg>
							</a>

							<p>Select the "From scratch" option</p>
							<img 
								src="/static/images/slack-notification-tutorial/1.png"
								style="width: 60%;"
							/>

							<p>Name your app, choose your Slack workspace, and click “Create app”</p>
							<img 
								src="/static/images/slack-notification-tutorial/2.png"
								style="width: 60%;"
							/>
						</div>

						<button 
							type="button"
							class="help"
							onclick="document.querySelector('.slack-tutorial-app').classList.toggle('slack-tutorial-app--visible');"
						>
							How do I configure my Slack app for status page alerts?
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
								<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
							</svg>
						</button>

						<div class="slack-tutorial-app">
							<p>Turn incoming webhooks on - you don't need to add any webhooks</p>
							<img
								src="/static/images/slack-notification-tutorial/configure-1.png"
								style="width: 100%;"
							/>

							<p>
								Add the following redirect URL to your Slack app:
								https://{{.Domain}}/callback/slack
							</p>
							<img 
								src="/static/images/slack-notification-tutorial/configure-2.png"
								style="width: 100%;"
							/>

							<p>Copy the client secret and paste it into Statusnook</p>
							<img 
								src="/static/images/slack-notification-tutorial/configure-3.png"
								style="width: 100%;"
							/>

							<p>Copy the sharable URL and paste it into Statusnook</p>
							<img 
								src="/static/images/slack-notification-tutorial/configure-4.png"
								style="width: 100%;"
							/>
						</div>
					</div>
					
					<div>
						{{if not .Ctx.ConfigFile}}
							<button type="submit">Confirm</button>
						{{end}}
					</div>
				</form>
			</div>
			<script>
				{{if .ConfigFileEnabled}}
					[...document.querySelector("form").elements].forEach((v) => {
						if (v.tagName === "FIELDSET" || v.classList.contains("help")) {
							return;
						}
						v.disabled = true;
					});
				{{end}}

				function onChangeChannel(e) {
					const form = e.closest("form");

					form.elements["smtp-notification-channel"].forEach(v => {
						if (v !== e) {
							v.checked = false;
						}
					});
				}
			</script>
		{{end}}
	`

	tmpl, err := parseTmpl("getAlertNotifications", markup)
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
		Domain:                  metaDomain,
		ConfigFileEnabled:       metaConfigFileEnabled,
		Ctx:                     getPageCtx(r),
	})
	if err != nil {
		log.Printf("getAlertNotifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func updateAlertSettings(
	tx *sql.Tx,
	slackInstallURL string,
	slackClientSecret string,
	managedSubscriptions bool,
) error {
	const query = `
		insert into alert_setting(name, value) values(?, ?), (?, ?), (?, ?)
		on conflict(name) do update set value = excluded.value
	`

	_, err := tx.Exec(
		query,
		"slack-install-url",
		slackInstallURL,
		"slack-client-secret",
		slackClientSecret,
		"managed-subscriptions",
		managedSubscriptions,
	)
	if err != nil {
		return fmt.Errorf("updateAlertSettings.Exec: %w", err)
	}

	return nil
}

func updateAlertSMTPNotificationSetting(tx *sql.Tx, notificationID int) error {
	const deleteQuery = `
		delete from alert_setting_smtp_notification
	`

	_, err := tx.Exec(deleteQuery)
	if err != nil {
		return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecDelete: %w", err)
	}

	if notificationID != 0 {
		const insertQuery = `
			insert into alert_setting_smtp_notification(notification_channel_id) values(?)
		`

		_, err = tx.Exec(insertQuery, notificationID)
		if err != nil {
			return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecInsert: %w", err)
		}
	}

	return nil
}

func postAlertNotifications(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
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
