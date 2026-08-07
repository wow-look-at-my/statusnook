package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"net/smtp"
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

				// v.EmailAddress, not email: this loop deactivates the
				// subscription of each SUPPRESSED address. Looking up the
				// requester instead meant one subscriber gated the whole dump --
				// either every suppressed address was deactivated or none was,
				// depending on whether the person subscribing already had a row.
				subscription, err := getAlertSubscriptionByEmail(tx, v.EmailAddress)
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
		w.Write([]byte(`
		<dialog id="email-already-subscribed-modal" class="email-already-subscribed-modal success-modal" hx-swap-oob="true">
			<div>
				<div>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
						<path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z" clip-rule="evenodd" />
					</svg>
				</div>
				<span>
					This email address is already subscribed to receive updates
				</span>

				<button onclick="document.querySelector('.email-already-subscribed-modal').close();">Dismiss</button>
			</div>

			<script>
				document.querySelector('.email-updates-modal').close();
				document.querySelector('.email-already-subscribed-modal').showModal();
			</script>
		</dialog>
		`))
		return
	}

	hasRecentPendingSub, err := checkHasRecentPendingEmailAlertSubscription(tx, email, time.Now().UTC())
	if err != nil {
		log.Printf("postSubscribeEmail.checkHasRecentPendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if hasRecentPendingSub {
		w.Write([]byte(postSubscribeEmailMarkup))
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
		// err is nil here, so the old "%s" printed %!s(<nil>).
		log.Printf("postSubscribeEmail.NotificationDetailsAssert: channel %d is not SMTP", channel.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Committed before the SMTP conversation, not after. rwDB is a single
	// connection opened IMMEDIATE, and net/smtp has no dial timeout, so
	// holding the transaction across a send to an unreachable host let one
	// unauthenticated request block every writer -- monitor logging included --
	// for the OS connect timeout. The pending row is all the confirm flow
	// needs; the send is best-effort and already logged when it fails.
	if err := tx.Commit(); err != nil {
		log.Printf("postSubscribeEmail.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	msg := [][]byte{
		[]byte("Subject: Confirm your subscription to " + headerValue(metaName.Load()) + " status alerts"),
		[]byte("To: " + headerValue(email)),
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

	const emailTmpl = `Hi,<br><br>
	
To start receiving status alert emails from {{.Name}}, please <a href="{{.Link}}">confirm your subscription</a>.
<br><br>

If this email reached you by mistake, feel free to ignore it and we won't subscribe you.
`

	tmpl, err := parseEmailTmpl("alertConfirm", emailTmpl)
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
			Name: metaName.Load(),
			Link: protocol + "://" + metaDomain.Load() + "/subscribe/email/confirm?token=" + token,
		},
	)
	if err != nil {
		log.Printf("postSubscribeEmail.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
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
		[]string{email},
		bytes.Join(msg, []byte("\r\n")),
	)
	if err != nil {
		log.Printf("postSubscribeEmail.SendMail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write([]byte(postSubscribeEmailMarkup))
}

// pendingSubscriptionLifetime bounds how long a confirmation link works.
// Without it the token was replayable forever, and the row it belongs to was
// never deleted either.
const pendingSubscriptionLifetime = 24 * time.Hour

func getSubscribeEmailConfirm(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("getSubscribeEmailConfirm", getSubscribeEmailConfirmMarkup)
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

	tmpl, err := parseTmpl("unsubscribe", getUnsubscribeMarkup)
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
			Name:  metaName.Load(),
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

	tmpl, err := parseTmpl("postUnsubscribe", postUnsubscribeMarkup)
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
			Name:  metaName.Load(),
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

	tmpl, err := parseTmpl("postResubscribe", postResubscribeMarkup)
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
			Name:  metaName.Load(),
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
