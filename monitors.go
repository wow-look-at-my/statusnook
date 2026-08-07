package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func createMonitorLog(
	tx *sql.Tx,
	startedAt time.Time,
	endedAt time.Time,
	responseCode int64,
	errorMessage sql.NullString,
	attempts int,
	result string,
	monitorID int,
) (int, error) {
	const query = `
		insert into 
			monitor_log(started_at, ended_at, response_code, error_message, 
				attempts, result, monitor_id)
			values(?, ?, ?, ?, ?, ?, ?)
		returning id
	`

	var id int

	err := tx.QueryRow(
		query,
		startedAt,
		endedAt,
		responseCode,
		errorMessage,
		attempts,
		result,
		monitorID,
	).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createMonitorLog.Exec: %w", err)
	}

	return id, nil
}

func createMonitorLogLastChecked(
	tx *sql.Tx,
	startedAt time.Time,
	monitorID int,
	monitorLogID int,
) error {
	const query = `
		insert into monitor_log_last_checked(checked_at, monitor_id, monitor_log_id)
			values(?, ?, ?) 
		on conflict(monitor_id) do update set 
			checked_at = excluded.checked_at,
			monitor_log_id = excluded.monitor_log_id
	`

	_, err := tx.Exec(
		query,
		startedAt,
		monitorID,
		monitorLogID,
	)
	if err != nil {
		return fmt.Errorf("createMonitorLogLastChecked.Exec: %w", err)
	}

	return nil
}

type monitorLogLastChecked struct {
	ID           int
	CheckedAt    time.Time
	ResponseCode sql.NullInt32
}

func getMonitorLogLastChecked(
	tx *sql.Tx,
	monitorID int,
) (monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
		where monitor_log_last_checked.monitor_id = ?
	`

	var lastChecked monitorLogLastChecked

	err := tx.QueryRow(query, monitorID).Scan(
		&lastChecked.ID,
		&lastChecked.CheckedAt,
		&lastChecked.ResponseCode,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lastChecked, nil
		}
		return lastChecked, fmt.Errorf("getMonitorLogLastChecked.QueryRow: %w", err)
	}

	return lastChecked, nil
}

func listAllMonitorLogLastChecked(tx *sql.Tx) ([]monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
	`

	var allLastChecked []monitorLogLastChecked

	rows, err := tx.Query(query)
	if err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var lastChecked monitorLogLastChecked

		err := rows.Scan(
			&lastChecked.ID,
			&lastChecked.CheckedAt,
			&lastChecked.ResponseCode,
		)
		if err != nil {
			return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Scan: %w", err)
		}

		allLastChecked = append(allLastChecked, lastChecked)
	}

	if err := rows.Err(); err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.RowsErr: %w", err)
	}

	return allLastChecked, nil
}

func sendMonitorAlertEmail(
	monitor Monitor,
	channel NotificationChannel,
	statusCode sql.NullInt64,
	result string,
	startedAt time.Time,
	emailAddresses []string,
	status string,
) error {
	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if !ok {
		return fmt.Errorf(
			"sendMonitorAlertEmail.SMTPAssert: failed to assert channel %d",
			channel.ID,
		)
	}

	smtpAuth := PlainOrLoginAuth(
		smtpDetail.Username,
		smtpDetail.Password,
		smtpDetail.Host,
	)

	const downSubject = "🚨 Issue detected on monitor"
	const upSubject = "✅ Issue resolved on monitor"

	subject := downSubject
	if status == "up" {
		subject = upSubject
	}

	msg := [][]byte{
		[]byte("Subject: " + headerValue(subject) + " \"" +
			headerValue(monitor.Name) + "\""),
		[]byte("To: " + headerValue(strings.Join(emailAddresses, ", "))),
		[]byte("From: " + headerValue(metaName.Load()) + " " + "<" + smtpDetail.From + ">"),
		[]byte("Content-Type: text/html; charset=UTF-8"),
	}
	for k, v := range smtpDetail.Headers {
		if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") &&
			k == "X-PM-Message-Stream" {
			continue
		}
		msg = append(msg, []byte(headerValue(k)+": "+headerValue(v)))
	}
	if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		msg = append(msg, []byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-transactional"]))
	}

	markup := sendMonitorAlertEmailDownMarkup
	if status == "up" {
		markup = sendMonitorAlertEmailUpMarkup
	}

	tmpl, err := parseEmailTmpl(status+"MonitorSMTP", markup)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.parseEmailTmplsSMTP: %w", err)
	}

	emailBytes := bytes.Buffer{}

	err = tmpl.Execute(
		&emailBytes,
		struct {
			MonitorID   int
			MonitorName string
			StatusCode  int
			CheckedAt   string
			Result      string
			Domain      string
		}{
			MonitorID:   monitor.ID,
			MonitorName: monitor.Name,
			StatusCode:  int(statusCode.Int64),
			CheckedAt:   startedAt.Format("2006/01/02 15:04:05 MST"),
			Result:      result,
			Domain:      metaDomain.Load(),
		},
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.Execute: %w", err)
	}

	emailStr := "\r\n" + emailBytes.String()

	msg = append(msg, []byte(emailStr))

	err = smtp.SendMail(
		smtpDetail.Host+":"+strconv.Itoa(smtpDetail.Port),
		smtpAuth,
		smtpDetail.From,
		emailAddresses,
		bytes.Join(msg, []byte("\r\n")),
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.SendMail: %w", err)
	}

	return nil
}

func sendMonitorAlertSlack(
	monitor Monitor,
	channel NotificationChannel,
	statusCode sql.NullInt64,
	startedAt time.Time,
	result string,
	httpClient http.Client,
	status string,
	domain string,
) error {
	slackDetail, ok := channel.Details.(SlackNotificationDetails)
	if !ok {
		return fmt.Errorf(
			"sendMonitorAlertSlack.SlackAssert: failed to assert channel %d",
			channel.ID,
		)
	}

	markup := sendMonitorAlertSlackDownMarkup
	if status == "up" {
		markup = sendMonitorAlertSlackUpMarkup
	}

	tmpl, err := parseTextTmpl(status+"MonitorSlack", markup)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.parseEmailTmplsSlack: %w", err)
	}

	emailStr := bytes.Buffer{}

	err = tmpl.Execute(
		&emailStr,
		struct {
			MonitorID   int
			MonitorName string
			StatusCode  int
			CheckedAt   string
			Result      string
			Domain      string
		}{
			MonitorID:   monitor.ID,
			MonitorName: monitor.Name,
			StatusCode:  int(statusCode.Int64),
			CheckedAt:   startedAt.Format("2006/01/02 15:04:05 MST"),
			Result:      result,
			Domain:      metaDomain.Load(),
		},
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.Execute: %w", err)
	}

	type SlackWebhookRequestBody struct {
		Text string `json:"text"`
	}

	body := SlackWebhookRequestBody{Text: emailStr.String()}

	serializedBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.MarshalSlack: %w", err)
	}

	resp, err := httpClient.Post(
		slackDetail.WebhookURL,
		"application/json",
		bytes.NewBuffer(serializedBody),
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.Post: %w", err)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("sendMonitorAlertSlack.ReadAll: %w", readErr)
	}

	// A revoked webhook or an archived channel answers 404 invalid_token /
	// 410 channel_is_archived, which is a perfectly successful HTTP round
	// trip. Not checking it meant every alert to that channel was dropped in
	// silence.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"sendMonitorAlertSlack.StatusCode %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody)),
		)
	}

	return nil
}
