package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"net/url"
	"strconv"
)

func getDetailsMonitor(w http.ResponseWriter, r *http.Request) {
	getEditMonitor(w, r)
}

func editMonitor(
	tx *sql.Tx,
	id int,
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
		update monitor set name = ?, url = ?, method = ?, frequency = ?, timeout = ?, 
			attempts = ?, request_headers = ?, body_format = ?, body = ?
		where id = ?
	`

	var monitorID int
	_, err := tx.Exec(
		query,
		name,
		url,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
		id,
	)
	if err != nil {
		return monitorID, fmt.Errorf("editMonitor.QueryRow: %w", err)
	}

	return id, nil
}

func updateMonitorSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update monitor set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateMonitorSlug.QueryRow: %w", err)
	}

	return id, nil
}

func postEditMonitor(w http.ResponseWriter, r *http.Request) {
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

	bodyFormat := sql.NullString{}
	if r.PostFormValue("format") != "" {
		bodyFormat = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("format"),
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
		log.Printf("postEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	idParam := chi.URLParam(r, "id")
	monitorID, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err = getMonitorByID(tx, monitorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Printf("postEditMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, err = editMonitor(
		tx,
		monitorID,
		name,
		reqURL,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
	)
	if err != nil {
		log.Printf("postEditMonitor.createMonitor: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorNotificationChannels(tx, monitorID, notificationChannels)
	if err != nil {
		log.Printf("postEditMonitor.updateMonitorNotificationChannels: %s", err)
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
		log.Printf("postEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors/"+idParam)
}

func deleteMonitorByID(tx *sql.Tx, id int) error {
	const query = `
		delete from monitor where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteMonitorByID.Exec: %w", err)
	}

	return nil
}

func deleteMonitor(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(chi.URLParam(r, "id"))

	tx, err := rwDB.Begin()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	err = deleteMonitorByID(tx, id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.deleteMonitorByID: %s", err)
		return
	}

	err = tx.Commit()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.Commit: %s", err)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors")
}
