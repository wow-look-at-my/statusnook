package main

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The user, session and meta helpers moved onto sqlc-generated queries. Two
// behaviours there are security-relevant and easy to lose in a rewrite: a
// session past sessionLifetime must stop validating, and an unknown username
// must come back as sql.ErrNoRows so postLogin treats it as a failed login
// rather than a server fault.
func TestUserAndSessionQueries(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	id, err := createUser(tx, "admin", "hash")
	require.NoError(t, err)
	require.NotZero(t, id)

	hash, gotID, err := getPasswordHash(tx, "admin")
	require.NoError(t, err)
	require.Equal(t, "hash", hash)
	require.Equal(t, id, gotID)

	_, _, err = getPasswordHash(tx, "nobody")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	require.NoError(t, createSession(tx, "tok", "csrf", id))

	gotUser, csrf, err := validateSession(tx, "tok")
	require.NoError(t, err)
	require.Equal(t, id, gotUser)
	require.Equal(t, "csrf", csrf)

	// Age the session past its lifetime; it must stop validating.
	_, err = tx.Exec(
		"update session set created_at = ? where token = ?",
		time.Now().UTC().Add(-sessionLifetime-time.Hour), "tok",
	)
	require.NoError(t, err)

	_, _, err = validateSession(tx, "tok")
	require.True(t, errors.Is(err, sql.ErrNoRows), "expired session still validated: %v", err)

	username, err := getUsernameByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "admin", username)

	require.NoError(t, editUserUsername(tx, id, "root"))
	username, err = getUsernameByID(tx, id)
	require.NoError(t, err)
	require.Equal(t, "root", username)
}

func TestMetaValueRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	_, err = getMetaValue(tx, "absent")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	require.NoError(t, updateMetaValue(tx, "name", "First"))
	v, err := getMetaValue(tx, "name")
	require.NoError(t, err)
	require.Equal(t, "First", v)

	// Upsert, not a second row.
	require.NoError(t, updateMetaValue(tx, "name", "Second"))
	v, err = getMetaValue(tx, "name")
	require.NoError(t, err)
	require.Equal(t, "Second", v)
}
