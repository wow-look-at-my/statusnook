package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/go-chi/chi/v5"
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

	tmpl, err := parseTmpl("get_invitation.html")
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
		w.Write(alertOOB("Username is required"))
		return
	}

	password := r.PostFormValue("password")
	passwordConfirmation := r.PostFormValue("password-confirmation")

	if password != passwordConfirmation {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("Passwords do not match"))
		return
	}

	if len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB("Password must contain at least 8 characters"))
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
		if isConstraintErr(err) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB("This username is already taken"))
			return
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

	http.SetCookie(w, sessionCookie(r, token))

	w.Header().Add("HX-Location", "/admin/alerts")
}
