package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

type SMTPNotificationDetails struct {
	Host     string            `json:"host"`
	Port     int               `json:"port"`
	Username string            `json:"username"`
	Password string            `json:"password"`
	From     string            `json:"from"`
	Headers  map[string]string `json:"headers"`
	Misc     map[string]string `json:"misc"`
}

type SlackNotificationDetails struct {
	WebhookURL string `json:"webhookURL"`
}

type NotificationChannel struct {
	ID      int
	Slug    string
	Name    string
	Type    string
	Details any
}

type listNotificationsOptions struct {
	Type string
}

func listNotificationChannels(tx *sql.Tx, options listNotificationsOptions) ([]NotificationChannel, error) {
	const baseQuery = `
		select id, slug, name, type, details from notification_channel
	`

	query := baseQuery

	params := []any{}

	if options.Type != "" {
		query += " where type = ?"
		params = append(params, options.Type)
	}

	var channels []NotificationChannel

	rows, err := tx.Query(query, params...)
	if err != nil {
		return channels, fmt.Errorf("listNotificationChannels.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var detailsStr string
		var channel NotificationChannel

		err := rows.Scan(&channel.ID, &channel.Slug, &channel.Name, &channel.Type, &detailsStr)
		if err != nil {
			return channels, fmt.Errorf("listNotificationChannels.Scan: %w", err)
		}

		if channel.Type == "smtp" {
			var details SMTPNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return channels, fmt.Errorf("listNotificationChannels.UnmarshalSMTP: %w", err)
			}

			channel.Details = details
		} else if channel.Type == "slack" {
			var details SlackNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return channels, fmt.Errorf("listNotificationChannels.UnmarshalSlack: %w", err)
			}

			channel.Details = details
		}

		channels = append(channels, channel)
	}

	return channels, nil
}

func listNotificationChannelsByMonitorID(tx *sql.Tx, monitorID int) ([]NotificationChannel, error) {
	const query = `
		select notification_channel.id, notification_channel.slug, 
		notification_channel.name, notification_channel.type, notification_channel.details 
		from monitor_notification_channel
		left join notification_channel on 
			notification_channel.id = monitor_notification_channel.notification_channel_id
		where monitor_notification_channel.monitor_id = ?
	`

	var notifications []NotificationChannel

	rows, err := tx.Query(query, monitorID)
	if err != nil {
		return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var detailsStr string
		var channel NotificationChannel

		err := rows.Scan(&channel.ID, &channel.Slug, &channel.Name, &channel.Type, &detailsStr)
		if err != nil {
			return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.Scan: %w", err)
		}

		if channel.Type == "smtp" {
			var details SMTPNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.UnmarshalSMTP: %w", err)
			}

			channel.Details = details
		} else if channel.Type == "slack" {
			var details SlackNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.UnmarshalSlack: %w", err)
			}

			channel.Details = details
		}

		notifications = append(notifications, channel)
	}

	return notifications, nil
}

func createNotification(
	tx *sql.Tx,
	slug string,
	name string,
	notificationType string,
	details string,
) error {
	const query = `
		insert into notification_channel(slug, name, type, details) values(?, ?, ?, ?)
	`

	_, err := tx.Exec(query, slug, name, notificationType, details)
	if err != nil {
		return fmt.Errorf("createNotification.Exec: %w", err)
	}

	return nil
}

func postCreateNotification(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	notificationType := r.PostFormValue("type")
	if notificationType != "smtp" && notificationType != "slack" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	displayName := r.PostFormValue("display-name")
	if displayName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if notificationType == "smtp" {
		host := r.PostFormValue("host")
		if host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		port := r.PostFormValue("port")
		if port == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		portNum, err := strconv.Atoi(port)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		username := r.PostFormValue("username")
		if username == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		password := r.PostFormValue("password")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		from := r.PostFormValue("from")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err = mail.ParseAddress(from)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		headers := map[string]string{}
		if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
			for i := 0; i < len(r.Form["header-key"]); i++ {
				headers[r.Form["header-key"][i]] = r.Form["header-value"][i]
			}
		}

		misc := map[string]string{}
		if strings.EqualFold(host, "smtp.postmarkapp.com") {
			txStream := r.PostFormValue("pm-transactional")
			bStream := r.PostFormValue("pm-broadcast")

			if txStream == "" || bStream == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			misc["pm-transactional"] = txStream
			misc["pm-broadcast"] = bStream
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postCreateNotification.BeginSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		details := SMTPNotificationDetails{
			Host:     host,
			Port:     portNum,
			Username: username,
			Password: password,
			From:     from,
			Headers:  headers,
			Misc:     misc,
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		channels, err := listNotificationChannels(tx, listNotificationsOptions{})
		if err != nil {
			log.Printf("postCreateNotification.listNotificationChannelsSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		channelSlugs := map[string]bool{}
		for _, v := range channels {
			channelSlugs[v.Slug] = true
		}

		err = createNotification(
			tx,
			generateSlug(displayName, channelSlugs),
			displayName,
			notificationType,
			string(serializedDetails),
		)
		if err != nil {
			log.Printf("postCreateNotification.createNotificationSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("postCreateNotification.CommitSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if notificationType == "slack" {
		webhookURL, err := url.ParseRequestURI(r.PostFormValue("webhook-url"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postCreateNotification.BeginSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		details := SlackNotificationDetails{
			WebhookURL: webhookURL.String(),
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		channels, err := listNotificationChannels(tx, listNotificationsOptions{})
		if err != nil {
			log.Printf("postCreateNotification.listNotificationChannelsSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		channelSlugs := map[string]bool{}
		for _, v := range channels {
			channelSlugs[v.Slug] = true
		}

		err = createNotification(
			tx,
			generateSlug(displayName, channelSlugs),
			displayName,
			notificationType,
			string(serializedDetails),
		)
		if err != nil {
			log.Printf("postCreateNotification.createNotificationSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("postCreateNotification.CommitSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getNotificationChannelByID(tx *sql.Tx, id int) (NotificationChannel, error) {
	const query = `
		select id, slug, name, type, details from notification_channel
		where id = ?
	`

	var channel NotificationChannel
	var detailsStr string

	err := tx.QueryRow(query, id).Scan(
		&channel.ID,
		&channel.Slug,
		&channel.Name,
		&channel.Type,
		&detailsStr,
	)
	if err != nil {
		return channel, fmt.Errorf("getNotificationChannelByID.QueryRow: %w", err)
	}

	if channel.Type == "smtp" {
		var details SMTPNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelByID.UnmarshalSMTP: %w", err)
		}

		channel.Details = details
	} else if channel.Type == "slack" {
		var details SlackNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelByID.UnmarshalSlack: %w", err)
		}

		channel.Details = details
	}

	return channel, nil
}

func getNotificationChannelBySlug(tx *sql.Tx, slug string) (NotificationChannel, error) {
	const query = `
		select id, slug, name, type, details from notification_channel
		where slug = ?
	`

	var channel NotificationChannel
	var detailsStr string

	err := tx.QueryRow(query, slug).Scan(
		&channel.ID,
		&channel.Slug,
		&channel.Name,
		&channel.Type,
		&detailsStr,
	)
	if err != nil {
		return channel, fmt.Errorf("getNotificationChannelBySlug.QueryRow: %w", err)
	}

	if channel.Type == "smtp" {
		var details SMTPNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelBySlug.UnmarshalSMTP: %w", err)
		}

		channel.Details = details
	} else if channel.Type == "slack" {
		var details SlackNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelBySlug.UnmarshalSlack: %w", err)
		}

		channel.Details = details
	}

	return channel, nil
}
