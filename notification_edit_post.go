package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

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

func postEditNotification(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	idParam := chi.URLParam(r, "id")
	notificationID, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditNotification.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, notificationID)
	if err != nil {
		log.Printf("postEditNotification.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	displayName := r.PostFormValue("display-name")
	if displayName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if channel.Type == "smtp" {
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

		details := SMTPNotificationDetails{
			Host:     host,
			Port:     portNum,
			Username: username,
			Password: password,
			Headers:  headers,
			From:     from,
			Misc:     misc,
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if channel.Type == "slack" {
		webhookURL, err := url.ParseRequestURI(r.PostFormValue("webhook-url"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
		}

		details := SlackNotificationDetails{
			WebhookURL: webhookURL.String(),
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditNotification.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getViewNotification(w http.ResponseWriter, r *http.Request) {
	getEditNotification(w, r)
}

func deleteNotificationChannelByID(tx *sql.Tx, id int) error {
	const query = `
		delete from notification_channel where id = $1
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteNotificationChannelByID.Exec: %w", err)
	}

	return nil
}

func deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteNotificationChannel.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteNotificationChannelByID(tx, id)
	if err != nil {
		log.Printf("deleteNotificationChannel.deleteNotificationChannelByID: %s", err)
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteNotificationChannel.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}
