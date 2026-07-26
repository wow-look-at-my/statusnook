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
	const markup = `
		{{define "title"}}Create an admin user - Statusnook Setup{{end}}
		{{define "body"}}
			<div class="auth-dialog-container">
				<div class="auth-dialog">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path d="M10 8a3 3 0 100-6 3 3 0 000 6zM3.465 14.493a1.23 1.23 0 00.41 1.412A9.957 9.957 0 0010 18c2.31 0 4.438-.784 6.131-2.1.43-.333.604-.903.408-1.41a7.002 7.002 0 00-13.074.003z" />
					  		</svg>			
						</div>
						<h1>Create an admin user</h1>
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

	tmpl, err := parseTmpl("getSetupAccount", markup)
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
	const markup = `
		{{define "title"}}Name your nook - Statusnook Setup{{end}}
		{{define "body"}}
			<div class="auth-dialog-container">
				<div class="auth-dialog">
					<div>
						<div>
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M4.5 2A2.5 2.5 0 0 0 2 4.5v3.879a2.5 2.5 0 0 0 .732 1.767l7.5 7.5a2.5 2.5 0 0 0 3.536 0l3.878-3.878a2.5 2.5 0 0 0 0-3.536l-7.5-7.5A2.5 2.5 0 0 0 8.38 2H4.5ZM5 6a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" clip-rule="evenodd" />
							</svg>
						</div>
						<h1>Name your nook</h1>
					</div>
					<div style="margin-bottom: 6.0rem;">
						<p style="text-align: center;">This name will be displayed on your status page</p>
					</div>
					<form hx-post hx-swap="none">
						<div id="alert" class="alert" hx-swap-oob></div>
						<label>
							Name
							<input name="name" type="text" placeholder="Statusnook" required>
						</label>

						<button>Confirm</button>
					</form>
				</div>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getSetupName", markup)
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
