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
	const markup = `
		{{define "title"}}Log in{{end}}
		{{define "body"}}
			<div class="auth-dialog-container">
				<div class="auth-dialog">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M8 7a5 5 0 113.61 4.804l-1.903 1.903A1 1 0 019 14H8v1a1 1 0 01-1 1H6v1a1 1 0 01-1 1H3a1 1 0 01-1-1v-2a1 1 0 01.293-.707L8.196 8.39A5.002 5.002 0 018 7zm5-3a.75.75 0 000 1.5A1.5 1.5 0 0114.5 7 .75.75 0 0016 7a3 3 0 00-3-3z" clip-rule="evenodd" />
					  		</svg>	  
						</div>
						<h1>Log in</h1>
					</div>
					<form hx-post hx-swap="none">
						<div id="alert" class="alert" hx-swap-oob></div>
						<label>
							Username
							<input name="username" required />
						</label>

						<label>
							Password
							<input name="password" type="password" required/>
						</label>

						<button>Confirm</button>
					</form>
				</div>
			</div>
		{{end}}
	`

	authCtx := getAuthCtx(r)
	if authCtx.ID != 0 {
		http.Redirect(w, r, "/admin/alerts", http.StatusFound)
		return
	}

	tmpl, err := parseTmpl("getLogin", markup)
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
	const badCredentials = `
		<div id="alert" class="alert" hx-swap-oob="true">
			Incorrect credentials
		</div>
	`

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	if username == "" || password == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Enter a username and password
			</div>
		`))
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
			w.Write([]byte(fmt.Sprintf(`
				<div id="alert" class="alert" hx-swap-oob="true">
					Too many failed attempts. Try again in %d minutes
				</div>
			`, int(retryAfter.Minutes())+1)))
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
			w.Write([]byte(badCredentials))
			return
		}
		log.Printf("postLogin.getPasswordHash: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(password)); err != nil {
		failed()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(badCredentials))
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
