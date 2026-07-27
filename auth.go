package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func getLogin(w http.ResponseWriter, r *http.Request) {
	authCtx := getAuthCtx(r)
	if authCtx.ID != 0 {
		http.Redirect(w, r, "/admin/alerts", http.StatusFound)
		return
	}

	tmpl, err := parseTmpl("get_login.html")
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
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	if username == "" || password == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("Enter a username and password"))
		return
	}

	// Rate limit per source address and per username, so neither one account
	// nor one client can be hammered.
	now := time.Now().UTC()
	keys := []string{"ip:" + clientIP(r), "user:" + strings.ToLower(username)}
	for _, key := range keys {
		if ok, retryAfter := loginLimiter.allow(key, now); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write(alertOOB(fmt.Sprintf(
				"Too many failed attempts. Try again in %d minutes",
				int(retryAfter.Minutes())+1,
			)))
			return
		}
	}

	failed := func() {
		for _, key := range keys {
			loginLimiter.fail(key, now)
		}
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
			// Hash a dummy password so a missing user does not answer faster
			// than a wrong password.
			bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))
			failed()
			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB("Incorrect credentials"))
			return
		}
		log.Printf("postLogin.getPasswordHash: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(password)); err != nil {
		failed()
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("Incorrect credentials"))
		return
	}

	token, csrfToken, err := newSessionTokens()
	if err != nil {
		log.Printf("postLogin.newSessionTokens: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

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

	for _, key := range keys {
		loginLimiter.succeed(key)
	}

	http.SetCookie(w, sessionCookie(r, token))

	w.Header().Add("HX-Location", "/admin/alerts")
}

// dummyPasswordHash is a valid bcrypt hash of a random string, compared
// against when the username does not exist to keep response times even.
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func adminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("HX-Location", "/admin/alerts")
}
