package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for user, session and user_invitation. SQL in sql/users.sql.

func listActiveUserInvitations(tx *sql.Tx, minTime time.Time) ([]UserInvitation, error) {
	invs := []UserInvitation{}

	rows, err := sqlcgen.New(tx).ListActiveUserInvitations(context.Background(), minTime)
	if err != nil {
		return invs, fmt.Errorf("listActiveUserInvitations.Query: %w", err)
	}

	for _, r := range rows {
		invs = append(invs, UserInvitation{
			ID:        int(r.ID),
			Token:     r.Token,
			CreatedAt: r.CreatedAt,
		})
	}

	return invs, nil
}

func validateUserInvitationToken(tx *sql.Tx, token string, minTime time.Time) (int, error) {
	id, err := sqlcgen.New(tx).ValidateUserInvitationToken(
		context.Background(),
		sqlcgen.ValidateUserInvitationTokenParams{Token: token, CreatedAt: minTime},
	)
	if err != nil {
		return int(id), err
	}

	return int(id), nil
}

func createUserInvitation(tx *sql.Tx, token string, createdAt time.Time) error {
	err := sqlcgen.New(tx).CreateUserInvitation(
		context.Background(),
		sqlcgen.CreateUserInvitationParams{Token: token, CreatedAt: createdAt},
	)
	if err != nil {
		return fmt.Errorf("createUserInvitation.Exec: %w", err)
	}

	return nil
}

func deleteUserInvitation(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteUserInvitation(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteUserInvitation.Exec: %w", err)
	}

	return nil
}

func createSession(tx *sql.Tx, token string, csrfToken string, userID int) error {
	err := sqlcgen.New(tx).CreateSession(context.Background(), sqlcgen.CreateSessionParams{
		Token:     token,
		CsrfToken: csrfToken,
		CreatedAt: time.Now().UTC(),
		UserID:    int64(userID),
	})
	if err != nil {
		return fmt.Errorf("createSession.Exec: %w", err)
	}

	return nil
}

func validateSession(tx *sql.Tx, token string) (int, string, error) {
	row, err := sqlcgen.New(tx).ValidateSession(context.Background(), sqlcgen.ValidateSessionParams{
		Token:     token,
		CreatedAt: time.Now().UTC().Add(-sessionLifetime),
	})
	if err != nil {
		// Unwrapped: callers compare against sql.ErrNoRows to mean "no valid
		// session", which is the ordinary path for an expired cookie.
		return int(row.ID), row.CsrfToken.String, err
	}

	// csrf_token comes back nullable because the join is a LEFT JOIN, though
	// the where clause means a matched row always has a session.
	return int(row.ID), row.CsrfToken.String, nil
}

func listUsers(tx *sql.Tx) ([]SettingsUser, error) {
	users := []SettingsUser{}

	rows, err := sqlcgen.New(tx).ListUsers(context.Background())
	if err != nil {
		return users, fmt.Errorf("listUsers.Query: %w", err)
	}

	for _, r := range rows {
		users = append(users, SettingsUser{ID: int(r.ID), Username: r.Username})
	}

	return users, nil
}

func getPasswordHash(tx *sql.Tx, username string) (string, int, error) {
	row, err := sqlcgen.New(tx).GetPasswordHash(context.Background(), username)
	if err != nil {
		// Unwrapped: postLogin branches on sql.ErrNoRows to mean "no such
		// user", which is a normal failed login rather than a fault.
		return row.Password, int(row.ID), err
	}

	return row.Password, int(row.ID), nil
}

func getUsernameByID(tx *sql.Tx, id int) (string, error) {
	username, err := sqlcgen.New(tx).GetUsernameByID(context.Background(), int64(id))
	if err != nil {
		return username, fmt.Errorf("getUsernameByID.Scan: %w", err)
	}

	return username, nil
}

func deleteSession(tx *sql.Tx, token string) error {
	if err := sqlcgen.New(tx).DeleteSession(context.Background(), token); err != nil {
		return fmt.Errorf("deleteSession.Exec: %w", err)
	}

	return nil
}

func deleteAllSessionsByUserID(tx *sql.Tx, id int) error {
	err := sqlcgen.New(tx).DeleteAllSessionsByUserID(context.Background(), int64(id))
	if err != nil {
		return fmt.Errorf("deleteAllSessionsByUserID.Exec: %w", err)
	}

	return nil
}

func createUser(tx *sql.Tx, username string, pwHash string) (int, error) {
	id, err := sqlcgen.New(tx).CreateUser(context.Background(), sqlcgen.CreateUserParams{
		Username: username,
		Password: pwHash,
	})
	if err != nil {
		return int(id), fmt.Errorf("createUser.Scan: %w", err)
	}

	return int(id), nil
}

func editUserUsername(tx *sql.Tx, id int, username string) error {
	err := sqlcgen.New(tx).EditUserUsername(context.Background(), sqlcgen.EditUserUsernameParams{
		Username: username,
		ID:       int64(id),
	})
	if err != nil {
		return fmt.Errorf("editUserUsername.Exec: %w", err)
	}

	return nil
}

func editUser(tx *sql.Tx, id int, username string, pwHash string) error {
	err := sqlcgen.New(tx).EditUser(context.Background(), sqlcgen.EditUserParams{
		Username: username,
		Password: pwHash,
		ID:       int64(id),
	})
	if err != nil {
		return fmt.Errorf("editUser.Exec: %w", err)
	}

	return nil
}

func deleteUserByID(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteUserByID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteUserByID.Exec: %w", err)
	}

	return nil
}
