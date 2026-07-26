package main

import (
	"database/sql"
	"fmt"
)

func createSession(tx *sql.Tx, token string, csrfToken string, userID int) error {
	const query = `
		insert into session(token, csrf_token, user_id) values(?, ?, ?)
	`

	_, err := tx.Exec(query, token, csrfToken, userID)
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
		where session.token = ?
	`

	userID := 0
	csrfToken := ""
	err := tx.QueryRow(query, token).Scan(&userID, &csrfToken)
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
