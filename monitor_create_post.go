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

func updateMonitorNotificationChannels(tx *sql.Tx, monitorID int, channelIDs []int) error {
	const deleteQuery = `
		delete from monitor_notification_channel where monitor_id = ?
	`

	_, err := tx.Exec(deleteQuery, monitorID)
	if err != nil {
		return fmt.Errorf("updateMonitorNotificationChannels.DeleteExec: %w", err)
	}

	if len(channelIDs) > 0 {
		const baseInsertQuery = `
			insert into monitor_notification_channel(monitor_id, notification_channel_id)
			values
		`

		insertQuery := baseInsertQuery

		for i := range channelIDs {
			insertQuery += "(?, ?)"

			if i != len(channelIDs)-1 {
				insertQuery += ","
			}
		}

		params := []any{}
		for _, v := range channelIDs {
			params = append(params, monitorID, v)
		}

		_, err = tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMonitorNotificationChannels.InsertExec: %w", err)
		}
	}

	return nil
}

func createMonitor(
	tx *sql.Tx,
	slug string,
	name string,
	url string,
	method string,
	frequency int,
	timeout int,
	attempts int,
	requestHeaders sql.NullString,
	bodyFormat sql.NullString,
	body sql.NullString,
) (int, error) {
	const query = `
		insert into
			monitor(slug, name, url, method, frequency, timeout, attempts, request_headers, 
				body_format, body)
			values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id
	`

	var monitorID int
	err := tx.QueryRow(
		query,
		slug,
		name,
		url,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
	).Scan(&monitorID)
	if err != nil {
		return monitorID, fmt.Errorf("createMonitor.QueryRow: %w", err)
	}

	return monitorID, nil
}

func postCreateMonitor(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
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
		for i := range r.Form["header-key"] {
			requestHeadersMap[r.Form["header-key"][i]] = r.Form["header-value"][i]
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
		for i := 0; i < len(r.Form["form-key"]); i++ {
			urlValues.Add(r.Form["form-key"][i], r.Form["form-value"][i])
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
