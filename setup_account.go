package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"log"
	"net/http"
)

func getSetupAccount(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("get_setup_account.html")
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func logout(w http.ResponseWriter, r *http.Request) {
	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("logout.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	sessionToken, err := r.Cookie("session")
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if err = deleteSession(tx, sessionToken.Value); err != nil {
		log.Printf("logout.deleteSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("logout.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Path:     "/",
			MaxAge:   -1,
			Secure:   requestIsHTTPS(r),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/")
}

func createUser(tx *sql.Tx, username string, pwHash string) (int, error) {
	const query = `
		insert into user(username, password) values(?, ?) returning id
	`

	userID := 0
	row := tx.QueryRow(query, username, pwHash)
	err := row.Scan(&userID)
	if err != nil {
		return userID, fmt.Errorf("createUser.Scan: %w", err)
	}

	return userID, nil
}

func editUserUsername(tx *sql.Tx, id int, username string) error {
	const query = `
		update user set username = ? where id = ?
	`

	_, err := tx.Exec(query, username, id)
	if err != nil {
		return fmt.Errorf("editUserUsername.Exec: %w", err)
	}

	return nil
}

func editUser(tx *sql.Tx, id int, username string, pwHash string) error {
	const query = `
		update user set username = ?, password = ? where id = ?
	`

	_, err := tx.Exec(query, username, pwHash, id)
	if err != nil {
		return fmt.Errorf("editUser.Exec: %w", err)
	}

	return nil
}

func deleteUserByID(tx *sql.Tx, id int) error {
	const query = `
		delete from user where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteUserByID.Exec: %w", err)
	}

	return nil
}

func postSetupAccount(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("postSetup.GenerateFromPassword: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	userID, err := createUser(tx, username, string(pwHash))
	if err != nil {
		log.Printf("postSetup.createUser: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSetup.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postSetup.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	err = createSession(tx, token, csrfToken, userID)
	if err != nil {
		log.Printf("postSetup.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "name")
	if err != nil {
		log.Printf("postSetup.updateMetaValue: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup = "name"

	http.SetCookie(w, sessionCookie(r, token))

	w.Header().Add("HX-Location", "/setup/name")
}

func getSetupName(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("get_setup_name.html")
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSetupName(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Name is required
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupName.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "name", name)
	if err != nil {
		log.Printf("postSetupName.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "done")
	if err != nil {
		log.Printf("postSetupName.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetupName.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName = name
	metaSetup = "done"

	w.Header().Add("HX-Location", "/")
}
