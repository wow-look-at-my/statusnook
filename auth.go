package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"golang.org/x/crypto/bcrypt"
	"log"
	"net/http"
	"time"
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
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Incorrect credentials
			</div>
		`))
		return
	}

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
			Expires:  time.Now().UTC().Add(time.Hour * 876600),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}

func adminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("HX-Location", "/admin/alerts")
}
