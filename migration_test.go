package main

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

// TrimRight takes a cutset: "..._add_slug_columns.sql" lost its trailing "s"
// as well as the extension. Two migrations differing only by a trailing
// character from ".sql" would then collide on migration.name, which is unique
// and checked with log.Fatalf at startup.
func TestMigrationNameStripsOnlyTheExtension(t *testing.T) {
	cases := map[string]string{
		"1715019045_add_slug_columns.sql":    "1715019045_add_slug_columns",
		"1714828244_add_user_invitation.sql": "1714828244_add_user_invitation",
		"1786031724_add_indexes.sql":         "1786031724_add_indexes",
		"1700000000_add_call.sql":            "1700000000_add_call",
		"1700000001_add_calls.sql":           "1700000001_add_calls",
	}

	for file, want := range cases {
		got := migrationName(file)
		assert.Equal(t, want, got)

	}

	// The collision the old spelling produced.
	assert.NotEqual(t, migrationName("a_add_calls.sql"), migrationName("a_add_call.sql"))

}
