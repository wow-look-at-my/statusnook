package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type AlertSubscription struct {
	ID          int
	Type        string
	Destination string
	Meta        string
	Active      bool
}

func listActiveAlertEmailSubscriptions(tx *sql.Tx) ([]AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and active = true
	`

	var subs []AlertSubscription

	rows, err := tx.Query(query)
	if err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sub AlertSubscription

		err := rows.Scan(
			&sub.ID,
			&sub.Type,
			&sub.Destination,
			&sub.Meta,
			&sub.Active,
		)
		if err != nil {
			return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Scan: %w", err)
		}

		subs = append(subs, sub)
	}

	return subs, nil
}

func deleteAlertSubscriptionByMeta(tx *sql.Tx, meta string) error {
	const query = `
		delete from alert_subscription where meta = ?
	`

	_, err := tx.Exec(query, meta)
	if err != nil {
		return fmt.Errorf("deleteAlertSubscriptionByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByMeta(tx *sql.Tx, meta string, active bool) error {
	const query = `
		update alert_subscription set active = ? where meta = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, meta)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByEmail(tx *sql.Tx, email string, active bool) error {
	const query = `
		update alert_subscription set active = ? where destination = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, email)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByEmail.Exec: %w", err)
	}

	return nil
}

func createAlertSubscription(tx *sql.Tx, subscriptionType string, destination string, meta string) error {
	const query = `
		insert into alert_subscription(type, destination, meta) values(?, ?, nullif(?, ''))
		on conflict(type, destination) do update set active = true
	`

	_, err := tx.Exec(query, subscriptionType, destination, meta)
	if err != nil {
		return fmt.Errorf("createAlertSubscription.Exec: %w", err)
	}

	return nil
}

type SlackOAuthAccessResponse struct {
	OK   bool `json:"ok"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	IncomingWebhook struct {
		URL       string `json:"url"`
		ChannelID string `json:"channel_id"`
	} `json:"incoming_webhook"`
}

func slackOAuth2Callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("slackOAuth2Callback.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	settings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("slackOAuth2Callback.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	slackInstallURL, err := url.ParseRequestURI(settings.SlackInstallURL)
	if err != nil {
		log.Printf("slackOAuth2Callback.ParseRequestURI: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	form := url.Values{}
	form.Add("code", code)
	form.Add("client_id", slackInstallURL.Query().Get("client_id"))
	form.Add("client_secret", settings.SlackClientSecret)

	// http.PostForm uses the default client, which has no timeout: a slow
	// response would hold this handler open indefinitely.
	slackClient := http.Client{Timeout: 30 * time.Second}

	resp, err := slackClient.PostForm(slackTokenURL, form)
	if err != nil {
		log.Printf("slackOAuth2Callback.PostForm: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("slackOAuth2Callback.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	accessResponse := SlackOAuthAccessResponse{}

	err = json.Unmarshal(respBody, &accessResponse)
	if err != nil {
		log.Printf("slackOAuth2Callback.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !accessResponse.OK {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	meta := accessResponse.Team.ID + "_" + accessResponse.IncomingWebhook.ChannelID

	err = deleteAlertSubscriptionByMeta(tx, meta)
	if err != nil {
		log.Printf("slackOAuth2Callback.deleteAlertSubscriptionByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertSubscription(
		tx,
		"slack",
		accessResponse.IncomingWebhook.URL,
		meta,
	)
	if err != nil {
		log.Printf("slackOAuth2Callback.createAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("slackOAuth2Callback.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/?slack_app_installed=1", http.StatusFound)
}

func postmarkDeleteSuppression(email string, token string, stream string) error {
	body := fmt.Sprintf(
		`
		{
			"Suppressions": [
				{
					"EmailAddress": "%s"
				}
			]
		}
		`,
		email,
	)

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodPost,
		postmarkAPIURL+"/message-streams/"+stream+"/suppressions/delete",
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

func checkHasRecentPendingEmailAlertSubscription(tx *sql.Tx, email string, now time.Time) (bool, error) {
	const query = `
		select exists(
			select 1 from pending_email_alert_subscription where email = ?
			and created_at > datetime(?, '-10 minutes')
		)
	`

	var hasRecent bool

	err := tx.QueryRow(query, email, now).Scan(&hasRecent)
	if err != nil {
		return hasRecent, fmt.Errorf("checkHasRecentPendingEmailAlertSubscription.Exec: %w", err)
	}

	return hasRecent, nil
}

func createPendingEmailAlertSubscription(tx *sql.Tx, token string, email string, createdAt time.Time) error {
	const query = `
		insert into pending_email_alert_subscription(token, email, created_at)
		values(?, ?, ?)
	`

	_, err := tx.Exec(query, token, email, createdAt)
	if err != nil {
		return fmt.Errorf("createPendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

type SupressionDumpResponse struct {
	Suppressions []Supression
}

type Supression struct {
	EmailAddress      string
	SuppressionReason string
	Origin            string
	CreatedAt         time.Time
}

func postmarkDumpSupressions(token string, stream string) (SupressionDumpResponse, error) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	var supressionsResp SupressionDumpResponse

	req, err := http.NewRequest(
		http.MethodGet,
		postmarkAPIURL+"/message-streams/"+stream+"/suppressions/dump"+
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

var lastSuppressionSync time.Time
var supressionSyncMu sync.Mutex
