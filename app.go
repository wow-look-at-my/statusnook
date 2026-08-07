package main

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

var BUILD = "dev"
var CA = certmagic.LetsEncryptStagingCA
var VERSION = "v0.3.0"

const SELF_SIGNED_KEY_NAME = "self-signed-key.pem"

var appWg sync.WaitGroup
var appCtx context.Context
var cancelAppCtx context.CancelFunc

type statusCtxKey struct{}

func statusMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.Begin()
		if err != nil {
			log.Printf("statusMiddleware.Begin: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		severity, err := getSeverity(tx)
		if err != nil {
			tx.Rollback()
			log.Printf("statusMiddleware.getSeverity: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("statusMiddleware.Commit: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), statusCtxKey{}, severity)

		h.ServeHTTP(w, r.WithContext(ctx))
	})
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

// serveDecompressed inflates a gzip-only embedded asset for a client that did
// not advertise gzip. Rare enough to do on the fly; the alternative is
// embedding a second copy of every asset.
func serveDecompressed(w http.ResponseWriter, gzPath string) {
	file, err := staticFS.Open(gzPath)
	if err != nil {
		log.Printf("serveDecompressed.Open %s: %s", gzPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		log.Printf("serveDecompressed.NewReader %s: %s", gzPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("serveDecompressed.Copy %s: %s", gzPath, err)
	}
}

var dockerFlag = flag.Bool("docker", false, "")

type SupressionDumpResponse struct {
	Suppressions []Supression
}

type Supression struct {
	EmailAddress      string
	SuppressionReason string
	Origin            string
	CreatedAt         time.Time
}

var lastSuppressionSync time.Time
var supressionSyncMu sync.Mutex

func index(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("index.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("index.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("index.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("index.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("index.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	err = tx.Commit()
	if err != nil {
		log.Printf("index.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("index", indexMarkup)
	if err != nil {
		log.Printf("index.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  string
	}

	formattedAlerts := make([]FormattedAlertDetail, 0, len(alerts))

	for _, alert := range alerts {
		messages := make([]FormattedAlertDetailMessage, 0, len(alert.Messages))
		for _, message := range alert.Messages {
			createdAt := message.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
			if message.CreatedAt.Year() == time.Now().UTC().Year() {
				createdAt = message.CreatedAt.Format("Jan 2 • 15:04 MST")
			}

			formattedMessage := FormattedAlertDetailMessage{
				ID:        message.ID,
				Content:   message.Content,
				CreatedAt: createdAt,
			}
			if message.LastUpdatedAt != nil {
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04",
				)
			}

			messages = append(messages, formattedMessage)
		}

		serviceNames := make([]string, 0, len(alert.Services))
		for _, service := range alert.Services {
			serviceNames = append(serviceNames, service.Name)
		}

		formattedAlert := FormattedAlertDetail{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: alert.CreatedAt.Format("02/01/2006 15:04 MST"),
			Messages:  messages,
			Services:  strings.Join(serviceNames, " • "),
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	serviceStatuses := make(map[int]string, len(services))
	for _, service := range services {
		serviceStatuses[service.ID] = ""
	}
	for _, alert := range alerts {
		for _, service := range alert.Services {
			if serviceStatuses[service.ID] != "red" {
				serviceStatuses[service.ID] = alert.Severity
			}
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Services             []service
			IncidentAlerts       []FormattedAlertDetail
			ServiceStatuses      map[int]string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			Services:             services,
			IncidentAlerts:       formattedAlerts,
			ServiceStatuses:      serviceStatuses,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("index.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook")
}

func postResolve(w http.ResponseWriter, r *http.Request) {
	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postResolve.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.URLEncoding.EncodeToString(tokenBytes)

	authCtx := getAuthCtx(r)

	crossAuthTokensMu.Lock()
	crossAuthTokens[token] = crossAuthToken{userID: authCtx.ID, issuedAt: time.Now().UTC()}
	crossAuthTokensMu.Unlock()

	w.Write([]byte(token))
}

// safeAfterPath keeps the "after" parameter to a path on this host. Anything
// else is an open redirect: an "@host/x" value terminates the authority's
// userinfo, so the browser lands on evil.example.com while the URL still
// opens with the real status domain.
func safeAfterPath(after string) string {
	if after == "" {
		return "/"
	}

	parsed, err := url.Parse(after)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" {
		return "/"
	}

	path := parsed.EscapedPath()
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "/"
	}

	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}

	return path
}

func history(w http.ResponseWriter, r *http.Request) {
	periodParam := r.URL.Query().Get("period")

	if len(periodParam) == 0 {
		periodParam = time.Now().UTC().Format("2006-01")
	}

	if len(periodParam) != 7 {
		http.Redirect(w, r, "/history", http.StatusFound)
		return
	}

	periodDate, err := time.Parse("2006-01", periodParam)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("history.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	oldestAlertDate, err := getOldestAlertDate(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getOldestAlertDate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getAlertHistory(tx, periodDate)
	if err != nil {
		log.Printf("history.listAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("history.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("history.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("history", historyMarkup)
	if err != nil {
		log.Printf("history.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  string
	}

	formattedAlerts := make([]FormattedAlertDetail, 0, len(alerts))

	for _, alert := range alerts {
		messages := make([]FormattedAlertDetailMessage, 0, len(alert.Messages))
		for _, message := range alert.Messages {
			formattedMessage := FormattedAlertDetailMessage{
				ID:        message.ID,
				Content:   message.Content,
				CreatedAt: message.CreatedAt.Format("Jan 2 • 15:04 MST"),
			}
			if message.LastUpdatedAt != nil {
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04 MST",
				)
			}

			messages = append(messages, formattedMessage)
		}

		serviceNames := make([]string, 0, len(alert.Services))
		for _, service := range alert.Services {
			serviceNames = append(serviceNames, service.Name)
		}

		formattedAlert := FormattedAlertDetail{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: alert.CreatedAt.Format("02/01/2006 15:04 MST"),
			Messages:  messages,
			Services:  strings.Join(serviceNames, " • "),
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	previousPeriodDate := periodDate.AddDate(0, -1, 0)
	nextPeriodDate := periodDate.AddDate(0, 1, 0)

	previousPeriodStr := previousPeriodDate.Format("2006-01")
	nextPeriodStr := nextPeriodDate.Format("2006-01")

	now := time.Now().UTC()

	if oldestAlertDate.IsZero() ||
		time.Date(previousPeriodDate.Year(), previousPeriodDate.Month(), 1, 0, 0, 0, 0, now.Location()).
			Before(time.Date(oldestAlertDate.Year(), oldestAlertDate.Month(), 1, 0, 0, 0, 0, now.Location())) {
		previousPeriodStr = ""
	}

	if nextPeriodDate.After(time.Now().UTC()) {
		nextPeriodStr = ""
	}

	err = tmpl.Execute(
		w,
		struct {
			IncidentAlerts       []FormattedAlertDetail
			MaintenanceAlerts    []FormattedAlertDetail
			PeriodText           string
			PreviousPeriod       string
			NextPeriod           string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			IncidentAlerts:       formattedAlerts,
			PeriodText:           periodDate.Format("Jan 2006"),
			PreviousPeriod:       previousPeriodStr,
			NextPeriod:           nextPeriodStr,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("history.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func adminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("HX-Location", "/admin/alerts")
}

func update(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("update", updateMarkup)
	if err != nil {
		log.Printf("update.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			CurrentVersion string
			Ctx            pageCtx
		}{
			CurrentVersion: VERSION,
			Ctx:            getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("update.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
