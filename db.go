package main

import (
	"cmp"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

//go:embed schema.sql
var sqlSchema string

func Migration1715019045AddSlugColumns(tx *sql.Tx) error {
	requiresSlug := map[string]string{
		"old__service":              "service",
		"old__monitor":              "monitor",
		"old__notification_channel": "notification_channel",
		"old__mail_group":           "mail_group",
	}

	for k, v := range requiresSlug {
		err := copyNonSlugToSlugTable(tx, k, v)
		if err != nil {
			return fmt.Errorf("Migration1715019045AddSlugColumns.copyNonSlugToSlugTable"+k+": %w", err)
		}
	}

	copy := map[string]string{
		"old__alert_service":                   "alert_service",
		"old__monitor_log":                     "monitor_log",
		"old__monitor_log_last_checked":        "monitor_log_last_checked",
		"old__monitor_notification_channel":    "monitor_notification_channel",
		"old__alert_setting_smtp_notification": "alert_setting_smtp_notification",
		"old__mail_group_member":               "mail_group_member",
		"old__mail_group_monitor":              "mail_group_monitor",
	}

	for src, dst := range copy {
		err := copyTable(tx, src, dst)
		if err != nil {
			return fmt.Errorf("Migration1715019045AddSlugColumns.copyTable"+src+": %w", err)
		}
	}

	dropTablesQuery := ""
	for k := range copy {
		dropTablesQuery += "drop table " + k + "; "
	}
	for k := range requiresSlug {
		dropTablesQuery += "drop table " + k + "; "
	}

	_, err := tx.Exec(dropTablesQuery)
	if err != nil {
		return fmt.Errorf("Migration1715019045AddSlugColumns.ExecDropTables: %w", err)
	}

	return nil
}

// migrationName strips the extension from a migration filename.
//
// TrimRight takes a CUTSET, not a suffix, so the old spelling turned
// "..._add_slug_columns.sql" into "..._add_slug_column" -- eating the trailing
// "s" too. Harmless while both the write and the compare were equally wrong,
// but two migrations differing only by a trailing s or l would collapse onto
// one name and hit the unique constraint, which is a log.Fatalf at startup.
func migrationName(fileName string) string {
	return strings.TrimSuffix(fileName, ".sql")
}

func initDB(immediate bool) *sql.DB {
	// 0700, not ModePerm: app.db holds every live session token, the bcrypt
	// hashes, the SMTP and Slack credentials, the GitHub PAT, and the AES key
	// that decrypts every `secret_` value. On a shared host 0755 hands all of
	// that to any local account.
	if _, err := os.Stat("statusnook-data"); errors.Is(err, os.ErrNotExist) {
		err := os.Mkdir("statusnook-data", 0700)
		if err != nil {
			log.Fatalf("initDB.Mkdir: %s", err)
		}
	} else if err := os.Chmod("statusnook-data", 0700); err != nil {
		log.Fatalf("initDB.Chmod: %s", err)
	}

	dsn := "file:statusnook-data/app.db?_foreign_keys=on&_journal_mode=wal"
	if immediate {
		dsn += "&_txlock=immediate"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		log.Fatalf("initDB.Open: %s", err)
	}

	if immediate {
		files, err := migrationsFS.ReadDir("migrations")
		if err != nil {
			log.Fatalf("initDB.ReadDir: %s", err)
		}

		slices.SortFunc(files, func(a, b fs.DirEntry) int {
			return cmp.Compare(a.Name(), b.Name())
		})

		const tableCountQuery = `
			select
				count(*)
			from
				sqlite_schema
			where 
				type = 'table' and 
				name not like 'sqlite_%';
		`

		tableCount := 0
		row := db.QueryRow(tableCountQuery)
		err = row.Scan(&tableCount)
		if err != nil {
			log.Fatalf("initDB.ScanTableCount: %s", err)
		}

		if tableCount == 0 {
			tx, err := db.Begin()
			if err != nil {
				log.Fatalf("initDB.BeginExecSchema: %s", err)
			}
			defer tx.Rollback()

			_, err = tx.Exec(sqlSchema)
			if err != nil {
				log.Fatalf("initDB.ExecSchema: %s", err)
			}

			params := []any{}
			placeholders := ""
			for i, v := range files {
				placeholders += "(?, ?)"
				if i < len(files)-1 {
					placeholders += ", "
				}
				params = append(params, migrationName(v.Name()), true)
			}

			insertMigrationQuery := fmt.Sprintf(
				`insert into migration(name, skipped) values %s;`,
				placeholders,
			)

			_, err = tx.Exec(insertMigrationQuery, params...)
			if err != nil {
				log.Fatalf("initDB.ExecInsertMigration: %s", err)
			}

			err = tx.Commit()
			if err != nil {
				log.Fatalf("initDB.CommitExecSchema: %s", err)
			}
		} else {
			const createMigrationTableQuery = `
				create table if not exists migration(
					id integer primary key,
					name text not null unique,
					skipped int not null
				);
			`

			_, err := db.Exec(createMigrationTableQuery)
			if err != nil {
				log.Fatalf("initDB.ExecCreateMigrationTable: %s", err)
			}

			existingMigrations := map[string]bool{}

			rows, err := db.Query("select name from migration")
			if err != nil {
				log.Fatalf("initDB.ExecQueryMigrations: %s", err)
			}
			defer rows.Close()

			for rows.Next() {
				var name string
				err = rows.Scan(&name)
				if err != nil {
					log.Fatalf("initDB.ScanMigration: %s", err)
				}

				existingMigrations[name] = true
			}

			if err := rows.Err(); err != nil {
				log.Fatalf("initDB.RowsErrMigration: %s", err)
			}

			for _, file := range files {
				name := migrationName(file.Name())

				// Installs that ran the old TrimRight spelling recorded the
				// truncated name, so both count as already-applied. Without
				// this the rename re-runs every migration whose name ends in a
				// character from ".sql".
				_, applied := existingMigrations[name]
				if !applied {
					_, applied = existingMigrations[strings.TrimRight(file.Name(), ".sql")]
				}
				if applied {
					continue
				}
				migrationName := name

				data, err := migrationsFS.ReadFile(path.Join("migrations", file.Name()))
				if err != nil {
					log.Fatalf("initDB.ReadFile %s: %s", file.Name(), err)
				}

				func() {
					tx, err := db.Begin()
					if err != nil {
						log.Fatalf("initDB.BeginMigration %s: %s", file.Name(), err)
					}
					defer tx.Rollback()

					_, err = tx.Exec(string(data))
					if err != nil {
						log.Fatalf("initDB.ExecMigration %s: %s", file.Name(), err)
					}

					if file.Name() == "1715019045_add_slug_columns.sql" {
						err = Migration1715019045AddSlugColumns(tx)
						if err != nil {
							log.Fatalf("initDB.Migration1715019045AddSlugColumns %s: %s", file.Name(), err)
						}
					}

					insertMigrationQuery := fmt.Sprintf(
						`insert into migration(name, skipped) values ('%s', false)`,
						migrationName,
					)
					_, err = tx.Exec(insertMigrationQuery)
					if err != nil {
						log.Fatalf("initDB.ExecInsertMigrationSkip %s: %s", file.Name(), err)
					}

					err = tx.Commit()
					if err != nil {
						log.Fatalf("initDB.CommitMigration %s: %s", file.Name(), err)
					}

					if file.Name() == "1715019045_add_slug_columns.sql" {
						_, err = db.Exec("vacuum")
						if err != nil {
							log.Printf("initDB.Migration1715019045AddSlugColumnsExecVacuum: %s", err)
						}
					}
				}()
			}
		}
	}

	restrictDBFilePermissions()

	return db
}

// restrictDBFilePermissions narrows the database files to 0600. SQLite creates
// them with 0644 minus the umask, and their contents are session tokens,
// credentials and the secret key -- see the comment in initDB.
func restrictDBFilePermissions() {
	for _, name := range []string{
		"statusnook-data/app.db",
		"statusnook-data/app.db-wal",
		"statusnook-data/app.db-shm",
	} {
		err := os.Chmod(name, 0600)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}

		// Loud, because the alternative is a database readable by every
		// account on the host and nobody knowing.
		log.Printf(
			"ERROR restrictDBFilePermissions.Chmod %s: %s -- this file may be "+
				"readable by other users on this host",
			name,
			err,
		)
	}
}

