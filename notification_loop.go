package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UnsentAlertNotification struct {
	AlertNotificationID int
	Destination         string
	Content             string
	Type                string
	AlertMessageID      int
	AlertTitle          string
	AlertType           string
	AlertSeverity       string
	AlertServices       string
}

func listUnsentAlertNotifications(tx *sql.Tx) ([]UnsentAlertNotification, error) {
	const query = `
		select 
			alert_notification.id, alert_subscription.destination, alert_message.content, 
			alert_subscription.type, alert_message.id, alert.title, alert.type, alert.severity, 
			group_concat(service.name, " • ")
		from alert_notification
		left join alert_subscription on alert_subscription.id = alert_subscription_id
		left join alert_message on alert_message.id = alert_message_id
		left join alert on alert.id = alert_message.alert_id
		left join alert_service on alert_service.alert_id = alert_message.alert_id
		left join service on service.id = alert_service.service_id
		where alert_notification.sent_at is null
		group by alert_notification.id
		order by alert_message.created_at asc
	`

	notifications := []UnsentAlertNotification{}

	rows, err := tx.Query(query)
	if err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		notification := UnsentAlertNotification{}
		err := rows.Scan(
			&notification.AlertNotificationID,
			&notification.Destination,
			&notification.Content,
			&notification.Type,
			&notification.AlertMessageID,
			&notification.AlertTitle,
			&notification.AlertType,
			&notification.AlertSeverity,
			&notification.AlertServices,
		)
		if err != nil {
			return notifications, fmt.Errorf("listUnsentAlertNotifications.Scan: %w", err)
		}

		notifications = append(notifications, notification)
	}

	return notifications, nil
}

