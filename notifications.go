package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var emailTmplsMu sync.RWMutex
var emailTmpls = map[string]*template.Template{}

func parseEmailTmpl(name string, markup string) (*template.Template, error) {
	emailTmplsMu.RLock()
	tmpl, ok := emailTmpls[name]
	emailTmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}

	tmpl = template.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseEmailTmpl.Parse: %w", err)
	}

	emailTmplsMu.Lock()
	emailTmpls[name] = tmpl
	emailTmplsMu.Unlock()

	return tmpl, nil
}

// How often the queue is drained. A variable so a test can tick it faster than
// the thirty seconds go-toolchain gives a single test to finish.
var notificationLoopInterval = 10 * time.Second

func notificationLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(notificationLoopInterval)
	defer ticker.Stop()
	tick := ticker.C
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

				if len(notifications) == 0 {
					return
				}

				// Hoisted out of the per-notification loop below. All four are
				// constant for the batch, and listUnsentAlertNotifications
				// returns one row per SUBSCRIBER -- so reloading every active
				// subscription inside the loop made a single alert update
				// O(N^2) in subscribers, against the same file the monitor
				// loop is writing to.
				tx, err = db.Begin()
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

				for _, notification := range notifications {
					func() {
						severityEmoji := "🟠"
						if notification.AlertSeverity == "red" {
							severityEmoji = "🔴"
						}

						if notification.Type == "slack" {
							httpClient := http.Client{
								Timeout: time.Second * 10,
							}

							tmpl, err := parseTextTmpl("alertSlack", notificationLoopText)
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
									// Escaped: the template above is text/template, which
									// escapes nothing, so a quote in a title broke the JSON.
									Title: jsonString(strings.ToUpper(notification.AlertType[:1]) +
										notification.AlertType[1:] + " - " + notification.AlertTitle),
									Content:   jsonString(notification.Content),
									Services:  jsonString(notification.AlertServices),
									AlertType: notification.AlertType,
									Severity:  jsonString(severityEmoji),
									Domain:    jsonString(metaDomain.Load()),
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
							slackBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
							resp.Body.Close()
							if readErr != nil {
								log.Printf("notificationLoop.ReadAllSlack: %s", readErr)
								return
							}

							// Returning here leaves sent_at null, so the next tick
							// retries. Stamping it on a 404 from a revoked webhook
							// dropped the alert permanently and reported nothing.
							if resp.StatusCode != http.StatusOK {
								log.Printf(
									"notificationLoop.PostStatusCode %d: %s",
									resp.StatusCode,
									strings.TrimSpace(string(slackBody)),
								)
								return
							}

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
								log.Printf(
									"notificationLoop.NotificationDetailsAssert: channel %d is not SMTP",
									notificationChannel.ID,
								)
								return
							}

							msg := [][]byte{
								[]byte("Subject: " + headerValue(metaName.Load()) + " " + notification.AlertType +
									" alert: update regarding \"" + headerValue(notification.AlertTitle) + "\""),
								[]byte("To: " + headerValue(notification.Destination)),
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
										"<https://"+metaDomain.Load()+
										"/unsubscribe?token="+subTokensEmailMap[notification.Destination]+">"),
								)
							}

							tmpl, err := parseEmailTmpl("alert", notificationLoopMarkup)
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
									Domain:               metaDomain.Load(),
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

							err = smtp.SendMail(
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

							tx, err := rwDB.Begin()
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

type StatusnookConfigSlackNotificationChannel struct {
	WebhookURL string `json:"webhookURL" yaml:"webhook-url"`
}

type StatusnookConfigSMTPNotificationChannel struct {
	Host     string            `json:"host" yaml:"host"`
	Port     int               `json:"port" yaml:"port"`
	Username string            `json:"username" yaml:"username"`
	Password string            `json:"password" yaml:"password"`
	From     string            `json:"from" yaml:"from"`
	Headers  map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Misc     map[string]string `json:"misc,omitempty" yaml:"misc,omitempty"`
}

type StatusnookConfigMailGroup struct {
	Name        string   `json:"name" yaml:"name"`
	Members     []string `json:"members,omitempty" yaml:"members,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
}

func postmarkDeleteSuppression(email string, token string, stream string) error {
	// Marshalled, not Sprintf'd: email arrives from an unauthenticated form,
	// and mail.ParseAddress accepts quoted local parts containing escaped
	// quotes, which broke straight out of the string literal.
	bodyBytes, err := json.Marshal(struct {
		Suppressions []struct {
			EmailAddress string `json:"EmailAddress"`
		} `json:"Suppressions"`
	}{
		Suppressions: []struct {
			EmailAddress string `json:"EmailAddress"`
		}{{EmailAddress: email}},
	})
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.Marshal: %w", err)
	}
	body := string(bodyBytes)

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodPost,
		postmarkAPIBaseURL+"/message-streams/"+stream+"/suppressions/delete",
		strings.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.NewRequest: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Postmark-Server-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.Do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.ReadAll: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("postmarkDeleteSuppression.StatusCode: %s", string(respBody))
	}

	return nil
}

func postmarkDumpSupressions(token string, stream string) (SupressionDumpResponse, error) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	var supressionsResp SupressionDumpResponse

	req, err := http.NewRequest(
		http.MethodGet,
		postmarkAPIBaseURL+"/message-streams/"+stream+"/suppressions/dump"+
			"?SupressionReason=ManualSuppression",
		nil,
	)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.NewRequest: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Postmark-Server-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.Do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.ReadAll: %w", err)
	}

	if resp.StatusCode != 200 {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.StatusCode: %s", string(body))
	}

	err = json.Unmarshal(body, &supressionsResp)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.Unmarshal: %w", err)
	}

	return supressionsResp, nil
}
