package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/goksan/statusnook/internal/sqlcgen"
)

// Data access for the meta key/value table. SQL in sql/meta.sql.

func updateMetaValue(tx *sql.Tx, name string, value string) error {
	err := sqlcgen.New(tx).UpdateMetaValue(context.Background(), sqlcgen.UpdateMetaValueParams{
		Name:  name,
		Value: value,
	})
	if err != nil {
		return fmt.Errorf("updateMetaValue.Exec: %w", err)
	}

	return nil
}

func getMetaValue(tx *sql.Tx, name string) (string, error) {
	v, err := sqlcgen.New(tx).GetMetaValue(context.Background(), name)
	if err != nil {
		// Wrapped, matching what this replaced: callers test errors.Is(err,
		// sql.ErrNoRows) for "not set yet", which sees through %w.
		return v, fmt.Errorf("getMetaValue.Scan: %w", err)
	}

	return v, nil
}
