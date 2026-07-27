package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type plainOrLoginAuth struct {
	username string
	password string
	host     string
	auth     string
}

func PlainOrLoginAuth(username string, password string, host string) smtp.Auth {
	return &plainOrLoginAuth{username: username, password: password, host: host}
}

func (a *plainOrLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("plainAuth.Start: unencrypted connection")
	}

	if slices.Contains(server.Auth, "PLAIN") {
		a.auth = "PLAIN"
		return smtp.PlainAuth("", a.username, a.password, a.host).Start(server)
	}

	if slices.Contains(server.Auth, "LOGIN") {
		a.auth = "LOGIN"
		return "LOGIN", []byte(a.username), nil
	}

	return "", nil, fmt.Errorf("plainAuth.Start: unhandled auth")
}

func (a *plainOrLoginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if a.auth == "PLAIN" {
		return smtp.PlainAuth("", a.username, a.password, a.host).Next(fromServer, more)
	}

	if a.auth == "LOGIN" && more {
		switch string(fromServer) {
		case "Username:":
			return []byte(a.username), nil
		case "Password:":
			return []byte(a.password), nil
		default:
			return nil, fmt.Errorf("plainAuth.Next: unexpected from server")
		}
	}
	return nil, nil
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
		[]byte("Subject: " + subject + " \"" +
			monitor.Name + "\""),
		[]byte("To: " + strings.Join(emailAddresses, ", ")),
		[]byte("From: " + metaName + " " + "<" + smtpDetail.From + ">"),
		[]byte("Content-Type: text/html; charset=UTF-8"),
	}
	for k, v := range smtpDetail.Headers {
		if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") &&
			k == "X-PM-Message-Stream" {
			continue
		}
		msg = append(msg, []byte(k+": "+v))
	}
	if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		msg = append(msg, []byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-transactional"]))
	}

	markupFile := "monitor_down_email.html"
	if status == "up" {
		markupFile = "monitor_up_email.html"
	}

	tmpl, err := parseEmailTmpl(markupFile)
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
			Domain:      metaDomain,
		},
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.Execute: %w", err)
	}

	emailStr := "\r\n" + emailBytes.String()

	msg = append(msg, []byte(emailStr))

	err = sendMail(
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

	markupFile := "monitor_down_slack.json"
	if status == "up" {
		markupFile = "monitor_up_slack.json"
	}

	tmpl, err := parseTextTmpl(markupFile)
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
			Domain:      metaDomain,
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
	resp.Body.Close()

	return nil
}
