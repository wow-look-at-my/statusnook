package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
)

func postCreateMonitor(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	reqURL := r.PostFormValue("url")
	validURL := true
	parsedReqURL, err := url.Parse(reqURL)
	if err != nil {
		validURL = false
	} else if parsedReqURL.Scheme == "" || parsedReqURL.Host == "" {
		validURL = false
	} else if parsedReqURL.Scheme != "http" && parsedReqURL.Scheme != "https" {
		validURL = false
	}

	if !validURL {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<span id="alert-error" hx-swap-oob="true">
				Invalid URL
			</span>
		`))
		return
	}

	method := r.PostFormValue("method")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if method != http.MethodGet &&
		method != http.MethodPost &&
		method != http.MethodPatch &&
		method != http.MethodPut &&
		method != http.MethodDelete {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	frequency, err := strconv.Atoi(r.PostFormValue("frequency"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if frequency != 10 && frequency != 30 && frequency != 60 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	timeout, err := strconv.Atoi(r.PostFormValue("timeout"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if timeout != 5 && timeout != 10 && timeout != 15 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	attempts, err := strconv.Atoi(r.PostFormValue("attempts"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if attempts != 1 && attempts != 2 && attempts != 3 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	requestHeaders := sql.NullString{}
	requestHeadersMap := map[string]string{}
	if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
		// PostForm, not Form: Form merges the query string in, so
		// ?header-key=x on the URL injected an entry and skewed the two
		// slices against each other. Bounded by the shorter of the two --
		// indexing values by the keys' length panicked on any mismatch.
		keys := r.PostForm["header-key"]
		values := r.PostForm["header-value"]
		for i := 0; i < len(keys) && i < len(values); i++ {
			requestHeadersMap[keys[i]] = values[i]
		}
	}
	requestHeadersSerialized, err := json.Marshal(requestHeadersMap)
	if err != nil {
		log.Printf("postEditMonitor.Marshal: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(requestHeadersMap) > 0 {
		requestHeaders = sql.NullString{
			Valid:  true,
			String: string(requestHeadersSerialized),
		}
	}

	body := sql.NullString{}
	if r.PostFormValue("body") != "" {
		body = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("body"),
		}
	}

	if r.PostFormValue("form-key") != "" && r.PostFormValue("form-value") != "" {
		urlValues := url.Values{}
		formKeys := r.PostForm["form-key"]
		formValues := r.PostForm["form-value"]
		for i := 0; i < len(formKeys) && i < len(formValues); i++ {
			urlValues.Add(formKeys[i], formValues[i])
		}
		body = sql.NullString{
			Valid:  true,
			String: urlValues.Encode(),
		}
	}

	format := sql.NullString{}
	if r.PostFormValue("format") != "" {
		format = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("format"),
		}
	}

	notificationChannelsParam := r.PostForm["notification-channels"]
	notificationChannels := make([]int, 0, len(notificationChannelsParam))
	for _, channelID := range notificationChannelsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		notificationChannels = append(notificationChannels, id)
	}

	mailGroupsParam := r.PostForm["mail-groups"]
	mailGroups := make([]int, 0, len(mailGroupsParam))
	for _, channelID := range mailGroupsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		mailGroups = append(mailGroups, id)
	}
	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	if err != nil {
		log.Printf("postCreateMonitor.listMonitors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorSlugs := map[string]bool{}
	for _, v := range monitors {
		monitorSlugs[v.Slug] = true
	}

	monitorID, err := createMonitor(
		tx,
		generateSlug(name, monitorSlugs),
		name,
		reqURL,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		format,
		body,
	)
	if err != nil {
		log.Printf("postCreateMonitor.createMonitor: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorNotificationChannels(tx, monitorID, notificationChannels)
	if err != nil {
		log.Printf("postCreateMonitor.updateMonitorNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorMailGroups(tx, monitorID, mailGroups)
	if err != nil {
		log.Printf("postEditMonitor.updateMonitorMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors/"+strconv.Itoa(monitorID))
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

	if err := rows.Err(); err != nil {
		return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.RowsErr: %w", err)
	}

	return notifications, nil
}

func updateMonitorMailGroups(tx *sql.Tx, monitorID int, mailGroupIDs []int) error {
	const deleteQuery = `
		delete from mail_group_monitor where monitor_id = ?
	`

	_, err := tx.Exec(deleteQuery, monitorID)
	if err != nil {
		return fmt.Errorf("updateMonitorMailGroups.ExecDelete: %w", err)
	}

	if len(mailGroupIDs) > 0 {
		const baseInsertQuery = `
			insert into mail_group_monitor(mail_group_id, monitor_id) values
		`

		insertQuery := baseInsertQuery

		params := []any{}

		for i, v := range mailGroupIDs {
			insertQuery += "(?, ?)"
			if i < len(mailGroupIDs)-1 {
				insertQuery += ","
			}
			params = append(params, v, monitorID)
		}

		_, err = tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMonitorMailGroups.ExecInsert: %w", err)
		}
	}

	return nil
}

func listMailGroupIDsByMonitorID(tx *sql.Tx, monitorID int) ([]MailGroupIDs, error) {
	const query = `
		select mail_group_id, slug from mail_group_monitor 
		left join mail_group on mail_group.id = mail_group_id
		where monitor_id = ?
	`

	allIds := []MailGroupIDs{}

	rows, err := tx.Query(query, monitorID)
	if err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ids MailGroupIDs
		err = rows.Scan(&ids.ID, &ids.Slug)
		if err != nil {
			return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Scan: %w", err)
		}

		allIds = append(allIds, ids)
	}

	if err := rows.Err(); err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.RowsErr: %w", err)
	}

	return allIds, nil
}

func listMailGroupMembersEmailsByMonitorID(tx *sql.Tx, id int) ([]string, error) {
	const query = `
		select distinct email_address from mail_group_member
		left join mail_group on mail_group.id = mail_group_member.mail_group_id
		left join mail_group_monitor on mail_group_monitor.mail_group_id = mail_group.id
		where mail_group_monitor.monitor_id = ?
	`

	emails := []string{}

	rows, err := tx.Query(query, id)
	if err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var email string
		err = rows.Scan(&email)
		if err != nil {
			return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Scan: %w", err)
		}

		emails = append(emails, email)
	}

	if err := rows.Err(); err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.RowsErr: %w", err)
	}

	return emails, nil
}
