package main

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// The service helpers moved onto sqlc-generated queries. applyConfig's rename
// handling keys off two specific errors coming back out of updateServiceSlug,
// so those have to survive the move, not just the happy path.
func TestServiceQueriesRoundTrip(t *testing.T) {
	withTestDB(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	// schema.sql seeds a "website" service, so start from a known-empty table.
	seeded, err := listServices(tx)
	require.NoError(t, err)
	for _, s := range seeded {
		require.NoError(t, deleteServiceByID(tx, s.ID))
	}

	require.NoError(t, createService(tx, "web", "Website", "example.com"))
	require.NoError(t, createService(tx, "api", "API", "api.example.com"))

	services, err := listServices(tx)
	require.NoError(t, err)
	require.Len(t, services, 2)

	bySlug := map[string]service{}
	for _, s := range services {
		bySlug[s.Slug] = s
	}
	require.Equal(t, "Website", bySlug["web"].Name)
	require.Equal(t, "example.com", bySlug["web"].HelperText)

	got, err := getServiceByID(tx, bySlug["web"].ID)
	require.NoError(t, err)
	require.Equal(t, "Website", got.Name)

	require.NoError(t, editService(tx, bySlug["web"].ID, "Site", "www.example.com"))
	got, err = getServiceByID(tx, bySlug["web"].ID)
	require.NoError(t, err)
	require.Equal(t, "Site", got.Name)
	require.Equal(t, "www.example.com", got.HelperText)

	id, err := updateServiceSlug(tx, "web", "website")
	require.NoError(t, err)
	require.Equal(t, bySlug["web"].ID, id)

	// Renaming a slug that does not exist must still surface as ErrNoRows --
	// applyConfig reports "does not exist, drop this rename" off it.
	_, err = updateServiceSlug(tx, "nope", "whatever")
	require.True(t, errors.Is(err, sql.ErrNoRows), "want ErrNoRows, got %v", err)

	// Renaming onto a taken slug must still surface as a constraint error --
	// applyConfig reports "would not be unique" off it.
	_, err = updateServiceSlug(tx, "website", "api")
	var sqliteErr sqlite3.Error
	require.True(t, errors.As(err, &sqliteErr), "want sqlite3.Error, got %v", err)
	require.Equal(t, sqlite3.ErrConstraint, sqliteErr.Code)

	require.NoError(t, deleteServiceByID(tx, bySlug["api"].ID))
	services, err = listServices(tx)
	require.NoError(t, err)
	require.Len(t, services, 1)
}
