package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

func getEditUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditUser.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	username, err := getUsernameByID(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("getEditUser.getUsernameByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("getEditUser", getEditUserMarkup)
	if err != nil {
		log.Printf("getEditUser.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Username string
			Ctx      pageCtx
		}{
			Username: username,
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditUser.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postEditUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	password := r.PostFormValue("password")
	if password == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if password != "retain" && len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="password-alert" class="alert alert--field" hx-swap-oob="true">
				Password must contain at least 8 characters
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditUser.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = getUsernameByID(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("postEditUser.getUsernameByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if password != "retain" {
		pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Printf("postEditUser.GenerateFromPassword: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editUser(tx, id, username, string(pwHash))
		if err != nil {
			var sqliteErr sqlite3.Error
			if errors.As(err, &sqliteErr) {
				if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(`
						<div id="username-alert" class="alert alert--field" hx-swap-oob="true">
							This username is already taken
						</div>
					`))
					return
				}
			}
			log.Printf("postEditUser.editUser: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = deleteAllSessionsByUserID(tx, id)
		if err != nil {
			log.Printf("postEditUser.deleteAllSessionsByUserID: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else {
		err = editUserUsername(tx, id, username)
		if err != nil {
			var sqliteErr sqlite3.Error
			if errors.As(err, &sqliteErr) {
				if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(`
						<div id="username-alert" class="alert alert--field" hx-swap-oob="true">
							This username is already taken
						</div>
					`))
					return
				}
			}
			log.Printf("postEditUser.editUserUsername: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditUser.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	authCtx := getAuthCtx(r)

	if authCtx.ID == id && password != "retain" {
		w.Header().Add("HX-Location", "/login")
	} else {
		w.Header().Add("HX-Location", "/admin/settings")
	}
}

func deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	authCtx := getAuthCtx(r)
	if authCtx.ID == id {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteUser.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteUserByID(tx, id)
	if err != nil {
		log.Printf("deleteUser.deleteUserByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteUser.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

type UserInvitation struct {
	ID        int
	Token     string
	CreatedAt time.Time
}

func listActiveUserInvitations(tx *sql.Tx, minTime time.Time) ([]UserInvitation, error) {
	const query = `
		select id, token, created_at from user_invitation
		where created_at > ?
		order by id desc
	`

	invs := []UserInvitation{}

	rows, err := tx.Query(query, minTime)
	if err != nil {
		return invs, fmt.Errorf("listActiveUserInvitations.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var inv UserInvitation
		err := rows.Scan(&inv.ID, &inv.Token, &inv.CreatedAt)
		if err != nil {
			return invs, fmt.Errorf("listActiveUserInvitations.Scan: %w", err)
		}

		invs = append(invs, inv)
	}

	if err := rows.Err(); err != nil {
		return invs, fmt.Errorf("listActiveUserInvitations.RowsErr: %w", err)
	}

	return invs, nil
}

func validateUserInvitationToken(tx *sql.Tx, token string, minTime time.Time) (int, error) {
	const query = `
		select id from user_invitation where token = ? and created_at > ?
	`

	var id int
	err := tx.QueryRow(query, token, minTime).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("validateUserInvitationToken.Scan: %w", err)
	}

	return id, nil
}

func createUserInvitation(tx *sql.Tx, token string, createdAt time.Time) error {
	const query = `
		insert into user_invitation(token, created_at) values(?, ?)
	`

	_, err := tx.Exec(query, token, createdAt)
	if err != nil {
		return fmt.Errorf("createUserInvitation.Exec: %w", err)
	}

	return nil
}

func deleteUserInvitation(tx *sql.Tx, id int) error {
	const query = `
		delete from user_invitation where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteUserInvitation.Exec: %w", err)
	}

	return nil
}

func postInviteUser(w http.ResponseWriter, r *http.Request) {
	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postInviteUser.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postInviteUser.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	token := base64.URLEncoding.EncodeToString(tokenBytes)

	err = createUserInvitation(tx, token, time.Now().UTC())
	if err != nil {
		log.Printf("postInviteUser.createUserInvitation: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postInviteUser.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postDeleteInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postDeleteInvite.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteUserInvitation(tx, id)
	if err != nil {
		log.Printf("postDeleteInvite.deleteUserInvitation: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postDeleteInvite.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// sessionLifetime is how long a session token stays valid. Sessions used to
// carry no timestamp and the cookie was set to expire in 100 years, so a token
// recovered from a backup or an old browser profile was a permanent admin
// credential.
const sessionLifetime = 30 * 24 * time.Hour

func createSession(tx *sql.Tx, token string, csrfToken string, userID int) error {
	const query = `
		insert into session(token, csrf_token, created_at, user_id) values(?, ?, ?, ?)
	`

	_, err := tx.Exec(query, token, csrfToken, time.Now().UTC(), userID)
	if err != nil {
		return fmt.Errorf("createSession.Exec: %w", err)
	}

	return nil
}

func validateSession(tx *sql.Tx, token string) (int, string, error) {
	const query = `
		select user.id, session.csrf_Token
		from user
		left join session on session.user_id = user.id
		where session.token = ? and session.created_at > ?
	`

	userID := 0
	csrfToken := ""
	err := tx.QueryRow(query, token, time.Now().UTC().Add(-sessionLifetime)).
		Scan(&userID, &csrfToken)
	if err != nil {
		return userID, csrfToken, err
	}

	return userID, csrfToken, nil
}

type SettingsUser struct {
	ID       int
	Username string
}

func listUsers(tx *sql.Tx) ([]SettingsUser, error) {
	const query = `
		select id, username from user
	`

	users := []SettingsUser{}

	rows, err := tx.Query(query)
	if err != nil {
		return users, fmt.Errorf("listUsers.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var user SettingsUser
		err := rows.Scan(&user.ID, &user.Username)
		if err != nil {
			return users, fmt.Errorf("listUsers.Scan: %w", err)
		}

		users = append(users, user)
	}

	if err := rows.Err(); err != nil {
		return users, fmt.Errorf("listUsers.RowsErr: %w", err)
	}

	return users, nil
}

func getPasswordHash(tx *sql.Tx, username string) (string, int, error) {
	const query = `
		select password, id
		from user
		where username = ?
	`

	hash := ""
	userID := 0
	err := tx.QueryRow(query, username).Scan(&hash, &userID)
	if err != nil {
		return hash, userID, fmt.Errorf("getPasswordHash.QueryRow: %w", err)
	}

	return hash, userID, nil
}

func getUsernameByID(tx *sql.Tx, id int) (string, error) {
	const query = `
		select username from user where id = ?
	`

	username := ""
	err := tx.QueryRow(query, id).Scan(&username)
	if err != nil {
		return username, fmt.Errorf("getUsernameByID.Scan: %w", err)
	}

	return username, nil
}

func deleteSession(tx *sql.Tx, token string) error {
	const query = `
		delete from session where token = ?
	`

	if _, err := tx.Exec(query, token); err != nil {
		return fmt.Errorf("deleteSession.Exec: %w", err)
	}

	return nil
}

func deleteAllSessionsByUserID(tx *sql.Tx, id int) error {
	const query = `
		delete from session where user_id = ?
	`

	if _, err := tx.Exec(query, id); err != nil {
		return fmt.Errorf("deleteAllSessionsByUserID.Exec: %w", err)
	}

	return nil
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
			Name:   "session",
			MaxAge: -1,
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
