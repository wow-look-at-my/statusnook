package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

func postSubscribeEmail(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")

	_, err := mail.ParseAddress(email)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Subscribing sends mail to an address the caller chose, so the endpoint is
	// a mail relay for anyone who can reach the status page. The per-address
	// throttle below does not stop someone cycling through addresses.
	now := time.Now().UTC()
	key := "subscribe:" + clientIP(r)
	if ok, retryAfter := subscribeLimiter.allow(key, now); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	subscribeLimiter.fail(key, now)

	supressionSyncMu.Lock()
	if time.Since(lastSuppressionSync) > time.Second*10 {
		tx, err := db.Begin()
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.BeginSuppressionsPrep: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		alertNotificationChannelID, err := getAlertSMTPNotificationSetting(tx)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getAlertSMTPNotificationSetting: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		notificationChannel, err := getNotificationChannelByID(tx, alertNotificationChannelID)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getNotificationChannelByID: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		smtpDetail, ok := notificationChannel.Details.(SMTPNotificationDetails)
		if !ok {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.ChannelAssert: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		alertSettings, err := getAlertSettings(tx)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getAlertSettings: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = tx.Commit()
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.CommitBeginSuppressionsPrep: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !alertSettings.ManagedSubscriptions &&
			strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
			supressions, err := postmarkDumpSupressions(smtpDetail.Password, smtpDetail.Misc["pm-broadcast"])
			if err != nil {
				supressionSyncMu.Unlock()
				log.Printf("postSubscribeEmail.postmarkDumpSupressions: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			for _, v := range supressions.Suppressions {
				tx, err := rwDB.Begin()
				if err != nil {
					supressionSyncMu.Unlock()
					log.Printf("postSubscribeEmail.BeginSuppressions: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				defer tx.Rollback()

				subscription, err := getAlertSubscriptionByEmail(tx, email)
				if err != nil {
					if !errors.Is(err, sql.ErrNoRows) {
						supressionSyncMu.Unlock()
						log.Printf("postSubscribeEmail.getAlertSubscriptionByEmail: %s", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}

				if subscription.ID != 0 {
					err = updateEmailAlertSubscriptionActiveByEmail(tx, v.EmailAddress, false)
					if err != nil {
						supressionSyncMu.Unlock()
						log.Printf(
							"postSubscribeEmail.updateEmailAlertSubscriptionActiveByEmail: %s",
							err,
						)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}

				err = tx.Commit()
				if err != nil {
					supressionSyncMu.Unlock()
					log.Printf("postSubscribeEmail.CommitSuppressions: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
		}
		lastSuppressionSync = time.Now().UTC()
	}
	supressionSyncMu.Unlock()

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSubscribeEmail.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	sub, err := getAlertSubscriptionByEmail(tx, email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postSubscribeEmail.getAlertSubscriptionByEmail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if sub.Active {
		w.Write(renderFragment("fragment_already_subscribed.html", nil))
		return
	}

	hasRecentPendingSub, err := checkHasRecentPendingEmailAlertSubscription(tx, email, time.Now().UTC())
	if err != nil {
		log.Printf("postSubscribeEmail.checkHasRecentPendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if hasRecentPendingSub {
		writeTemplate(w, "subscribe_email_pending.html")
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSubscribeEmail.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	token := base64.URLEncoding.EncodeToString(tokenBytes)

	err = createPendingEmailAlertSubscription(tx, token, email, time.Now().UTC())
	if err != nil {
		log.Printf("postSubscribeEmail.createPendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	notificationID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil {
		log.Printf("postSubscribeEmail.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	channel, err := getNotificationChannelByID(tx, notificationID)
	if err != nil {
		log.Printf("postSubscribeEmail.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if !ok {
		log.Printf("postSubscribeEmail.NotificationDetailsAssert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	msg := [][]byte{
		[]byte("Subject: Confirm your subscription to " + metaName + " status alerts"),
		[]byte("To: " + email),
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

	tmpl, err := parseEmailTmpl("alert_confirm_email.html")
	if err != nil {
		log.Printf("postSubscribeEmail.parseEmailTmpls: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	protocol := "https"
	if BUILD == "dev" {
		protocol = "http"
	}

	emailBytes := bytes.Buffer{}

	err = tmpl.Execute(
		&emailBytes,
		struct {
			Name string
			Link string
		}{
			Name: metaName,
			Link: protocol + "://" + metaDomain + "/subscribe/email/confirm?token=" + token,
		},
	)
	if err != nil {
		log.Printf("postSubscribeEmail.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
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
		[]string{email},
		bytes.Join(msg, []byte("\r\n")),
	)
	if err != nil {
		log.Printf("postSubscribeEmail.SendMail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSubscribeEmail.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeTemplate(w, "subscribe_email_pending.html")
}

func updatePendingEmailAlertSubscription(tx *sql.Tx, confirmedAt time.Time, token string) error {
	const query = `
		update pending_email_alert_subscription set confirmed_at = ? where token = ?
	`

	_, err := tx.Exec(query, confirmedAt, token)
	if err != nil {
		return fmt.Errorf("updatePendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func getAlertSubscriptionByEmail(tx *sql.Tx, email string) (AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and destination = ?
	`

	var sub AlertSubscription
	err := tx.QueryRow(query, email).Scan(
		&sub.ID,
		&sub.Type,
		&sub.Destination,
		&sub.Meta,
		&sub.Active,
	)
	if err != nil {
		return sub, fmt.Errorf("getAlertSubscriptionByEmail.Scan: %w", err)
	}

	return sub, nil
}

func getPendingEmailAlertSubscriptionEmailByToken(tx *sql.Tx, token string) (string, error) {
	const query = `
		select email from pending_email_alert_subscription where token = ? and confirmed_at is null
	`

	var email string

	err := tx.QueryRow(query, token).Scan(&email)
	if err != nil {
		return email, fmt.Errorf("getPendingEmailAlertSubscriptionEmailByToken.Scan: %w", err)
	}

	return email, nil
}

func getSubscribeEmailConfirm(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("get_subscribe_email_confirm.html")
	if err != nil {
		log.Printf("getSubscribeEmailConfirm.parseTmpl: %s", err)
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
		log.Printf("getSubscribeEmailConfirm.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSubscribeEmailConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notificationChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpNotificationChannel, err := getNotificationChannelByID(tx, notificationChannelID)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	email, err := getPendingEmailAlertSubscriptionEmailByToken(tx, token)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getPendingEmailAlertSubscriptionEmailByToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	sub, err := getAlertSubscriptionByEmail(tx, email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postSubscribeEmailConfirm.getAlertSubscriptionByEmail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if sub.Active {
		return
	}

	smtpDetail, ok := smtpNotificationChannel.Details.(SMTPNotificationDetails)
	if !ok {
		log.Printf("postSubscribeEmailConfirm.Details: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.CommitRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !alertSettings.ManagedSubscriptions &&
		strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		err = postmarkDeleteSuppression(email, smtpDetail.Password, smtpDetail.Misc["pm-broadcast"])
		if err != nil {
			log.Printf("postSubscribeEmailConfirm.postmarkDeleteSuppression: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	tx, err = rwDB.Begin()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.BeginUpdate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updatePendingEmailAlertSubscription(tx, time.Now().UTC(), token)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.updatePendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertSubscription(
		tx,
		"email",
		email,
		base64.URLEncoding.EncodeToString(tokenBytes),
	)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.createAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.CommitUpdate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/?email_subscribed=1", http.StatusFound)
}

func getUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tmpl, err := parseTmpl("unsubscribe.html")
	if err != nil {
		log.Printf("getUnsubscribeEmail.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName,
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getUnsubscribeEmail.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postUnsubscribe.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateEmailAlertSubscriptionActiveByMeta(tx, token, false)
	if err != nil {
		log.Printf("postUnsubscribe.updateEmailAlertSubscriptionActiveByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postUnsubscribe.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("post_unsubscribe.html")
	if err != nil {
		log.Printf("postUnsubscribe.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName,
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("postUnsubscribe.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postResubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postResubscribe.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateEmailAlertSubscriptionActiveByMeta(tx, token, true)
	if err != nil {
		log.Printf("postResubscribe.updateEmailAlertSubscriptionActiveByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postResubscribe.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("post_resubscribe.html")
	if err != nil {
		log.Printf("postResubscribe.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName,
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("postResubscribe.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