func notificationLoop(ctx context.Context, wg *sync.WaitGroup) {
	tick := time.Tick(time.Second * 10)
	for {
		select {
		case <-tick:
			func() {
				tx, err := db.Begin()
				if err != nil {
					log.Printf("notificationLoop.BeginListUnsentAlertNotifications: %s", err)
					return
				}
				defer tx.Rollback()

				notifications, err := listUnsentAlertNotifications(tx)
				if err != nil {
					log.Printf("notificationLoop.listUnsentAlertNotifications: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("notificationLoop.CommitListUnsentAlertNotifications: %s", err)
					return
				}

				for _, notification := range notifications {
					func() {
						tx, err := db.Begin()
						if err != nil {
							log.Printf("notificationLoop.ReadBegin: %s", err)
							return
						}
						defer tx.Rollback()

						notificationChannelID, err := getAlertSMTPNotificationSetting(tx)
						if err != nil {
							log.Printf("notificationLoop.getAlertSMTPNotificationSetting: %s", err)
							return
						}

						notificationChannel, err := getNotificationChannelByID(tx, notificationChannelID)
						if err != nil {
							log.Printf("notificationLoop.getNotificationChannelByID: %s", err)
							return
						}

						alertSettings, err := getAlertSettings(tx)
						if err != nil {
							log.Printf("notificationLoop.getAlertSettings: %s", err)
							return
						}

						emailSubs, err := listActiveAlertEmailSubscriptions(tx)
						if err != nil {
							log.Printf("notificationLoop.listActiveAlertEmailSubscriptions: %s", err)
							return
						}

						subTokensEmailMap := make(map[string]string, len(emailSubs))
						for _, v := range emailSubs {
							subTokensEmailMap[v.Destination] = v.Meta
						}

						err = tx.Commit()
						if err != nil {
							log.Printf("notificationLoop.ReadCommit: %s", err)
							return
						}

						severityEmoji := "🟠"
						if notification.AlertSeverity == "red" {
							severityEmoji = "🔴"
						}

						if notification.Type == "slack" {
							httpClient := http.Client{
								Timeout: time.Second * 10,
							}

							tmpl, err := parseTextTmpl("alert_slack.json")
							if err != nil {
								log.Printf("notificationLoop.parseEmailTmplsSlack: %s", err)
								return
							}

							notificationStr := bytes.Buffer{}

							err = tmpl.Execute(
								&notificationStr,
								struct {
									Title     string
									Content   string
									Services  string
									AlertType string
									Severity  string
									Domain    string
								}{
									Title: strings.ToUpper(notification.AlertType[:1]) +
										notification.AlertType[1:] + " - " + notification.AlertTitle,
									Content:   notification.Content,
									Services:  notification.AlertServices,
									AlertType: notification.AlertType,
									Severity:  severityEmoji,
									Domain:    metaDomain,
								},
							)
							if err != nil {
								log.Printf("notificationLoop.ExecuteSlack: %s", err)
								return
							}

							resp, err := httpClient.Post(
								notification.Destination,
								"application/json",
								&notificationStr,
							)
							if err != nil {
								log.Printf("notificationLoop.Post: %s", err)
								return
							}
							defer resp.Body.Close()

							tx, err := rwDB.Begin()
							if err != nil {
								log.Printf("notificationLoop.SlackBegin: %s", err)
								return
							}
							defer tx.Rollback()

							err = updateAlertSentAtByID(
								tx,
								time.Now().UTC(),
								[]int{notification.AlertNotificationID},
							)
							if err != nil {
								log.Printf("notificationLoop.updateAlertSentAtByID: %s", err)
								return
							}

							err = tx.Commit()
							if err != nil {
								log.Printf("notificationLoop.SlackCommit: %s", err)
								return
							}
						} else if notification.Type == "email" {
							smtpDetail, ok := notificationChannel.Details.(SMTPNotificationDetails)
							if !ok {
								log.Printf("notificationLoop.NotificationDetailsAssert: %s", err)
								return
							}

							msg := [][]byte{
								[]byte("Subject: " + metaName + " " + notification.AlertType +
									" alert: update regarding \"" + notification.AlertTitle + "\""),
								[]byte("To: " + notification.Destination),
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
								msg = append(
									msg,
									[]byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-broadcast"]),
								)
							}

							if alertSettings.ManagedSubscriptions {
								msg = append(
									msg,
									[]byte("List-Unsubscribe-Post: List-Unsubscribe=One-Click"),
									[]byte("List-Unsubscribe: "+
										"<https://"+metaDomain+
										"/unsubscribe?token="+subTokensEmailMap[notification.Destination]+">"),
								)
							}

							tmpl, err := parseEmailTmpl("alert_email.html")
							if err != nil {
								log.Printf("notificationLoop.parseEmailTmpls: %s", err)
								return
							}

							emailBytes := bytes.Buffer{}

							err = tmpl.Execute(
								&emailBytes,
								struct {
									Notification         UnsentAlertNotification
									SeverityEmoji        string
									Domain               string
									ManagedSubscriptions bool
									SubToken             string
								}{
									Notification:         notification,
									SeverityEmoji:        severityEmoji,
									Domain:               metaDomain,
									ManagedSubscriptions: alertSettings.ManagedSubscriptions,
									SubToken:             subTokensEmailMap[notification.Destination],
								},
							)
							if err != nil {
								log.Printf("notificationLoop.ExecuteSMTP: %s", err)
								return
							}

							emailStr := "\r\n" + emailBytes.String()

							msg = append(msg, []byte(emailStr))

							err = sendMail(
								smtpDetail.Host+":"+strconv.Itoa(smtpDetail.Port),
								PlainOrLoginAuth(
									smtpDetail.Username,
									smtpDetail.Password,
									smtpDetail.Host,
								),
								smtpDetail.From,
								[]string{notification.Destination},
								bytes.Join(msg, []byte("\r\n")),
							)
							if err != nil {
								log.Printf("notificationLoop.SendMail: %s", err)
								return
							}

							tx, err = rwDB.Begin()
							if err != nil {
								log.Printf("notificationLoop.BeginUpdateAlertSentAtByIDEmail: %s", err)
								return
							}
							defer tx.Rollback()

							err = updateAlertSentAtByID(
								tx,
								time.Now().UTC(),
								[]int{notification.AlertNotificationID},
							)
							if err != nil {
								log.Printf("notificationLoop.updateAlertSentAtByIDEmail: %s", err)
								return
							}

							err = tx.Commit()
							if err != nil {
								log.Printf("notificationLoop.CommitUpdateAlertSentAtByIDEmail: %s", err)
								return
							}
						}
					}()
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}
