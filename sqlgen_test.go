package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqlc's sqlite parser measures query text in runes but cuts it in bytes, so a
// single multi-byte character anywhere in a query file silently truncates the
// generated SQL for every query after it -- a bullet in one group_concat
// separator turned `delete from alert_setting_smtp_notification` into
// `delete from alert_setting_smtp_notificati`. Write char(N) instead.
func TestSQLQueryFilesAreASCII(t *testing.T) {
	paths, err := filepath.Glob("sql/*.sql")
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no sqlc query files found")

	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)

		for i, b := range data {
			require.Less(t, b, byte(0x80),
				"%s byte %d is non-ASCII (%#x); sqlc truncates every query after it",
				path, i, b)
		}
	}
}

// schema.sql is exempt from the rule above -- its seed rows are user-visible
// copy and carry an emoji -- so the risk it runs instead is that sqlc's catalog
// stops short and types a later table's columns as unknown. Every table having
// a generated model is the direct check for that.
func TestEveryTableHasAGeneratedModel(t *testing.T) {
	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)

	models, err := os.ReadFile("internal/sqlcgen/models.go")
	require.NoError(t, err)

	tables := regexp.MustCompile(`(?m)^create table (\w+)`).FindAllStringSubmatch(string(schema), -1)
	require.NotEmpty(t, tables)

	for _, match := range tables {
		name := "type " + snakeToPascal(match[1]) + " struct"
		require.True(t, strings.Contains(string(models), name),
			"schema.sql declares %q but sqlc generated no %q", match[1], name)
	}
}

func snakeToPascal(s string) string {
	parts := strings.Split(s, "_")
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}

	return strings.Join(parts, "")
}

// A query that mixes numbered placeholders with bare ones is a runtime failure
// waiting on a code path: sqlite numbers a bare `?` one past the highest it has
// already seen, so `where a = ? and b < ?2` wants three parameters while sqlc
// passes two. sqlc numbers a placeholder as soon as one named argument repeats
// or is written as sqlc.arg, so the fix is to name every parameter in that
// query, not just the one that needed it.
func TestGeneratedQueriesDoNotMixPlaceholderStyles(t *testing.T) {
	paths, err := filepath.Glob("internal/sqlcgen/*.sql.go")
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no generated query files found")

	numbered := regexp.MustCompile(`\?\d`)
	bare := regexp.MustCompile(`\?($|[^\d])`)
	queries := regexp.MustCompile("(?s)`(-- name: (\\w+).*?)`")

	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)

		for _, match := range queries.FindAllStringSubmatch(string(data), -1) {
			sql, name := match[1], match[2]
			if !numbered.MatchString(sql) {
				continue
			}

			require.False(t, bare.MatchString(sql),
				"%s in %s mixes ?N with bare ?; name every parameter in it", name, path)
		}
	}
}
