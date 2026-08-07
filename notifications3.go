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

	"github.com/go-chi/chi/v5"
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

	tmpl, err := parseTmpl("notifications", notificationsMarkup)
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

	tmpl, err := parseTmpl("getCreateNotification", getCreateNotificationMarkup)
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

	if err := rows.Err(); err != nil {
		return channels, fmt.Errorf("listNotificationChannels.RowsErr: %w", err)
	}

	return channels, nil
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
	if metaConfigFileEnabled.Load() {
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
			keys := r.PostForm["header-key"]
			values := r.PostForm["header-value"]
			for i := 0; i < len(keys) && i < len(values); i++ {
				headers[keys[i]] = values[i]
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
			// ParseRequestURI returns a nil URL alongside the error, and the
			// code below calls String() on it.
			w.WriteHeader(http.StatusBadRequest)
			return
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

func getEditNotification(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled.Load() {
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

	tmpl, err := parseTmpl("getEditNotification", getEditNotificationMarkup)
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

func editNotificationChannel(tx *sql.Tx, channel NotificationChannel) error {
	const query = `
		update notification_channel set name = ?, details = ?
		where id = ?
	`

	_, err := tx.Exec(query, channel.Name, channel.Details, channel.ID)
	if err != nil {
		return fmt.Errorf("editNotificationChannel.Exec: %w", err)
	}

	return nil
}

func updateNotificationChannelSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update notification_channel set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateNotificationChannelSlug.QueryRow: %w", err)
	}

	return id, nil
}
