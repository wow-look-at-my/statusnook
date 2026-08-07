package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func csrfMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}

		csrfToken := r.Header.Get("csrf-token")
		authCtx := getAuthCtx(r)

		// Constant-time: a 32-byte token is not realistically timeable over a
		// network, but the comparison costs nothing either way.
		if subtle.ConstantTimeCompare([]byte(csrfToken), []byte(authCtx.CSRFToken)) != 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		h.ServeHTTP(w, r)
	})
}

type authCtxKey struct{}

type authCtx struct {
	ID        int
	CSRFToken string
}

func getAuthCtx(r *http.Request) authCtx {
	ctx := authCtx{}
	if val, ok := r.Context().Value(authCtxKey{}).(authCtx); ok {
		ctx = val
	}

	return ctx
}

type plainOrLoginAuth struct {
	username string
	password string
	host     string
	auth     string
}

func PlainOrLoginAuth(username string, password string, host string) smtp.Auth {
	return &plainOrLoginAuth{username: username, password: password, host: host}
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

	resp, err := http.PostForm(slackAPIBaseURL+"/api/oauth.v2.access", form)
	if err != nil {
		log.Printf("slackOAuth2Callback.PostForm: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
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

// userInvitationLifetime matches the 24h the invitation handlers already
// enforce when validating a token; expired rows were simply never removed.
const userInvitationLifetime = 24 * time.Hour

func getInvitation(w http.ResponseWriter, r *http.Request) {
	inviteToken := chi.URLParam(r, "token")
	if inviteToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getInvitation.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = validateUserInvitationToken(tx, inviteToken, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.Printf("getInvitation.validateUserInvitationToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getInvitation.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("getInvitation", getInvitationMarkup)
	if err != nil {
		log.Printf("getInvitation.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getInvitation.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postInvitation(w http.ResponseWriter, r *http.Request) {
	inviteToken := chi.URLParam(r, "token")
	if inviteToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postInvitation.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	id, err := validateUserInvitationToken(tx, inviteToken, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.Printf("postInvitation.validateUserInvitationToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = deleteUserInvitation(tx, id)
	if err != nil {
		log.Printf("postInvitation.deleteUserInvitation: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Username is required
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	passwordConfirmation := r.PostFormValue("password-confirmation")

	if password != passwordConfirmation {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Passwords do not match
			</div>
		`))
		return
	}

	if len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Password must contain at least 8 characters
			</div>
		`))
		return
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("postInvitation.GenerateFromPassword: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	userID, err := createUser(tx, username, string(pwHash))
	if err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) {
			if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`
					<div id="alert" class="alert" hx-swap-oob="true">
						This username is already taken
					</div>
				`))
				return
			}
		}
		log.Printf("postInvitation.createUser: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postInvitation.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postInvitation.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	err = createSession(tx, token, csrfToken, userID)
	if err != nil {
		log.Printf("postInvitation.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postInvitation.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}

// A cross-auth token mints a full admin session, and it travels in a URL
// query string -- proxy logs, browser history, Referer. Short-lived and
// single-use is the only thing keeping that from being a permanent
// credential lying around in logs.
const crossAuthTokenTTL = time.Minute

type crossAuthToken struct {
	userID   int
	issuedAt time.Time
}

var crossAuthTokensMu sync.Mutex
var crossAuthTokens = map[string]crossAuthToken{}

// redeemCrossAuthToken consumes a token, whatever the outcome: a token that
// was presented once is spent, valid or not.
func redeemCrossAuthToken(token string) (int, bool) {
	crossAuthTokensMu.Lock()
	defer crossAuthTokensMu.Unlock()

	now := time.Now().UTC()

	// Sweep here rather than on a timer: the map only grows when someone
	// issues a token, and this runs on the path that follows.
	for k, v := range crossAuthTokens {
		if now.Sub(v.issuedAt) > crossAuthTokenTTL {
			delete(crossAuthTokens, k)
		}
	}

	v, ok := crossAuthTokens[token]
	delete(crossAuthTokens, token)
	if !ok || now.Sub(v.issuedAt) > crossAuthTokenTTL {
		return 0, false
	}

	return v.userID, true
}

func getCrossAuth(w http.ResponseWriter, r *http.Request) {
	redirectURL := "https://" + metaDomain.Load() + safeAfterPath(r.URL.Query().Get("after"))

	auth := getAuthCtx(r)
	if auth.ID != 0 {
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	tokenParam := r.URL.Query().Get("token")
	if tokenParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userID, ok := redeemCrossAuthToken(tokenParam)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("getCrossAuth.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("getCrossAuth.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("getCrossAuth.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if err = createSession(tx, token, csrfToken, userID); err != nil {
		log.Printf("getCrossAuth.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getCrossAuth.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func getLogin(w http.ResponseWriter, r *http.Request) {

	authCtx := getAuthCtx(r)
	if authCtx.ID != 0 {
		http.Redirect(w, r, "/admin/alerts", http.StatusFound)
		return
	}

	tmpl, err := parseTmpl("getLogin", getLoginMarkup)
	if err != nil {
		log.Printf("getLogin.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tmpl.Execute(w, nil); err != nil {
		log.Printf("getLogin.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	rateLimitKey := loginRateLimitKey(r)
	if loginRateLimited(rateLimitKey) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Too many failed attempts. Try again later.
			</div>
		`))
		return
	}

	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Enter a username and password
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	if password == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Enter a username and password
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postLogin.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	pwHash, userID, err := getPasswordHash(tx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Hash against a throwaway so an unknown username costs the same
			// ~60ms as a known one. Returning early made the difference a
			// clean user-enumeration oracle.
			bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))

			recordLoginFailure(rateLimitKey)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="alert" class="alert" hx-swap-oob="true">
					Incorrect credentials
				</div>
			`))
			return
		}
		log.Printf("postLogin.getPasswordHash: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(password)); err != nil {
		recordLoginFailure(rateLimitKey)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Incorrect credentials
			</div>
		`))
		return
	}

	clearLoginFailures(rateLimitKey)

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postLogin.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postLogin.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	if err = createSession(tx, token, csrfToken, userID); err != nil {
		log.Printf("postLogin.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postLogin.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}
