package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for the service table. The SQL lives in sql/services.sql and is
// compiled to typed Go by sqlc (`go generate ./...`), so a column rename that
// breaks a query is a build error rather than a runtime scan failure.
//
// These wrappers keep the signatures their callers already use and convert
// between sqlc's generated row types and the app's own. That is what lets the
// tables migrate one at a time instead of in a single sweeping change.

func listServices(tx *sql.Tx) ([]service, error) {
	services := []service{}

	rows, err := sqlcgen.New(tx).ListServices(context.Background())
	if err != nil {
		return services, fmt.Errorf("listServices.Query: %w", err)
	}

	for _, r := range rows {
		services = append(services, service{
			ID:         int(r.ID),
			Slug:       r.Slug,
			Name:       r.Name,
			HelperText: r.HelperText,
		})
	}

	return services, nil
}

func createService(tx *sql.Tx, slug string, name string, helperText string) error {
	err := sqlcgen.New(tx).CreateService(context.Background(), sqlcgen.CreateServiceParams{
		Slug:       slug,
		Name:       name,
		HelperText: helperText,
	})
	if err != nil {
		return fmt.Errorf("createService.Exec: %w", err)
	}

	return nil
}

func deleteServiceByID(tx *sql.Tx, id int) error {
	if err := sqlcgen.New(tx).DeleteServiceByID(context.Background(), int64(id)); err != nil {
		return fmt.Errorf("deleteServiceByID.Exec: %w", err)
	}

	return nil
}

func getServiceByID(tx *sql.Tx, id int) (service, error) {
	row, err := sqlcgen.New(tx).GetServiceByID(context.Background(), int64(id))
	if err != nil {
		return service{}, fmt.Errorf("getServiceByID.Scan: %w", err)
	}

	// Slug is deliberately not selected here, matching the query this
	// replaced -- callers of this one use the id they already hold.
	return service{
		ID:         int(row.ID),
		Name:       row.Name,
		HelperText: row.HelperText,
	}, nil
}

func editService(tx *sql.Tx, id int, name string, helperText string) error {
	err := sqlcgen.New(tx).EditService(context.Background(), sqlcgen.EditServiceParams{
		Name:       name,
		HelperText: helperText,
		ID:         int64(id),
	})
	if err != nil {
		return fmt.Errorf("editService.Exec: %w", err)
	}

	return nil
}

func updateServiceSlug(tx *sql.Tx, old string, new string) (int, error) {
	id, err := sqlcgen.New(tx).UpdateServiceSlug(context.Background(), sqlcgen.UpdateServiceSlugParams{
		Slug:   new,
		Slug_2: old,
	})
	if err != nil {
		// Wrapped, not replaced: applyConfig tells sql.ErrNoRows (rename
		// source missing) from a sqlite3 constraint error (target slug taken)
		// through this, and errors.Is/As see through %w.
		return int(id), fmt.Errorf("updateServiceSlug.Exec: %w", err)
	}

	return int(id), nil
}