func copyTable(tx *sql.Tx, src string, dst string) error {
	cols := []string{}

	rows, err := tx.Query("select name from pragma_table_info('" + src + "')")
	if err != nil {
		return fmt.Errorf("copyTable.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		col := ""

		err := rows.Scan(&col)
		if err != nil {
			return fmt.Errorf("copyTable.Scan: %w", err)
		}

		cols = append(cols, col)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("copyTable.RowsErr: %w", err)
	}

	query := fmt.Sprintf(`
		insert into
			%s (
				%s
			)
		select
			%s
		from
			%s
	`,
		dst,
		strings.Join(cols, ", "),
		strings.Join(cols, ", "),
		src,
	)

	_, err = tx.Exec(query)
	if err != nil {
		return fmt.Errorf("copyTable.Exec: %w", err)
	}

	return nil
}

func copyNonSlugToSlugTable(tx *sql.Tx, src string, dst string) error {
	srcCols := []string{}

	rows, err := tx.Query("select name from pragma_table_info('" + src + "')")
	if err != nil {
		return fmt.Errorf("copyNonSlugToSlugTable.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		col := ""

		err := rows.Scan(&col)
		if err != nil {
			return fmt.Errorf("copyNonSlugToSlugTable.ScanTableInfo: %w", err)
		}

		srcCols = append(srcCols, col)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("copyNonSlugToSlugTable.RowsErr: %w", err)
	}

	dstCols := append(append([]string{}, "id", "slug"), srcCols[1:]...)

	srcColsPlusSlug := append(
		append([]string{}, "id", "(select slug from t where id = "+src+".id)"),
		srcCols[1:]...,
	)

	var count int
	err = tx.QueryRow(
		`select count(*) from ` + src,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("copyNonSlugToSlugTable.ScanCount: %w", err)
	}

	if count == 0 {
		return nil
	}

	cteQuery, params, err := generateSlugBackfillCte(tx, src)
	if err != nil {
		return fmt.Errorf("copyNonSlugToSlugTable.generateSlugBackfillCte: %w", err)
	}

	query := fmt.Sprintf(`
		%s

		insert into
			%s (
				%s
			)
		select
			%s
		from
			%s;
	`,
		cteQuery,
		dst,
		strings.Join(dstCols, ", "),
		strings.Join(srcColsPlusSlug, ", "),
		src,
	)

	_, err = tx.Exec(query, params...)
	if err != nil {
		return fmt.Errorf("copyNonSlugToSlugTable.Exec: %w", err)
	}

	return nil
}

func generateSlugBackfillCte(tx *sql.Tx, tableName string) (string, []any, error) {
	pattern := regexp.MustCompile(`[^\p{L}\d]+`)

	query := "select id, name from " + tableName + " order by id asc"

	rows, err := tx.Query(query)
	if err != nil {
		return "", []any{}, fmt.Errorf("generateSlugBackfillCte.Query: %w", err)
	}
	defer rows.Close()

	idToName := map[int]string{}
	slugToId := map[string]int{}
	sortedIds := []int{}

	for rows.Next() {
		var id int
		var name string

		err := rows.Scan(&id, &name)
		if err != nil {
			return "", []any{}, fmt.Errorf("generateSlugBackfillCte.Scan: %w", err)
		}

		idToName[id] = name
		sortedIds = append(sortedIds, id)
	}

	if err := rows.Err(); err != nil {
		return "", []any{}, fmt.Errorf("generateSlugBackfillCte.RowsErr: %w", err)
	}

	if len(idToName) == 0 {
		return "", []any{}, nil
	}

	slices.Sort(sortedIds)

	for _, id := range sortedIds {
		attempt := 0
		for {
			slug := strings.Trim(pattern.ReplaceAllString(strings.ToLower(idToName[id]), "-"), "-")

			if slug == "" {
				slug = strconv.Itoa(attempt)
			} else if attempt > 0 {
				slug += "-" + strconv.Itoa(attempt)
			}

			attempt++

			_, ok := slugToId[slug]
			if !ok {
				slugToId[slug] = id
				break
			}
		}
	}

	if len(slugToId) == 0 {
		return "", []any{}, nil
	}

	updateQuery := `
		with t(slug, id) as(values
	`

	params := []any{}

	i := 0
	for slug, id := range slugToId {
		updateQuery += "(?, ?)"
		params = append(params, slug, id)
		if i < len(slugToId)-1 {
			updateQuery += ","
		}
		i++
	}

	updateQuery += ")"

	return updateQuery, params, nil
}

//go:embed migrations/*
var migrationsFS embed.FS
var db *sql.DB
var rwDB *sql.DB
var metaName atomicString

func updateMetaValue(tx *sql.Tx, name string, value string) error {
	const query = `
		insert into meta(name, value) values(?, ?)
		on conflict(name) do update set value = excluded.value
	`

	_, err := tx.Exec(query, name, value)
	if err != nil {
		return fmt.Errorf("updateMetaValue.Exec: %w", err)
	}

	return nil
}

func getMetaValue(tx *sql.Tx, name string) (string, error) {
	const query = `
		select value from meta where name = ?
	`

	var v string

	err := tx.QueryRow(query, name).Scan(&v)
	if err != nil {
		return v, fmt.Errorf("getMetaValue.Scan: %w", err)
	}

	return v, nil
}

func generateSlug(name string, slugs map[string]bool) string {
	pattern := regexp.MustCompile(`[^\p{L}\d]+`)

	attempt := 0
	for {
		slug := strings.Trim(pattern.ReplaceAllString(strings.ToLower(name), "-"), "-")

		if slug == "" {
			slug = strconv.Itoa(attempt)
		} else if attempt > 0 {
			slug += "-" + strconv.Itoa(attempt)
		}

		attempt++

		_, ok := slugs[slug]
		if !ok {
			return slug
		}
	}
}
