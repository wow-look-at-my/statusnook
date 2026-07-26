package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
	"log"
	"net/http"
	"time"
)

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

	const markup = `
		{{define "title"}}You're invited to create an admin user{{end}}
		{{define "body"}}
			<div class="auth-dialog-container">
				<div class="auth-dialog">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path d="M10 8a3 3 0 100-6 3 3 0 000 6zM3.465 14.493a1.23 1.23 0 00.41 1.412A9.957 9.957 0 0010 18c2.31 0 4.438-.784 6.131-2.1.43-.333.604-.903.408-1.41a7.002 7.002 0 00-13.074.003z" />
					  		</svg>			
						</div>
						<h1>You're invited to create an admin user</h1>
					</div>
					<form hx-post hx-swap="none">
						<div id="alert" class="alert"></div>
						<label>
							Username
							<input name="username" required>
						</label>

						<label>
							Password
							<input name="password" type="password" required>
						</label>
					
						<label>
							Confirm password
							<input name="password-confirmation" type="password" required>
						</label>

						<button>Confirm</button>
					</form>
				</div>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getInvitation", markup)
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
			Expires:  time.Now().UTC().Add(time.Hour * 876600),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}
