package main

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math/big"
	mathRand "math/rand"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"os/signal"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	textTemplate "text/template"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/mattn/go-sqlite3"
	"github.com/mholt/acmez/acme"
	"github.com/miekg/dns"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

var BUILD = "dev"
var CA = certmagic.LetsEncryptStagingCA
var VERSION = "v0.3.0"

//go:embed schema.sql
var sqlSchema string

const SELF_SIGNED_CERT_NAME = "self-signed-cert.pem"
const SELF_SIGNED_KEY_NAME = "self-signed-key.pem"

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

var tmplsMu sync.RWMutex
var tmpls = map[string]*template.Template{}

func parseTmpl(name string, markup string) (*template.Template, error) {
	tmplsMu.RLock()
	tmpl, ok := tmpls[name]
	tmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}


	tmpl, err := template.New(name).Parse(parseTmplRootTmpl)
	if err != nil {
		return tmpl, err
	}

	tmpl, err = tmpl.Parse(markup)
	if err != nil {
		return tmpl, err
	}

	tmplsMu.Lock()
	tmpls[name] = tmpl
	tmplsMu.Unlock()

	return tmpl, nil
}

var emailTmplsMu sync.RWMutex
var emailTmpls = map[string]*template.Template{}

func parseEmailTmpl(name string, markup string) (*template.Template, error) {
	emailTmplsMu.RLock()
	tmpl, ok := emailTmpls[name]
	emailTmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}

	tmpl = template.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseEmailTmpl.Parse: %w", err)
	}

	emailTmplsMu.Lock()
	emailTmpls[name] = tmpl
	emailTmplsMu.Unlock()

	return tmpl, nil
}

var textTmplsMu sync.RWMutex
var textTmpls = map[string]*textTemplate.Template{}

func parseTextTmpl(name string, markup string) (*textTemplate.Template, error) {
	textTmplsMu.RLock()
	tmpl, ok := textTmpls[name]
	textTmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}

	tmpl = textTemplate.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseTextTmpl.Parse: %w", err)
	}

	textTmplsMu.Lock()
	textTmpls[name] = tmpl
	textTmplsMu.Unlock()

	return tmpl, nil
}

//go:embed static/*
var staticFS embed.FS

//go:embed migrations/*
var migrationsFS embed.FS

var appWg sync.WaitGroup
var db *sql.DB
var appCtx context.Context
var cancelAppCtx context.CancelFunc
var rwDB *sql.DB
var metaSetup atomicString
var metaName atomicString
var metaDomain atomicString
var metaUnconfirmedDomain atomicString
var metaUnconfirmedDomainProblem atomicString

var metaSSL atomicString

var metaConfigFileEnabled atomic.Bool

type statusCtxKey struct{}

type pageCtx struct {
	Status                   string
	Auth                     authCtx
	Index                    bool
	Name                     string
	HXRequest                bool
	HXBoosted                bool
	AdminArea                bool
	Nav                      string
	UnconfirmedDomainProblem string
	UnconfirmedDomain        string
	HideUnconfirmedDomain    bool
	ShouldAttemptRedirect    bool
	Domain                   string
	ConfigFile               bool
}

func getPageCtx(r *http.Request) pageCtx {
	status := ""
	if val, ok := r.Context().Value(statusCtxKey{}).(string); ok {
		status = val
	}

	authCtx := getAuthCtx(r)

	adminURLPrefix := ""
	adminArea := false
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		adminURLPrefix = strings.Split(r.URL.Path, "/")[2]
		adminArea = true
	}

	// r.Host is client-supplied and net/http admits bytes that fail to parse
	// here ("%zz", "[bad", "a:b:c"), which returned a nil URL that the
	// redirect check below dereferenced.
	hostname := ""
	if parsedURL, err := url.ParseRequestURI("https://" + r.Host); err == nil {
		hostname = parsedURL.Hostname()
	}

	return pageCtx{
		Status:                   status,
		Auth:                     authCtx,
		Index:                    r.URL.Path == "/" || r.URL.Path == "/history",
		Name:                     metaName.Load(),
		HXRequest:                r.Header.Get("HX-Request") == "true",
		HXBoosted:                r.Header.Get("HX-Boosted") == "true",
		AdminArea:                adminArea,
		Nav:                      adminURLPrefix,
		UnconfirmedDomainProblem: metaUnconfirmedDomainProblem.Load(),
		UnconfirmedDomain:        metaUnconfirmedDomain.Load(),
		HideUnconfirmedDomain:    r.URL.Path == "/admin/settings",
		ShouldAttemptRedirect: metaSSL.Load() == "true" && authCtx.ID != 0 &&
			metaDomain.Load() != "" && hostname != metaDomain.Load(),
		Domain:     metaDomain.Load(),
		ConfigFile: metaConfigFileEnabled.Load(),
	}
}

func csrfMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}

		csrfToken := r.Header.Get("csrf-token")
		authCtx := getAuthCtx(r)

		// Constant-time: a 32-byte token is not realistically timeable over a
		// network, but the comparison costs nothing either way.
		if subtle.ConstantTimeCompare([]byte(csrfToken), []byte(authCtx.CSRFToken)) != 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		h.ServeHTTP(w, r)
	})
}

func statusMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.Begin()
		if err != nil {
			log.Printf("statusMiddleware.Begin: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		severity, err := getSeverity(tx)
		if err != nil {
			tx.Rollback()
			log.Printf("statusMiddleware.getSeverity: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("statusMiddleware.Commit: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), statusCtxKey{}, severity)

		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

type authCtxKey struct{}

type authCtx struct {
	ID        int
	CSRFToken string
}

func getAuthCtx(r *http.Request) authCtx {
	ctx := authCtx{}
	if val, ok := r.Context().Value(authCtxKey{}).(authCtx); ok {
		ctx = val
	}

	return ctx
}

func createMonitorLog(
	tx *sql.Tx,
	startedAt time.Time,
	endedAt time.Time,
	responseCode int64,
	errorMessage sql.NullString,
	attempts int,
	result string,
	monitorID int,
) (int, error) {
	const query = `
		insert into 
			monitor_log(started_at, ended_at, response_code, error_message, 
				attempts, result, monitor_id)
			values(?, ?, ?, ?, ?, ?, ?)
		returning id
	`

	var id int

	err := tx.QueryRow(
		query,
		startedAt,
		endedAt,
		responseCode,
		errorMessage,
		attempts,
		result,
		monitorID,
	).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createMonitorLog.Exec: %w", err)
	}

	return id, nil
}

func createMonitorLogLastChecked(
	tx *sql.Tx,
	startedAt time.Time,
	monitorID int,
	monitorLogID int,
) error {
	const query = `
		insert into monitor_log_last_checked(checked_at, monitor_id, monitor_log_id)
			values(?, ?, ?) 
		on conflict(monitor_id) do update set 
			checked_at = excluded.checked_at,
			monitor_log_id = excluded.monitor_log_id
	`

	_, err := tx.Exec(
		query,
		startedAt,
		monitorID,
		monitorLogID,
	)
	if err != nil {
		return fmt.Errorf("createMonitorLogLastChecked.Exec: %w", err)
	}

	return nil
}

type monitorLogLastChecked struct {
	ID           int
	CheckedAt    time.Time
	ResponseCode sql.NullInt32
}

func getMonitorLogLastChecked(
	tx *sql.Tx,
	monitorID int,
) (monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
		where monitor_log_last_checked.monitor_id = ?
	`

	var lastChecked monitorLogLastChecked

	err := tx.QueryRow(query, monitorID).Scan(
		&lastChecked.ID,
		&lastChecked.CheckedAt,
		&lastChecked.ResponseCode,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lastChecked, nil
		}
		return lastChecked, fmt.Errorf("getMonitorLogLastChecked.QueryRow: %w", err)
	}

	return lastChecked, nil
}

func listAllMonitorLogLastChecked(tx *sql.Tx) ([]monitorLogLastChecked, error) {
	const query = `
		select monitor_log.monitor_id, checked_at, monitor_log.response_code 
		from monitor_log_last_checked
		left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
	`

	var allLastChecked []monitorLogLastChecked

	rows, err := tx.Query(query)
	if err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var lastChecked monitorLogLastChecked

		err := rows.Scan(
			&lastChecked.ID,
			&lastChecked.CheckedAt,
			&lastChecked.ResponseCode,
		)
		if err != nil {
			return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.Scan: %w", err)
		}

		allLastChecked = append(allLastChecked, lastChecked)
	}

	if err := rows.Err(); err != nil {
		return allLastChecked, fmt.Errorf("listAllMonitorLogLastChecked.RowsErr: %w", err)
	}

	return allLastChecked, nil
}

type plainOrLoginAuth struct {
	username string
	password string
	host     string
	auth     string
}

func PlainOrLoginAuth(username string, password string, host string) smtp.Auth {
	return &plainOrLoginAuth{username: username, password: password, host: host}
}

func (a *plainOrLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("plainAuth.Start: unencrypted connection")
	}

	if slices.Contains(server.Auth, "PLAIN") {
		a.auth = "PLAIN"
		return smtp.PlainAuth("", a.username, a.password, a.host).Start(server)
	}

	if slices.Contains(server.Auth, "LOGIN") {
		a.auth = "LOGIN"
		return "LOGIN", []byte(a.username), nil
	}

	return "", nil, fmt.Errorf("plainAuth.Start: unhandled auth")
}

func (a *plainOrLoginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if a.auth == "PLAIN" {
		return smtp.PlainAuth("", a.username, a.password, a.host).Next(fromServer, more)
	}

	if a.auth == "LOGIN" && more {
		switch string(fromServer) {
		case "Username:":
			return []byte(a.username), nil
		case "Password:":
			return []byte(a.password), nil
		default:
			return nil, fmt.Errorf("plainAuth.Next: unexpected from server")
		}
	}
	return nil, nil
}

func sendMonitorAlertEmail(
	monitor Monitor,
	channel NotificationChannel,
	statusCode sql.NullInt64,
	result string,
	startedAt time.Time,
	emailAddresses []string,
	status string,
) error {
	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if !ok {
		return fmt.Errorf(
			"sendMonitorAlertEmail.SMTPAssert: failed to assert channel %d",
			channel.ID,
		)
	}

	smtpAuth := PlainOrLoginAuth(
		smtpDetail.Username,
		smtpDetail.Password,
		smtpDetail.Host,
	)

	const downSubject = "🚨 Issue detected on monitor"
	const upSubject = "✅ Issue resolved on monitor"

	subject := downSubject
	if status == "up" {
		subject = upSubject
	}

	msg := [][]byte{
		[]byte("Subject: " + headerValue(subject) + " \"" +
			headerValue(monitor.Name) + "\""),
		[]byte("To: " + headerValue(strings.Join(emailAddresses, ", "))),
		[]byte("From: " + headerValue(metaName.Load()) + " " + "<" + smtpDetail.From + ">"),
		[]byte("Content-Type: text/html; charset=UTF-8"),
	}
	for k, v := range smtpDetail.Headers {
		if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") &&
			k == "X-PM-Message-Stream" {
			continue
		}
		msg = append(msg, []byte(headerValue(k)+": "+headerValue(v)))
	}
	if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		msg = append(msg, []byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-transactional"]))
	}



	markup := sendMonitorAlertEmailDownMarkup
	if status == "up" {
		markup = sendMonitorAlertEmailUpMarkup
	}

	tmpl, err := parseEmailTmpl(status+"MonitorSMTP", markup)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.parseEmailTmplsSMTP: %w", err)
	}

	emailBytes := bytes.Buffer{}

	err = tmpl.Execute(
		&emailBytes,
		struct {
			MonitorID   int
			MonitorName string
			StatusCode  int
			CheckedAt   string
			Result      string
			Domain      string
		}{
			MonitorID:   monitor.ID,
			MonitorName: monitor.Name,
			StatusCode:  int(statusCode.Int64),
			CheckedAt:   startedAt.Format("2006/01/02 15:04:05 MST"),
			Result:      result,
			Domain:      metaDomain.Load(),
		},
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.Execute: %w", err)
	}

	emailStr := "\r\n" + emailBytes.String()

	msg = append(msg, []byte(emailStr))

	err = smtp.SendMail(
		smtpDetail.Host+":"+strconv.Itoa(smtpDetail.Port),
		smtpAuth,
		smtpDetail.From,
		emailAddresses,
		bytes.Join(msg, []byte("\r\n")),
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertEmail.SendMail: %w", err)
	}

	return nil
}

func sendMonitorAlertSlack(
	monitor Monitor,
	channel NotificationChannel,
	statusCode sql.NullInt64,
	startedAt time.Time,
	result string,
	httpClient http.Client,
	status string,
	domain string,
) error {
	slackDetail, ok := channel.Details.(SlackNotificationDetails)
	if !ok {
		return fmt.Errorf(
			"sendMonitorAlertSlack.SlackAssert: failed to assert channel %d",
			channel.ID,
		)
	}



	markup := sendMonitorAlertSlackDownMarkup
	if status == "up" {
		markup = sendMonitorAlertSlackUpMarkup
	}

	tmpl, err := parseTextTmpl(status+"MonitorSlack", markup)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.parseEmailTmplsSlack: %w", err)
	}

	emailStr := bytes.Buffer{}

	err = tmpl.Execute(
		&emailStr,
		struct {
			MonitorID   int
			MonitorName string
			StatusCode  int
			CheckedAt   string
			Result      string
			Domain      string
		}{
			MonitorID:   monitor.ID,
			MonitorName: monitor.Name,
			StatusCode:  int(statusCode.Int64),
			CheckedAt:   startedAt.Format("2006/01/02 15:04:05 MST"),
			Result:      result,
			Domain:      metaDomain.Load(),
		},
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.Execute: %w", err)
	}

	type SlackWebhookRequestBody struct {
		Text string `json:"text"`
	}

	body := SlackWebhookRequestBody{Text: emailStr.String()}

	serializedBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.MarshalSlack: %w", err)
	}

	resp, err := httpClient.Post(
		slackDetail.WebhookURL,
		"application/json",
		bytes.NewBuffer(serializedBody),
	)
	if err != nil {
		return fmt.Errorf("sendMonitorAlertSlack.Post: %w", err)
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("sendMonitorAlertSlack.ReadAll: %w", readErr)
	}

	// A revoked webhook or an archived channel answers 404 invalid_token /
	// 410 channel_is_archived, which is a perfectly successful HTTP round
	// trip. Not checking it meant every alert to that channel was dropped in
	// silence.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"sendMonitorAlertSlack.StatusCode %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(respBody)),
		)
	}

	return nil
}

func monitorLoop(ctx context.Context, wg *sync.WaitGroup) {
	checkoutMu := sync.RWMutex{}
	lastCheckedMu := sync.RWMutex{}

	lastChecked := map[int]time.Time{}
	checkout := map[int]time.Time{}

	// NewTicker, not Tick: time.Tick leaks its ticker and its goroutine, and
	// these loops do exit -- on ctx.Done, and monitorUnconfirmedDomainLoop
	// returns on its own once the domain resolves.
	ticker := time.NewTicker(time.Millisecond * 500)
	defer ticker.Stop()
	tick := ticker.C

	for {
		select {
		case <-tick:
			func() {
				tx, err := db.Begin()
				if err != nil {
					log.Printf("monitorLoop.BeginListMonitors: %s", err)
					return
				}
				defer tx.Rollback()

				monitors, err := listMonitors(tx)
				if err != nil {
					log.Printf("monitorLoop.listMonitors: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("monitorLoop.CommitListMonitors: %s", err)
					return
				}

				for _, monitor := range monitors {
					monitor := monitor

					httpClient := http.Client{
						Timeout: time.Duration(monitor.Timeout) * time.Second,
					}

					lastCheckedMu.RLock()
					if time.Since(lastChecked[monitor.ID]) <
						time.Second*time.Duration(monitor.Frequency) {
						lastCheckedMu.RUnlock()
						continue
					}
					lastCheckedMu.RUnlock()

					checkoutMu.RLock()
					if _, ok := checkout[monitor.ID]; ok {
						checkoutMu.RUnlock()
						continue
					}
					checkoutMu.RUnlock()

					checkoutMu.Lock()
					checkout[monitor.ID] = time.Now().UTC()
					checkoutMu.Unlock()

					// Registered with the app WaitGroup: these outlive the tick
					// that spawned them by up to attempts x timeout, and on SIGTERM
					// they were racing db.Close() to write their monitor log.
					wg.Add(1)
					go func() {
						defer wg.Done()

						var endedAt time.Time

						defer func() {
							checkoutMu.Lock()
							delete(checkout, monitor.ID)
							checkoutMu.Unlock()

							if endedAt.IsZero() {
								endedAt = time.Now().UTC()
							}

							lastCheckedMu.Lock()
							lastChecked[monitor.ID] = endedAt
							lastCheckedMu.Unlock()
						}()

						startedAt := time.Now().UTC()

						var errorMessage sql.NullString

						var resp *http.Response
						var reqErr error
						var statusCode sql.NullInt64
						result := ""

						attempt := 0
						for attempt = 0; attempt < monitor.Attempts; attempt++ {
							var body io.Reader
							if monitor.Body.Valid {
								body = strings.NewReader(monitor.Body.String)
							}

							// Carries the app context so a check in flight at SIGTERM
							// aborts instead of holding shutdown for up to
							// attempts x timeout.
							monitorReq, err := http.NewRequestWithContext(
								ctx,
								monitor.Method,
								monitor.URL,
								body,
							)
							if err != nil {
								log.Printf("monitorLoop.NewRequest: %s", err)
								break
							}
							for k, v := range monitor.RequestHeaders {
								monitorReq.Header.Add(k, v)
							}

							// Cleared each attempt: a 500 on attempt 1 followed by a
							// connection failure on attempt 2 used to log the failure
							// with attempt 1's status code still attached.
							statusCode = sql.NullInt64{}

							resp, reqErr = httpClient.Do(monitorReq)
							if reqErr != nil {
								continue
							}

							_, err = io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
							if err != nil {
								log.Printf("monitorLoop.Copy: %s", err)
								break
							}

							statusCode = sql.NullInt64{
								Int64: int64(resp.StatusCode),
								Valid: true,
							}

							if resp.StatusCode >= 400 {
								result = "error"
								continue
							}

							result = "success"

							break
						}

						if result != "success" {
							urlErr := &url.Error{}
							if ok := errors.As(reqErr, &urlErr); ok {
								errorMessage = sql.NullString{
									String: reqErr.Error(),
									Valid:  true,
								}

								// Only a genuine timeout is reported as one. A DNS
								// failure, a refused connection and a TLS error are
								// all *url.Error too, and calling every one of them
								// "timeout" sends whoever is debugging the outage
								// looking in the wrong place.
								if urlErr.Timeout() || errors.Is(reqErr, context.DeadlineExceeded) {
									result = "timeout"
								} else {
									result = "error"
								}
							}
						}

						// attempt is the loop counter, which stops at the index of
						// the attempt that succeeded -- so a first-try success was
						// recorded as 0 attempts while a total failure recorded the
						// full count. The column is shown to the operator.
						attemptsMade := attempt
						if result == "success" {
							attemptsMade = attempt + 1
						}

						endedAt = time.Now().UTC()

						tx, err := rwDB.Begin()
						if err != nil {
							log.Printf("monitorLoop.BeginMonitorLog: %s", err)
							return
						}
						defer tx.Rollback()

						monitorLogID, err := createMonitorLog(
							tx,
							startedAt,
							endedAt,
							statusCode.Int64,
							errorMessage,
							attemptsMade,
							result,
							monitor.ID,
						)
						if err != nil {
							log.Printf("monitorLoop.createMonitorLog: %s", err)
							return
						}

						lastChecked, err := getMonitorLogLastChecked(tx, monitor.ID)
						if err != nil {
							log.Printf(
								"monitorLoop.checkNotificationDueByMonitorID: %s",
								err,
							)
							return
						}

						err = createMonitorLogLastChecked(tx, endedAt, monitor.ID, monitorLogID)
						if err != nil {
							log.Printf("monitorLoop.createMonitorLogLastChecked: %s", err)
							return
						}

						err = tx.Commit()
						if err != nil {
							log.Printf("monitorLoop.CommitMonitorLog: %s", err)
							return
						}

						tx, err = db.Begin()
						if err != nil {
							log.Printf("monitorLoop.BeginListNotificationsByMonitorID: %s", err)
							return
						}
						defer tx.Rollback()

						channels, err := listNotificationChannelsByMonitorID(tx, monitor.ID)
						if err != nil {
							log.Printf("monitorLoop.listNotificationChannelsByMonitorID: %s", err)
							return
						}

						err = tx.Commit()
						if err != nil {
							log.Printf("monitorLoop.CommitListNotificationChannelsByMonitorID: %s", err)
							return
						}

						if len(channels) > 0 {
							// ID is 0 only when there is no previous check at all
							// (getMonitorLogLastChecked returns a zero struct on
							// ErrNoRows). Without this a monitor added for an endpoint
							// that is ALREADY down has no happy state to transition
							// from, so it never alerts -- and every later failure looks
							// like more of the same. The team first hears about the
							// outage when it recovers.
							firstCheck := lastChecked.ID == 0

							lastHappy := lastChecked.ResponseCode.Int32 != 0 &&
								lastChecked.ResponseCode.Int32 < 400

							if firstCheck && result != "success" ||
								lastHappy && result != "success" ||
								!lastHappy && result == "success" {
								status := "down"
								if result == "success" {
									status = "up"
								}

								tx, err = db.Begin()
								if err != nil {
									log.Printf("monitorLoop.BeginListMailGroupMembersEmailsByMonitorID: %s", err)
									return
								}
								defer tx.Rollback()

								emailAddresses, err := listMailGroupMembersEmailsByMonitorID(
									tx,
									monitor.ID,
								)
								if err != nil {
									log.Printf("monitorLoop.listMailGroupMembersEmailsByMonitorID: %s", err)
									return
								}

								err = tx.Commit()
								if err != nil {
									log.Printf("monitorLoop.CommitListMailGroupMembersEmailsByMonitorID: %s", err)
									tx.Rollback()
									return
								}

								skipEmail := len(emailAddresses) == 0

								for _, channel := range channels {
									if channel.Type == "smtp" && !skipEmail {
										err := sendMonitorAlertEmail(
											monitor,
											channel,
											statusCode,
											result,
											startedAt,
											emailAddresses,
											status,
										)
										if err != nil {
											log.Printf("monitorLoop.sendMonitorAlertEmail: %s", err)
											continue
										}
										skipEmail = true
									} else if channel.Type == "slack" {
										err = sendMonitorAlertSlack(
											monitor,
											channel,
											statusCode,
											startedAt,
											result,
											httpClient,
											status,
											metaDomain.Load(),
										)
										if err != nil {
											log.Printf("monitorLoop.sendMonitorAlertSlack: %s", err)
											continue
										}
									}
								}
							}
						}
					}()
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}

type UnsentAlertNotification struct {
	AlertNotificationID int
	Destination         string
	Content             string
	Type                string
	AlertMessageID      int
	AlertTitle          string
	AlertType           string
	AlertSeverity       string
	AlertServices       string
}

func listUnsentAlertNotifications(tx *sql.Tx) ([]UnsentAlertNotification, error) {
	const query = `
		select 
			alert_notification.id, alert_subscription.destination, alert_message.content, 
			alert_subscription.type, alert_message.id, alert.title, alert.type, alert.severity, 
			group_concat(service.name, " • ")
		from alert_notification
		left join alert_subscription on alert_subscription.id = alert_subscription_id
		left join alert_message on alert_message.id = alert_message_id
		left join alert on alert.id = alert_message.alert_id
		left join alert_service on alert_service.alert_id = alert_message.alert_id
		left join service on service.id = alert_service.service_id
		where alert_notification.sent_at is null
		group by alert_notification.id
		order by alert_message.created_at asc
	`

	notifications := []UnsentAlertNotification{}

	rows, err := tx.Query(query)
	if err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		notification := UnsentAlertNotification{}
		err := rows.Scan(
			&notification.AlertNotificationID,
			&notification.Destination,
			&notification.Content,
			&notification.Type,
			&notification.AlertMessageID,
			&notification.AlertTitle,
			&notification.AlertType,
			&notification.AlertSeverity,
			&notification.AlertServices,
		)
		if err != nil {
			return notifications, fmt.Errorf("listUnsentAlertNotifications.Scan: %w", err)
		}

		notifications = append(notifications, notification)
	}

	if err := rows.Err(); err != nil {
		return notifications, fmt.Errorf("listUnsentAlertNotifications.RowsErr: %w", err)
	}

	return notifications, nil
}

func notificationLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()
	tick := ticker.C
	for {
		select {
		case <-tick:
			func() {
				tx, err := db.Begin()
				if err != nil {
					log.Printf("notificationLoop.BeginListUnsentAlertNotifications: %s", err)
					return
				}
				defer tx.Rollback()

				notifications, err := listUnsentAlertNotifications(tx)
				if err != nil {
					log.Printf("notificationLoop.listUnsentAlertNotifications: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("notificationLoop.CommitListUnsentAlertNotifications: %s", err)
					return
				}

				if len(notifications) == 0 {
					return
				}

				// Hoisted out of the per-notification loop below. All four are
				// constant for the batch, and listUnsentAlertNotifications
				// returns one row per SUBSCRIBER -- so reloading every active
				// subscription inside the loop made a single alert update
				// O(N^2) in subscribers, against the same file the monitor
				// loop is writing to.
				tx, err = db.Begin()
				if err != nil {
					log.Printf("notificationLoop.ReadBegin: %s", err)
					return
				}
				defer tx.Rollback()

				notificationChannelID, err := getAlertSMTPNotificationSetting(tx)
				if err != nil {
					log.Printf("notificationLoop.getAlertSMTPNotificationSetting: %s", err)
					return
				}

				notificationChannel, err := getNotificationChannelByID(tx, notificationChannelID)
				if err != nil {
					log.Printf("notificationLoop.getNotificationChannelByID: %s", err)
					return
				}

				alertSettings, err := getAlertSettings(tx)
				if err != nil {
					log.Printf("notificationLoop.getAlertSettings: %s", err)
					return
				}

				emailSubs, err := listActiveAlertEmailSubscriptions(tx)
				if err != nil {
					log.Printf("notificationLoop.listActiveAlertEmailSubscriptions: %s", err)
					return
				}

				subTokensEmailMap := make(map[string]string, len(emailSubs))
				for _, v := range emailSubs {
					subTokensEmailMap[v.Destination] = v.Meta
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("notificationLoop.ReadCommit: %s", err)
					return
				}

				for _, notification := range notifications {
					func() {
						severityEmoji := "🟠"
						if notification.AlertSeverity == "red" {
							severityEmoji = "🔴"
						}

						if notification.Type == "slack" {
							httpClient := http.Client{
								Timeout: time.Second * 10,
							}


							tmpl, err := parseTextTmpl("alertSlack", notificationLoopText)
							if err != nil {
								log.Printf("notificationLoop.parseEmailTmplsSlack: %s", err)
								return
							}

							notificationStr := bytes.Buffer{}

							err = tmpl.Execute(
								&notificationStr,
								struct {
									Title     string
									Content   string
									Services  string
									AlertType string
									Severity  string
									Domain    string
								}{
									// Escaped: the template above is text/template, which
									// escapes nothing, so a quote in a title broke the JSON.
									Title: jsonString(strings.ToUpper(notification.AlertType[:1]) +
										notification.AlertType[1:] + " - " + notification.AlertTitle),
									Content:   jsonString(notification.Content),
									Services:  jsonString(notification.AlertServices),
									AlertType: notification.AlertType,
									Severity:  jsonString(severityEmoji),
									Domain:    jsonString(metaDomain.Load()),
								},
							)
							if err != nil {
								log.Printf("notificationLoop.ExecuteSlack: %s", err)
								return
							}

							resp, err := httpClient.Post(
								notification.Destination,
								"application/json",
								&notificationStr,
							)
							if err != nil {
								log.Printf("notificationLoop.Post: %s", err)
								return
							}
							slackBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
							resp.Body.Close()
							if readErr != nil {
								log.Printf("notificationLoop.ReadAllSlack: %s", readErr)
								return
							}

							// Returning here leaves sent_at null, so the next tick
							// retries. Stamping it on a 404 from a revoked webhook
							// dropped the alert permanently and reported nothing.
							if resp.StatusCode != http.StatusOK {
								log.Printf(
									"notificationLoop.PostStatusCode %d: %s",
									resp.StatusCode,
									strings.TrimSpace(string(slackBody)),
								)
								return
							}

							tx, err := rwDB.Begin()
							if err != nil {
								log.Printf("notificationLoop.SlackBegin: %s", err)
								return
							}
							defer tx.Rollback()

							err = updateAlertSentAtByID(
								tx,
								time.Now().UTC(),
								[]int{notification.AlertNotificationID},
							)
							if err != nil {
								log.Printf("notificationLoop.updateAlertSentAtByID: %s", err)
								return
							}

							err = tx.Commit()
							if err != nil {
								log.Printf("notificationLoop.SlackCommit: %s", err)
								return
							}
						} else if notification.Type == "email" {
							smtpDetail, ok := notificationChannel.Details.(SMTPNotificationDetails)
							if !ok {
								log.Printf(
									"notificationLoop.NotificationDetailsAssert: channel %d is not SMTP",
									notificationChannel.ID,
								)
								return
							}

							msg := [][]byte{
								[]byte("Subject: " + headerValue(metaName.Load()) + " " + notification.AlertType +
									" alert: update regarding \"" + headerValue(notification.AlertTitle) + "\""),
								[]byte("To: " + headerValue(notification.Destination)),
								[]byte("From: " + headerValue(metaName.Load()) + " " + "<" + smtpDetail.From + ">"),
								[]byte("Content-Type: text/html; charset=UTF-8"),
							}
							for k, v := range smtpDetail.Headers {
								if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") &&
									k == "X-PM-Message-Stream" {
									continue
								}
								msg = append(msg, []byte(headerValue(k)+": "+headerValue(v)))
							}
							if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
								msg = append(
									msg,
									[]byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-broadcast"]),
								)
							}

							if alertSettings.ManagedSubscriptions {
								msg = append(
									msg,
									[]byte("List-Unsubscribe-Post: List-Unsubscribe=One-Click"),
									[]byte("List-Unsubscribe: "+
										"<https://"+metaDomain.Load()+
										"/unsubscribe?token="+subTokensEmailMap[notification.Destination]+">"),
								)
							}


							tmpl, err := parseEmailTmpl("alert", notificationLoopMarkup)
							if err != nil {
								log.Printf("notificationLoop.parseEmailTmpls: %s", err)
								return
							}

							emailBytes := bytes.Buffer{}

							err = tmpl.Execute(
								&emailBytes,
								struct {
									Notification         UnsentAlertNotification
									SeverityEmoji        string
									Domain               string
									ManagedSubscriptions bool
									SubToken             string
								}{
									Notification:         notification,
									SeverityEmoji:        severityEmoji,
									Domain:               metaDomain.Load(),
									ManagedSubscriptions: alertSettings.ManagedSubscriptions,
									SubToken:             subTokensEmailMap[notification.Destination],
								},
							)
							if err != nil {
								log.Printf("notificationLoop.ExecuteSMTP: %s", err)
								return
							}

							emailStr := "\r\n" + emailBytes.String()

							msg = append(msg, []byte(emailStr))

							err = smtp.SendMail(
								smtpDetail.Host+":"+strconv.Itoa(smtpDetail.Port),
								PlainOrLoginAuth(
									smtpDetail.Username,
									smtpDetail.Password,
									smtpDetail.Host,
								),
								smtpDetail.From,
								[]string{notification.Destination},
								bytes.Join(msg, []byte("\r\n")),
							)
							if err != nil {
								log.Printf("notificationLoop.SendMail: %s", err)
								return
							}

							tx, err := rwDB.Begin()
							if err != nil {
								log.Printf("notificationLoop.BeginUpdateAlertSentAtByIDEmail: %s", err)
								return
							}
							defer tx.Rollback()

							err = updateAlertSentAtByID(
								tx,
								time.Now().UTC(),
								[]int{notification.AlertNotificationID},
							)
							if err != nil {
								log.Printf("notificationLoop.updateAlertSentAtByIDEmail: %s", err)
								return
							}

							err = tx.Commit()
							if err != nil {
								log.Printf("notificationLoop.CommitUpdateAlertSentAtByIDEmail: %s", err)
								return
							}
						}
					}()
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}

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

var staticETags = map[string]string{}

// serveDecompressed inflates a gzip-only embedded asset for a client that did
// not advertise gzip. Rare enough to do on the fly; the alternative is
// embedding a second copy of every asset.
func serveDecompressed(w http.ResponseWriter, gzPath string) {
	file, err := staticFS.Open(gzPath)
	if err != nil {
		log.Printf("serveDecompressed.Open %s: %s", gzPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		log.Printf("serveDecompressed.NewReader %s: %s", gzPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	if _, err := io.Copy(w, reader); err != nil {
		log.Printf("serveDecompressed.Copy %s: %s", gzPath, err)
	}
}

func neuter(next http.Handler) http.Handler {
	gzAvailable := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			if strings.HasSuffix(d.Name(), ".gz") {
				gzAvailable[strings.Replace(path, ".gz", "", 1)] = path
			}

			contents, err := staticFS.ReadFile(path)
			if err != nil {
				return fmt.Errorf("neuter.ReadFile %s: %w", path, err)
			}
			sum := sha256.Sum256(contents)
			staticETags[path] = `"` + hex.EncodeToString(sum[:16]) + `"`
		}
		return nil
	})
	if err != nil {
		log.Fatalf("neuter.WalkDir: %s", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}

		// embed.FS reports a zero ModTime, so ServeContent emits no
		// Last-Modified and there is nothing to revalidate against. Without a
		// freshness directive every page view re-fetched all 2.1 MB of
		// static/, monaco included. The assets are baked into the binary, so
		// they cannot change without the ETag changing with them.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if etag, ok := staticETags[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			w.Header().Set("ETag", etag)
			if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		// These eleven assets are embedded ONLY in their compressed form --
		// there is no plain copy to fall back to -- so a client that does not
		// advertise gzip is served a decompressed copy rather than a response
		// labelled with an encoding it never asked for.
		if gzPath, ok := gzAvailable[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			split := strings.Split(gzPath, ".")
			ext := split[len(split)-2]
			w.Header().Add("Content-Type", mime.TypeByExtension("."+ext))
			w.Header().Add("Vary", "Accept-Encoding")

			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				r.URL.Path = gzPath
				w.Header().Add("Content-Encoding", "gzip")
				next.ServeHTTP(w, r)
				return
			}

			serveDecompressed(w, gzPath)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func attemptCertificateAcquisition(ctx context.Context, domain string) error {
	var testCache *certmagic.Cache
	testCache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(testCache, certmagic.Config{}), nil
		},
	})
	testCache.Stop()

	testMagic := certmagic.New(testCache, certmagic.Config{})

	testACME := certmagic.NewACMEIssuer(testMagic, certmagic.ACMEIssuer{
		CA:     certmagic.LetsEncryptStagingCA,
		Email:  " ",
		Agreed: true,
	})

	testMagic.Issuers = []certmagic.Issuer{testACME}

	err := testMagic.ObtainCertSync(ctx, domain)
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.ObtainCertSync: %w", err)
	}

	fileStorage, ok := certmagic.Default.Storage.(*certmagic.FileStorage)
	if !ok {
		// err is nil here, and %w with a nil operand yields an error whose
		// text is %!w(<nil>) and which errors.As can never match -- so the
		// caller's ACME-problem branch fell through to "unexpected error".
		return errors.New("attemptCertificateAcquisition: storage is not a certmagic.FileStorage")
	}

	err = os.RemoveAll(
		fileStorage.Filename(certmagic.StorageKeys.CertsPrefix(testACME.IssuerKey())),
	)
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.RemoveAll: %w", err)
	}

	err = certmagic.ManageSync(ctx, []string{domain})
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.ManageSync: %w", err)
	}

	return nil
}

func monitorUnconfirmedDomainLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(time.Minute * 1)
	defer ticker.Stop()
	tick := ticker.C

	for {
		if metaUnconfirmedDomain.Load() == "" || metaUnconfirmedDomainProblem.Load() != "" {
			wg.Done()
			return
		}

		select {
		case <-tick:
			func() {
				found, err := lookupDomain(metaUnconfirmedDomain.Load())
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.lookupDomain: %s", err)
					return
				}

				if !found {
					return
				}

				err = attemptCertificateAcquisition(ctx, metaUnconfirmedDomain.Load())
				if err != nil {
					unconfirmedDomainProblemMsg := "An unexpected error occurred"

					var acmeProblem acme.Problem
					if errors.As(err, &acmeProblem) {
						var ok bool
						unconfirmedDomainProblemMsg, ok = acmeProblemTypeMessages[acmeProblem.Type]
						if !ok {
							unconfirmedDomainProblemMsg = "An unhandled error occurred " +
								acmeProblem.Type
						}
					} else {
						log.Printf("monitorUnconfirmedDomainLoop.attemptCertificateAcquisition: %s", err)
					}

					tx, err := rwDB.Begin()
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.BeginUnconfirmedDomainProblem: %s", err)
						return
					}
					defer tx.Rollback()

					metaUnconfirmedDomainProblem.Store(unconfirmedDomainProblemMsg)
					err = updateMetaValue(tx, "unconfirmedDomainProblem", metaUnconfirmedDomainProblem.Load())
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.UpdateUnconfirmedDomainProblem: %s", err)
						return
					}

					if err := tx.Commit(); err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.CommitUnconfirmedDomainProblem: %s", err)
						return
					}

					return
				}

				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Begin: %s", err)
					return
				}
				defer tx.Rollback()

				metaDomain.Store(metaUnconfirmedDomain.Load())
				err = updateMetaValue(tx, "domain", metaUnconfirmedDomain.Load())
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueDomain: %s", err)
					return
				}

				metaUnconfirmedDomain.Store("")
				err = updateMetaValue(tx, "unconfirmedDomain", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomain: %s", err)
					return
				}

				metaUnconfirmedDomainProblem.Store("")
				err = updateMetaValue(tx, "unconfirmedDomainProblem", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomainProblem: %s", err)
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Commit: %s", err)
					return
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}

func GenerateSelfSignedCertificate() {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate key: %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatalf("Failed to generate serial number: %v", err)
	}

	notBefore := time.Now().UTC()
	notAfter := notBefore.Add(time.Hour * 720)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Statusnook Installer"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		log.Fatalf("Failed to create certificate: %v", err)
	}

	fingerprint := sha256.Sum256(derBytes)
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	certFile, err := os.Create(SELF_SIGNED_CERT_NAME)
	if err != nil {
		log.Fatalf("Failed to open %s for writing: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		log.Fatalf("Failed to write data to %s: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := certFile.Close(); err != nil {
		log.Fatalf("Error closing %s: %v", SELF_SIGNED_CERT_NAME, err)
	}

	keyOut, err := os.OpenFile(SELF_SIGNED_KEY_NAME, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Failed to open %s for writing: %v", SELF_SIGNED_KEY_NAME, err)
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		log.Fatalf("Unable to marshal private key: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		log.Fatalf("Failed to write data to %s: %v", SELF_SIGNED_KEY_NAME, err)
	}
	if err := keyOut.Close(); err != nil {
		log.Fatalf("Error closing %s: %v", SELF_SIGNED_KEY_NAME, err)
	}

	formattedFingerprint := ""
	for i := 0; i < len(fingerprintHex); i += 2 {
		formattedFingerprint +=
			strings.ToUpper(string(fingerprintHex[i])+string(fingerprintHex[i+1])) + ":"
	}
	formattedFingerprint = formattedFingerprint[:len(formattedFingerprint)-1]
	fmt.Println(formattedFingerprint)
}

var dockerFlag = flag.Bool("docker", false, "")

type StatusnookConfigSlackNotificationChannel struct {
	WebhookURL string `json:"webhookURL" yaml:"webhook-url"`
}

type StatusnookConfigSMTPNotificationChannel struct {
	Host     string            `json:"host" yaml:"host"`
	Port     int               `json:"port" yaml:"port"`
	Username string            `json:"username" yaml:"username"`
	Password string            `json:"password" yaml:"password"`
	From     string            `json:"from" yaml:"from"`
	Headers  map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Misc     map[string]string `json:"misc,omitempty" yaml:"misc,omitempty"`
}

type StatusnookConfigMonitor struct {
	Name                 string            `json:"name" yaml:"name"`
	URL                  string            `json:"url" yaml:"url"`
	Method               string            `json:"method" yaml:"method"`
	Frequency            int               `json:"frequency" yaml:"frequency"`
	Timeout              int               `json:"timeout" yaml:"timeout"`
	Attempts             int               `json:"attempts" yaml:"attempts"`
	RequestHeaders       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	RequestBody          any               `json:"body,omitempty" yaml:"body,omitempty"`
	NotificationChannels []string          `json:"notification-channels,omitempty" yaml:"notification-channels,omitempty"`
	MailGroups           []string          `json:"mail-groups,omitempty" yaml:"mail-groups,omitempty"`
}

type StatusnookConfigGeneralSettings struct {
	Name string `json:"name" yaml:"name"`
}

type StatusnookConfigService struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type StatusnookConfigAlertNotificationSettings struct {
	EmailNotificationChannel string `json:"email-notification-channel,omitempty" yaml:"email-notification-channel,omitempty"`
	ManagedSubscriptions     bool   `json:"managed-subscriptions,omitempty" yaml:"managed-subscriptions,omitempty"`
	SlackClientSecret        string `json:"slack-client-secret,omitempty" yaml:"slack-client-secret,omitempty"`
	SlackInstallURL          string `json:"slack-install-url,omitempty" yaml:"slack-install-url,omitempty"`
}

type StatusnookConfigMailGroup struct {
	Name        string   `json:"name" yaml:"name"`
	Members     []string `json:"members,omitempty" yaml:"members,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
}

type StatusnookConfig struct {
	GeneralSettings           StatusnookConfigGeneralSettings           `json:"general-settings" yaml:"general-settings"`
	MailGroups                map[string]StatusnookConfigMailGroup      `json:"mail-groups,omitempty" yaml:"mail-groups,omitempty"`
	NotificationChannels      map[string]map[string]any                 `json:"notification-channels,omitempty" yaml:"notification-channels,omitempty"`
	Monitors                  map[string]StatusnookConfigMonitor        `json:"monitors,omitempty" yaml:"monitors,omitempty"`
	Services                  map[string]StatusnookConfigService        `json:"services,omitempty" yaml:"services,omitempty"`
	AlertNotificationSettings StatusnookConfigAlertNotificationSettings `json:"alert-notification-settings,omitempty" yaml:"alert-notification-settings,omitempty"`
	Rename                    map[string]string                         `json:"rename,omitempty" yaml:"rename,omitempty"`
}

func applyConfig(tx *sql.Tx, cfgBytes []byte) ([]string, error) {
	msgs := []string{}

	decryptedCfg := string(cfgBytes)

	key, err := getMetaValue(tx, "secretKey")
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.getMetaValue: %w", err)
	}

	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.DecodeStringKey: %w", err)
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.NewCipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.NewGCM: %w", err)
	}

	slugPattern := regexp.MustCompile(`^-+|[^\p{Ll}\d-]+|-+$`)

	secretPattern := regexp.MustCompile(`\bsecret_[A-Za-z0-9+\/=.]+`)
	secretMatches := secretPattern.FindAllString(string(cfgBytes), -1)

	for _, v := range secretMatches {
		nonceSplit := strings.Split(v, ".")
		if len(nonceSplit) != 2 {
			continue
		}

		ciphertext, err := base64.StdEncoding.DecodeString(
			strings.TrimPrefix(nonceSplit[0], "secret_"),
		)
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.DecodeStringCiphertext: %w", err)
		}

		nonce, err := base64.StdEncoding.DecodeString(nonceSplit[1])
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.DecodeStringNonce: %w", err)
		}

		plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			continue
		}

		decryptedCfg = strings.ReplaceAll(decryptedCfg, v, string(plaintext))
	}

	cfg := StatusnookConfig{}
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(decryptedCfg)))
	decoder.KnownFields(true)
	err = decoder.Decode(&cfg)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.Decode: %w", err)
	}

	renameSrcMsg := func(k string) {
		msgs = append(msgs, "rename: '"+k+
			"' does not exist, drop this rename if you've already completed it")
	}

	renameDstMsg := func(entityType string, src string, dst string) {
		msgs = append(msgs, "rename: replace "+entityType+" '"+src+"' with "+" '"+dst+
			"' to perform a rename")
	}

	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	duplicateSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, "rename: "+entityType+"."+slug+
			" would not be unique")
	}

	for k, v := range cfg.Rename {
		validSrc := true
		validRename := true

		split := strings.Split(k, ".")
		if len(split) != 2 {
			validRename = false
			msgs = append(msgs, "rename: '"+k+"'"+
				" invalid")
			continue
		}

		entityType := split[0]
		src := split[1]

		if src == v {
			validRename = false
			msgs = append(msgs, "rename: service "+" '"+src+"' to "+" '"+v+
				"' is invalid")
		}

		if slugPattern.MatchString(v) {
			validRename = false
			msgs = append(msgs, "rename: '"+v+"'"+
				" must only contain lower-case letters, numbers, and hyphens")
		}

		if entityType == "services" {
			_, err := updateServiceSlug(tx, src, v)
			if err != nil {
				var sqliteErr sqlite3.Error

				if errors.Is(err, sql.ErrNoRows) {
					validSrc = false
					if validRename {
						renameSrcMsg(k)
					}
				} else if errors.As(err, &sqliteErr) {
					if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
						duplicateSlugMsg("services", v)
					}
				} else {
					return msgs, fmt.Errorf("applyConfig.updateServiceSlug: %w", err)
				}
			}

			if validSrc && validRename {
				if _, ok := cfg.Services[v]; !ok {
					renameDstMsg("service", src, v)
				}
			}
		} else if entityType == "notification-channels" {
			_, err := updateNotificationChannelSlug(tx, src, v)
			if err != nil {
				var sqliteErr sqlite3.Error

				if errors.Is(err, sql.ErrNoRows) {
					validSrc = false
					if validRename {
						renameSrcMsg(k)
					}
				} else if errors.As(err, &sqliteErr) {
					if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
						duplicateSlugMsg("services", v)
					}
				} else {
					return msgs, fmt.Errorf("applyConfig.updateNotificationChannelSlug: %w", err)
				}
			}

			if validSrc && validRename {
				if _, ok := cfg.NotificationChannels[v]; !ok {
					renameDstMsg("notification channel", src, v)
				}
			}
		} else if entityType == "mail-groups" {
			_, err := updateMailGroupSlug(tx, src, v)
			if err != nil {
				var sqliteErr sqlite3.Error

				if errors.Is(err, sql.ErrNoRows) {
					validSrc = false
					if validRename {
						renameSrcMsg(k)
					}
				} else if errors.As(err, &sqliteErr) {
					if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
						duplicateSlugMsg("services", v)
					}
				} else {
					return msgs, fmt.Errorf("applyConfig.updateMailGrouplug: %w", err)
				}
			}

			if validSrc && validRename {
				if _, ok := cfg.MailGroups[v]; !ok {
					renameDstMsg("mail group", src, v)
				}
			}
		} else if entityType == "monitors" {
			_, err := updateMonitorSlug(tx, src, v)
			if err != nil {
				var sqliteErr sqlite3.Error

				if errors.Is(err, sql.ErrNoRows) {
					validSrc = false
					if validRename {
						renameSrcMsg(k)
					}
				} else if errors.As(err, &sqliteErr) {
					if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
						duplicateSlugMsg("services", v)
					}
				} else {
					return msgs, fmt.Errorf("applyConfig.updateMonitorSlug: %w", err)
				}
			}

			if validSrc && validRename {
				if _, ok := cfg.Monitors[v]; !ok {
					renameDstMsg("monitor", src, v)
				}
			}
		} else {
			msgs = append(msgs, "rename: '"+k+"' is invalid")
		}
	}

	if cfg.GeneralSettings.Name != "" {
		err = updateMetaValue(tx, "name", cfg.GeneralSettings.Name)
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.updateMetaValueGeneralSettingsName: %w", err)
		}
	} else {
		msgs = append(msgs, "general-settings.name"+": is required")
	}

	existingServiceSlugs := map[string]int{}

	services, err := listServices(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listServices: %w", err)
	}

	for _, v := range services {
		if _, ok := cfg.Services[v.Slug]; !ok {
			err := deleteServiceByID(tx, v.ID)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.deleteServiceByID: %w", err)
			}
			continue
		}

		existingServiceSlugs[v.Slug] = v.ID
	}

	processedServices := map[string]bool{}

	for slug, v := range cfg.Services {
		processedServices[slug] = true

		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("services", slug)
			continue
		}

		if v.Name == "" {
			msgs = append(msgs, "services."+slug+": name is required")
		}

		if _, ok := existingServiceSlugs[slug]; ok {
			err = editService(tx, existingServiceSlugs[slug], v.Name, v.Description)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.editService: %w", err)
			}
		} else {
			err = createService(tx, slug, v.Name, v.Description)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.createService: %w", err)
			}
		}
	}

	existingMonitorSlugs := map[string]int{}

	monitors, err := listMonitors(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listMailGroups: %w", err)
	}

	for _, v := range monitors {
		existingMonitorSlugs[v.Slug] = v.ID
	}

	for slug, v := range cfg.Monitors {
		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("monitors", slug)
			continue
		}

		if v.Name == "" {
			msgs = append(msgs, "monitors."+slug+": name is required")
		}

		if v.URL == "" {
			msgs = append(msgs, "monitors."+slug+": url is required")
		}

		reqURL := v.URL
		validURL := true
		parsedReqURL, err := url.Parse(reqURL)
		if err != nil {
			validURL = false
		} else if parsedReqURL.Scheme == "" || parsedReqURL.Host == "" {
			validURL = false
		} else if parsedReqURL.Scheme != "http" && parsedReqURL.Scheme != "https" {
			validURL = false
		}
		if !validURL {
			msgs = append(msgs, "monitors."+slug+": url is invalid")
		}

		if !(strings.EqualFold(v.Method, "get") || strings.EqualFold(v.Method, "post") ||
			strings.EqualFold(v.Method, "patch") || strings.EqualFold(v.Method, "put") ||
			strings.EqualFold(v.Method, "delete")) {
			msgs = append(
				msgs,
				"monitors."+slug+": method must be one of get, post, patch, put, delete",
			)
		}

		if v.Frequency != 10 && v.Frequency != 30 && v.Frequency != 60 {
			msgs = append(msgs, "monitors."+slug+": frequency must be one of 10, 30, 60")
		}

		if v.Timeout != 5 && v.Timeout != 10 && v.Timeout != 15 {
			msgs = append(msgs, "monitors."+slug+": timeout must be one of 5, 10, 15")
		}

		if v.Attempts != 1 && v.Attempts != 2 && v.Attempts != 3 {
			msgs = append(msgs, "monitors."+slug+": attempts must be one of 1, 2, 3")
		}

		requestHeadersStr, err := json.Marshal(v.RequestHeaders)
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.MarshalRequestHeaders: %w", err)
		}

		requestBodyNullStr := sql.NullString{Valid: false}
		bodyFormat := sql.NullString{Valid: false}

		if v.RequestBody != nil {
			if _, ok := v.RequestBody.(map[string]any); ok {
				vMap := v.RequestBody.(map[string]any)

				values := url.Values{}
				for k, v := range vMap {
					str := ""
					switch vs := v.(type) {
					case int:
						str = strconv.Itoa(vs)
					case string:
						str = vs
					default:
						// A bool or a float used to become the empty string, so
						// `body: {enabled: true}` sent "enabled=" and said nothing.
						// The headers and misc maps already report this.
						msgs = append(
							msgs,
							"monitors."+slug+": invalid body value "+k+
								", must be string or number",
						)
						continue
					}
					values.Add(k, str)
				}

				bodyFormat = sql.NullString{Valid: true, String: "form"}
				requestBodyNullStr = sql.NullString{Valid: true, String: values.Encode()}
			} else if _, ok := v.RequestBody.(string); ok {
				bodyFormat = sql.NullString{Valid: true, String: "text"}
				requestBodyNullStr = sql.NullString{Valid: true, String: v.RequestBody.(string)}
			} else {
				msgs = append(msgs, "monitors."+slug+": body is invalid")
			}
		}

		if _, ok := existingMonitorSlugs[slug]; ok {
			_, err := editMonitor(
				tx,
				existingMonitorSlugs[slug],
				v.Name,
				v.URL,
				strings.ToUpper(v.Method),
				v.Frequency,
				v.Timeout,
				v.Attempts,
				sql.NullString{Valid: true, String: string(requestHeadersStr)},
				bodyFormat,
				requestBodyNullStr,
			)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.editMonitor: %w", err)
			}
		} else {
			_, err := createMonitor(
				tx,
				slug,
				v.Name,
				v.URL,
				strings.ToUpper(v.Method),
				v.Frequency,
				v.Timeout,
				v.Attempts,
				sql.NullString{Valid: true, String: string(requestHeadersStr)},
				bodyFormat,
				requestBodyNullStr,
			)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.createMonitor: %w", err)
			}
		}
	}

	existingNotificationChannelSlugs := map[string]int{}

	notificationChannels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listNotificationChannels: %w", err)
	}

	for _, v := range notificationChannels {
		if _, ok := cfg.NotificationChannels[v.Slug]; !ok {
			err := deleteNotificationChannelByID(tx, v.ID)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.deleteNotificationChannelByID: %w", err)
			}
			continue
		}

		existingNotificationChannelSlugs[v.Slug] = v.ID
	}

	for slug, v := range cfg.NotificationChannels {
		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("notification-channels", slug)
			continue
		}

		details := "{}"

		type baseNotificationChannel struct {
			Name string
			Type string
		}

		typeAny, ok := v["type"]
		if !ok {
			msgs = append(msgs, "notification-channels."+slug+": type is required")
		}

		cType, ok := typeAny.(string)
		if !ok {
			msgs = append(msgs, "notification-channels."+slug+": type is invalid")
		} else if cType == "" {
			msgs = append(msgs, "notification-channels."+slug+": type is required")
		}

		if cType != "smtp" && cType != "slack" {
			msgs = append(msgs, "notification-channels."+slug+": type must be one of smtp, slack")
		}

		nameAny, ok := v["name"]
		if !ok {
			msgs = append(msgs, "notification-channels."+slug+": name is required")
		}

		name, ok := nameAny.(string)
		if !ok {
			msgs = append(msgs, "notification-channels."+slug+": name is invalid")
		} else if name == "" {
			msgs = append(msgs, "notification-channels."+slug+": name is required")
		}

		channel := baseNotificationChannel{
			Type: cType,
			Name: name,
		}

		if channel.Type == "slack" {
			unknownProps := []string{}

			requiredProps := map[string]bool{
				"type":        true,
				"name":        true,
				"webhook-url": true,
			}

			for k := range v {
				if _, ok := requiredProps[k]; !ok {
					unknownProps = append(unknownProps, k)
				}
			}

			for _, prop := range unknownProps {
				msgs = append(
					msgs,
					"notification-channels."+slug+": "+prop+
						" is an invalid property for a slack notification channel",
				)
			}

			webhookURLAny, ok := v["webhook-url"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": webhook-url is required")
			}

			webhookURL, ok := webhookURLAny.(string)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": webhook-url is invalid")
			} else if webhookURL == "" {
				msgs = append(msgs, "notification-channels."+slug+": webhook-url is required")
			}

			validURL := true
			parsedReqURL, err := url.Parse(webhookURL)
			if err != nil {
				validURL = false
			} else if parsedReqURL.Scheme == "" || parsedReqURL.Host == "" {
				validURL = false
			} else if parsedReqURL.Scheme != "http" && parsedReqURL.Scheme != "https" {
				validURL = false
			}

			if !validURL {
				msgs = append(msgs, "notification-channels."+slug+": webhook-url is invalid")
			}

			d := StatusnookConfigSlackNotificationChannel{WebhookURL: webhookURL}
			detailBytes, err := json.Marshal(d)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.MarshalSlackNotificationDetails: %w", err)
			}

			details = string(detailBytes)
		} else if channel.Type == "smtp" {
			unknownProps := []string{}

			requiredProps := map[string]bool{
				"type":     true,
				"name":     true,
				"headers":  true,
				"host":     true,
				"port":     true,
				"username": true,
				"password": true,
				"from":     true,
				"misc":     true,
			}

			for k := range v {
				if _, ok := requiredProps[k]; !ok {
					unknownProps = append(unknownProps, k)
				}
			}

			for _, prop := range unknownProps {
				msgs = append(
					msgs,
					"notification-channels."+slug+": "+prop+
						" is an unknown property for an SMTP notification channel",
				)
			}

			hostAny, ok := v["host"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": host is required")
			}

			host, ok := hostAny.(string)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": host is invalid")
			} else if host == "" {
				msgs = append(msgs, "notification-channels."+slug+": host is required")
			}

			portAny, ok := v["port"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": port is required")
			}

			port, ok := portAny.(int)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": port is invalid")
			} else if port == 0 {
				msgs = append(msgs, "notification-channels."+slug+": port is required")
			}

			usernameAny, ok := v["username"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": username is required")
			}

			username, ok := usernameAny.(string)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": username is invalid")
			} else if username == "" {
				msgs = append(msgs, "notification-channels."+slug+": username is required")
			}

			passwordAny, ok := v["password"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": password is required")
			}

			password, ok := passwordAny.(string)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": password is invalid")
			} else if password == "" {
				msgs = append(msgs, "notification-channels."+slug+": password is required")
			}

			fromAny, ok := v["from"]
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": from is required")
			}

			from, ok := fromAny.(string)
			if !ok {
				msgs = append(msgs, "notification-channels."+slug+": from is invalid")
			} else if from == "" {
				msgs = append(msgs, "notification-channels."+slug+": from is required")
			}
			if _, err := mail.ParseAddress(from); err != nil {
				msgs = append(msgs, "notification-channels."+slug+": from is an invalid email address")
			}

			headers := map[string]string{}

			headersAny, ok := v["headers"]
			if ok {
				headersMapAny, ok := headersAny.(map[string]any)
				if !ok {
					msgs = append(msgs, "notification-channels."+slug+": headers is invalid")
				}

				for k, v := range headersMapAny {
					if vs, ok := v.(string); ok {
						headers[k] = vs
						continue
					}
					if vs, ok := v.(int); ok {
						headers[k] = strconv.Itoa(vs)
						continue
					}
					msgs = append(
						msgs,
						"notification-channels."+slug+": invalid header value "+k+
							", must be string or number",
					)
				}
			}

			misc := map[string]string{}

			miscAny, ok := v["misc"]
			if ok {
				miscMapAny, ok := miscAny.(map[string]any)
				if !ok {
					msgs = append(msgs, "notification-channels."+slug+": misc is invalid")
				}

				for k, v := range miscMapAny {
					if vs, ok := v.(string); ok {
						misc[k] = vs
						continue
					}
					if vs, ok := v.(int); ok {
						misc[k] = strconv.Itoa(vs)
						continue
					}
					msgs = append(
						msgs,
						"notification-channels."+slug+": invalid misc value "+k+
							", must be string or number",
					)
				}
			}

			d := StatusnookConfigSMTPNotificationChannel{
				Host:     host,
				Port:     port,
				Username: username,
				Password: password,
				From:     from,
				Headers:  headers,
				Misc:     misc,
			}
			detailBytes, err := json.Marshal(d)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.MarshalSlackNotificationDetails: %w", err)
			}

			details = string(detailBytes)
		}

		if _, ok := existingNotificationChannelSlugs[slug]; ok {
			err := editNotificationChannel(
				tx,
				NotificationChannel{
					ID:      existingNotificationChannelSlugs[slug],
					Name:    channel.Name,
					Type:    channel.Type,
					Details: details,
				},
			)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.editNotificationChannel: %w", err)
			}
		} else {
			err := createNotification(tx, slug, channel.Name, channel.Type, details)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.createNotification: %w", err)
			}
		}
	}

	existingMailGroupSlugs := map[string]int{}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listMailGroups2: %w", err)
	}

	for _, v := range mailGroups {
		if _, ok := cfg.MailGroups[v.Slug]; !ok {
			err := deleteMailGroupByID(tx, v.ID)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.deleteMailGroupByID: %w", err)
			}
			continue
		}
		existingMailGroupSlugs[v.Slug] = v.ID
	}

	for slug, v := range cfg.MailGroups {
		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("mail-groups", slug)
			continue
		}

		if v.Name == "" {
			msgs = append(msgs, "mail-groups."+slug+": name is required")
		}

		uniqueMembers := map[string]bool{}

		for _, v := range v.Members {
			email, err := mail.ParseAddress(v)
			if err != nil {
				msgs = append(msgs, "mail-groups."+slug+": email is invalid \""+v+"\"")
				continue
			}

			if _, ok := uniqueMembers[strings.ToLower(email.String())]; ok {
				msgs = append(msgs, "mail-groups."+slug+": member is duplicated \""+v+"\"")
			}

			uniqueMembers[strings.ToLower(email.String())] = true
		}

		if _, ok := existingMailGroupSlugs[slug]; ok {
			err := updateMailGroup(tx, existingMailGroupSlugs[slug], v.Name, v.Description)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.updateMailGroup: %w", err)
			}
			err = updateMailGroupMembers(tx, existingMailGroupSlugs[slug], v.Members)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.updateMailGroupMembersUpdate: %w", err)
			}
		} else {
			id, err := createMailGroup(tx, slug, v.Name, v.Description)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.createMailGroup: %w", err)
			}

			err = updateMailGroupMembers(tx, id, v.Members)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.updateMailGroupMembersCreate: %w", err)
			}
		}
	}

	monitors, err = listMonitors(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listMonitors2: %w", err)
	}

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listNotificationChannels2: %w", err)
	}

	channelSlugs := map[string]int{}
	for _, v := range channels {
		channelSlugs[v.Slug] = v.ID
	}

	mailGroups, err = listMailGroups(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listMailGroups3: %w", err)
	}

	mailGroupSlugs := map[string]int{}
	for _, v := range mailGroups {
		mailGroupSlugs[v.Slug] = v.ID
	}

	for _, v := range monitors {
		if _, ok := cfg.Monitors[v.Slug]; !ok {
			err := deleteMonitorByID(tx, v.ID)
			if err != nil {
				return msgs, fmt.Errorf("applyConfig.deleteMonitorByID: %w", err)
			}
			continue
		}

		existingMonitorSlugs[v.Slug] = v.ID
	}

	for slug, monitor := range cfg.Monitors {
		if slug == "" || slugPattern.MatchString(slug) {
			invalidSlugMsg("monitors", slug)
			continue
		}

		skipUpdate := false

		channelIDs := []int{}
		uniqueNotificationChannels := map[string]bool{}
		for _, channelSlug := range monitor.NotificationChannels {
			if _, ok := channelSlugs[channelSlug]; !ok {
				msgs = append(
					msgs,
					"monitors."+slug+
						": notification-channels contains unknown channel \""+channelSlug+"\"",
				)
				skipUpdate = true
				continue
			}

			if _, ok := uniqueNotificationChannels[channelSlug]; ok {
				msgs = append(
					msgs,
					"monitors."+slug+
						": notification channel is duplicated \""+channelSlug+"\"",
				)
				skipUpdate = true
				continue
			}

			uniqueNotificationChannels[channelSlug] = true
			channelIDs = append(channelIDs, channelSlugs[channelSlug])
		}

		mailGroupIDs := []int{}
		uniqueMailGroups := map[string]bool{}
		for _, groupSlug := range monitor.MailGroups {
			if _, ok := mailGroupSlugs[groupSlug]; !ok {
				msgs = append(
					msgs,
					"monitors."+slug+
						": mail-groups contains unknown mail group \""+groupSlug+"\"",
				)
				skipUpdate = true
				continue
			}

			if _, ok := uniqueMailGroups[groupSlug]; ok {
				msgs = append(
					msgs,
					"monitors."+slug+
						": mail group is duplicated \""+groupSlug+"\"",
				)
				skipUpdate = true
				continue
			}

			uniqueMailGroups[groupSlug] = true
			mailGroupIDs = append(mailGroupIDs, mailGroupSlugs[groupSlug])
		}

		if skipUpdate {
			continue
		}

		err = updateMonitorNotificationChannels(
			tx,
			existingMonitorSlugs[slug],
			channelIDs,
		)
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.updateMonitorNotificationChannels: %w", err)
		}

		err = updateMonitorMailGroups(
			tx,
			existingMonitorSlugs[slug],
			mailGroupIDs,
		)
		if err != nil {
			return msgs, fmt.Errorf("applyConfig.updateMonitorMailGroups: %w", err)
		}
	}

	managedSubscriptions := cfg.AlertNotificationSettings.ManagedSubscriptions

	err = updateAlertSettings(
		tx,
		cfg.AlertNotificationSettings.SlackInstallURL,
		cfg.AlertNotificationSettings.SlackClientSecret,
		managedSubscriptions,
	)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.updateAlertSettings: %w", err)
	}

	if cfg.AlertNotificationSettings.EmailNotificationChannel != "" {
		if _, ok := channelSlugs[cfg.AlertNotificationSettings.EmailNotificationChannel]; !ok {
			msgs = append(
				msgs,
				"alert-notification-settings.email-notification-channel: "+
					"refers to unknown channel \""+
					cfg.AlertNotificationSettings.EmailNotificationChannel+"\"",
			)
		}
	}

	channel, err := getNotificationChannelBySlug(
		tx,
		cfg.AlertNotificationSettings.EmailNotificationChannel,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return msgs, fmt.Errorf("applyConfig.getNotificationChannelBySlug: %w", err)
	}

	err = updateAlertSMTPNotificationSetting(tx, channel.ID)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.updateAlertSMTPNotificationSetting: %w", err)
	}

	uniqueMessages := map[string]bool{}

	finalMsgs := []string{}
	for _, v := range msgs {
		if _, ok := uniqueMessages[v]; ok {
			continue
		}
		uniqueMessages[v] = true
		finalMsgs = append(finalMsgs, v)
	}

	err = updateMetaValue(tx, "configFile", string(cfgBytes))
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.updateMetaValueConfigFile: %w", err)
	}

	return finalMsgs, nil
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

func generateConfig(tx *sql.Tx) (string, error) {
	cfgStr := ""

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.listMailGroups: %w", err)
	}

	mailGroupMembers := map[int][]string{}

	for _, v := range mailGroups {
		members, err := listMailGroupMembersByID(tx, v.ID)
		if err != nil {
			return cfgStr, fmt.Errorf("generateConfig.listMailGroupMembersByID: %w", err)
		}
		for _, m := range members {
			mailGroupMembers[v.ID] = append(mailGroupMembers[v.ID], m.EmailAddress)
		}
	}

	cfgMailGroups := map[string]StatusnookConfigMailGroup{}
	for _, v := range mailGroups {
		cfgMailGroups[v.Slug] = StatusnookConfigMailGroup{
			Name:        v.Name,
			Members:     mailGroupMembers[v.ID],
			Description: v.Description,
		}
	}

	notificationChannels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.listNotificationChannels: %w", err)
	}

	cfgNotificationChannels := map[string]map[string]any{}
	for _, v := range notificationChannels {
		if v.Type == "smtp" {
			details, ok := v.Details.(SMTPNotificationDetails)
			if !ok {
				return cfgStr, fmt.Errorf("generateConfig.AssertSMTPNotificationDetails")
			}

			cfgNotificationChannel := map[string]any{
				"type":     v.Type,
				"name":     v.Name,
				"host":     details.Host,
				"port":     details.Port,
				"username": details.Username,
				"password": details.Password,
				"from":     details.From,
			}
			if len(details.Headers) > 0 {
				cfgNotificationChannel["headers"] = details.Headers
			}
			if len(details.Misc) > 0 {
				cfgNotificationChannel["misc"] = details.Misc
			}

			cfgNotificationChannels[v.Slug] = cfgNotificationChannel
		} else if v.Type == "slack" {
			details, ok := v.Details.(SlackNotificationDetails)
			if !ok {
				return cfgStr, fmt.Errorf("generateConfig.AssertSlackNotificationDetails")
			}

			cfgNotificationChannels[v.Slug] = map[string]any{
				"type":        v.Type,
				"name":        v.Name,
				"webhook-url": details.WebhookURL,
			}
		}
	}

	monitors, err := listMonitors(tx)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.listMonitors: %w", err)
	}

	cfgMonitors := map[string]StatusnookConfigMonitor{}
	for _, v := range monitors {
		channels, err := listNotificationChannelsByMonitorID(tx, v.ID)
		if err != nil {
			return cfgStr, fmt.Errorf("generateConfig.listNotificationChannelsByMonitorID: %w", err)
		}

		cfgNotificationChannels := []string{}
		for _, c := range channels {
			cfgNotificationChannels = append(cfgNotificationChannels, c.Slug)
		}

		mailGroups, err := listMailGroupIDsByMonitorID(tx, v.ID)
		if err != nil {
			return cfgStr, fmt.Errorf("generateConfig.listMailGroupIDsByMonitorID: %w", err)
		}

		cfgMailGroups := []string{}
		for _, m := range mailGroups {
			cfgMailGroups = append(cfgMailGroups, m.Slug)
		}

		cfgMonitor := StatusnookConfigMonitor{
			Name:                 v.Name,
			URL:                  v.URL,
			Method:               v.Method,
			Frequency:            v.Frequency,
			Timeout:              v.Timeout,
			Attempts:             v.Attempts,
			RequestHeaders:       v.RequestHeaders,
			NotificationChannels: cfgNotificationChannels,
			MailGroups:           cfgMailGroups,
		}
		if v.Body.String != "" {
			if v.BodyFormat.String == "form" {
				values, err := url.ParseQuery(v.Body.String)
				if err != nil {
					return cfgStr, fmt.Errorf("generateConfig.ParseQuery: %w", err)
				}

				flatValues := map[string]string{}
				for k, v := range values {
					flatValues[k] = v[0]
				}

				cfgMonitor.RequestBody = flatValues

			} else {
				cfgMonitor.RequestBody = v.Body.String
			}
		}

		cfgMonitors[v.Slug] = cfgMonitor
	}

	services, err := listServices(tx)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.listServices: %w", err)
	}

	cfgServices := map[string]StatusnookConfigService{}
	for _, v := range services {
		cfgServices[v.Slug] = StatusnookConfigService{Name: v.Name, Description: v.HelperText}
	}

	smtpNotificationChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cfgStr, fmt.Errorf("generateConfig.getAlertSMTPNotificationSetting: %w", err)
	}

	var smtpNotificationChannel NotificationChannel
	if smtpNotificationChannelID != 0 {
		smtpNotificationChannel, err = getNotificationChannelByID(tx, smtpNotificationChannelID)
		if err != nil {
			return cfgStr, fmt.Errorf("generateConfig.getNotificationChannelByID: %w", err)
		}
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.getAlertSettings: %w", err)
	}

	cfg := StatusnookConfig{
		MailGroups:           cfgMailGroups,
		NotificationChannels: cfgNotificationChannels,
		Monitors:             cfgMonitors,
		Services:             cfgServices,
		AlertNotificationSettings: StatusnookConfigAlertNotificationSettings{
			EmailNotificationChannel: smtpNotificationChannel.Slug,
			ManagedSubscriptions:     alertSettings.ManagedSubscriptions,
			SlackClientSecret:        alertSettings.SlackClientSecret,
			SlackInstallURL:          alertSettings.SlackInstallURL,
		},
		GeneralSettings: StatusnookConfigGeneralSettings{Name: metaName.Load()},
	}

	cfgBytes, err := yaml.Marshal(cfg)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.Marshal: %w", err)
	}

	cfgStr = string(cfgBytes)

	return cfgStr, nil
}

func main() {
	portFlag := flag.Int("port", 80, "")
	selfSignedFlag := flag.Bool("generate-self-signed-cert", false, "")

	flag.Parse()

	if *validateConfigFlag != "" {
		if err := validateConfig(*validateConfigFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("config ok")
		return
	}

	if *selfSignedFlag {
		GenerateSelfSignedCertificate()
		return
	}

	db = initDB(false)
	// Unbounded by default, so a burst opened an unbounded number of SQLite
	// connections, each re-running the DSN pragmas. Writes are already
	// serialized on rwDB below.
	db.SetMaxOpenConns(max(4, runtime.NumCPU()*4))
	db.SetMaxIdleConns(4)

	rwDB = initDB(true)
	rwDB.SetMaxOpenConns(1)

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("main.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	setup, err := getMetaValue(tx, "setup")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueSetup: %s", err)
		return
	}
	metaSetup.Store(setup)

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueName: %s", err)
		return
	}
	metaName.Store(name)

	domain, err := getMetaValue(tx, "domain")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueDomain: %s", err)
		return
	}
	metaDomain.Store(domain)

	unconfirmedDomain, err := getMetaValue(tx, "unconfirmedDomain")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueUnconfirmedDomain: %s", err)
		return
	}
	metaUnconfirmedDomain.Store(unconfirmedDomain)

	unconfirmedDomainProblem, err := getMetaValue(tx, "unconfirmedDomainProblem")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueUnconfirmedDomainProblem: %s", err)
		return
	}
	metaUnconfirmedDomainProblem.Store(unconfirmedDomainProblem)

	configFileEnabled, err := getMetaValue(tx, "configFileEnabled")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueConfigFileEnabled: %s", err)
		return
	}
	if configFileEnabled == "true" {
		metaConfigFileEnabled.Store(true)
	}

	ssl, err := getMetaValue(tx, "ssl")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueSSL: %s", err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		metaSSL.Store("true")
		if BUILD == "dev" || *portFlag != 80 {
			metaSSL.Store("false")
		}

		err = updateMetaValue(tx, "ssl", metaSSL.Load())
		if err != nil {
			log.Printf("main.updateMetaValueSSL: %s", err)
			return
		}
	} else {
		metaSSL.Store(ssl)
	}

	_, err = getMetaValue(tx, "secretKey")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Fatalf("main.getMetaValueSecretKey: %s", err)
			return
		}

		keyBytes := make([]byte, 32)
		_, err = rand.Read(keyBytes)
		if err != nil {
			log.Printf("main.Read: %s", err)
			return
		}

		keyB64 := base64.StdEncoding.EncodeToString(keyBytes)

		err = updateMetaValue(tx, "secretKey", keyB64)
		if err != nil {
			log.Printf("main.updateMetaValueSecretKey: %s", err)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Fatalf("main.Commit: %s", err)
		return
	}

	r := chi.NewRouter()
	if BUILD == "dev" {
		r.Use(middleware.Logger)
	}

	// No CSP: the pages carry inline <script> blocks that would need nonces
	// first. The rest cost nothing. X-Frame-Options matters more than usual
	// here -- the CSRF token is injected by the page's own JavaScript, so a
	// framed admin's click carries a valid token.
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.ServeHTTP(w, r)
		})
	})

	if BUILD == "release" && metaSSL.Load() == "true" {
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if certmagic.DefaultACME.HandleHTTPChallenge(w, r) {
					return
				}

				if r.TLS == nil {
					toURL := "https://"

					requestHost, _, err := net.SplitHostPort(r.Host)
					if err != nil {
						requestHost = r.Host
					}

					toURL += requestHost
					toURL += r.URL.RequestURI()

					w.Header().Set("Connection", "close")

					http.Redirect(w, r, toURL, http.StatusFound)

					return
				}
				h.ServeHTTP(w, r)
			})
		})
	}
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /healthz is exempt so a probe still answers on an instance
			// nobody has finished setting up.
			if metaSetup.Load() != "done" &&
				!strings.HasPrefix(r.URL.Path, "/setup") &&
				!strings.HasPrefix(r.URL.Path, "/static") &&
				r.URL.Path != "/healthz" {
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Neither static assets nor the health probe have a session to
			// look up, and this middleware opens a transaction for every
			// request that reaches it -- every image and font included.
			if strings.HasPrefix(r.URL.Path, "/static") || r.URL.Path == "/healthz" {
				h.ServeHTTP(w, r)
				return
			}

			tx, err := db.Begin()
			if err != nil {
				log.Printf("adminMiddleware.Begin: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			sessionToken, err := r.Cookie("session")
			if err != nil {
				tx.Rollback()
				h.ServeHTTP(w, r)
				return
			}

			id, csrfToken, err := validateSession(tx, sessionToken.Value)
			if err != nil {
				tx.Rollback()
				if !errors.Is(err, sql.ErrNoRows) {
					log.Printf("adminMiddleware.validateSession: %s", err)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				h.ServeHTTP(w, r)
				return
			}

			if err = tx.Commit(); err != nil {
				log.Printf("adminMiddleware.Commit: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			ctx := context.WithValue(
				r.Context(),
				authCtxKey{},
				authCtx{
					ID:        id,
					CSRFToken: csrfToken,
				},
			)

			h.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	fs := http.FileServer(http.FS(staticFS))
	r.Get("/static/*", neuter(fs).ServeHTTP)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			log.Printf("healthz.Ping: %s", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("database unreachable"))
			return
		}
		w.Write([]byte("ok"))
	})
	r.Route("/", func(r chi.Router) {
		r.Use(statusMiddleware)
		r.Get("/", index)
		r.Get("/resolve", getResolve)
		r.Get("/cross-auth", getCrossAuth)
		r.Get("/history", history)
		r.Get("/unsubscribe", getUnsubscribe)
		r.Post("/unsubscribe", postUnsubscribe)
		r.Post("/resubscribe", postResubscribe)
		r.Get("/invitation/{token}", getInvitation)
		r.Post("/invitation/{token}", postInvitation)
		r.Post("/github-config-webhook", configWebhook)
	})
	r.Route("/admin", func(r chi.Router) {
		r.Use(csrfMiddleware)
		r.Use(statusMiddleware)
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := getAuthCtx(r)

				if ctx.ID == 0 {
					http.Redirect(w, r, "/login", http.StatusFound)
					return
				}

				h.ServeHTTP(w, r)
			})
		})
		r.Get("/", adminIndex)
		r.Post("/resolve", postResolve)
		r.Route("/alerts", func(r chi.Router) {
			r.Get("/", alerts)
			r.Route("/notifications", func(r chi.Router) {
				r.Get("/", getAlertNotifications)
				r.Post("/", postAlertNotifications)
			})
			r.Get("/{id}", getAlert)
			r.Delete("/{id}", deleteAlert)
			r.Get("/create", getCreateAlert)
			r.Post("/create", postCreateAlert)
			r.Get("/{id}/edit", getEditAlert)
			r.Post("/{id}/edit", postEditAlert)
			r.Get("/{id}/messages", getAddAlertMessage)
			r.Post("/{id}/messages", postAddAlertMessage)
			r.Post("/{id}/resolve", postResolveAlert)
			r.Post("/{id}/unresolve", postUnresolveAlert)
			r.Delete("/{id}/messages/{messageID}", deleteAlertMessage)
			r.Get("/{id}/messages/{messageID}", getEditAlertMessage)
			r.Post("/{id}/messages/{messageID}", postEditAlertMessage)
		})
		r.Route("/monitors", func(r chi.Router) {
			r.Get("/", monitors)
			r.Get("/{id}", getMonitor)
			r.Get("/{id}/all", getMonitorAllLogs)
			r.Get("/{id}/poll", getMonitorPoll)
			r.Delete("/{id}", deleteMonitor)
			r.Get("/create", getCreateMonitor)
			r.Post("/create", postCreateMonitor)
			r.Get("/{id}/edit", getEditMonitor)
			r.Post("/{id}/edit", postEditMonitor)
			r.Get("/{id}/view", getDetailsMonitor)
		})
		r.Route("/services", func(r chi.Router) {
			r.Get("/", services)
			r.Get("/create", getCreateService)
			r.Post("/create", postCreateService)
			r.Delete("/{id}", deleteService)
			r.Get("/{id}/edit", getEditService)
			r.Post("/{id}/edit", postEditService)
		})
		r.Route("/notifications", func(r chi.Router) {
			r.Get("/", notifications)
			r.Get("/create", getCreateNotification)
			r.Post("/create", postCreateNotification)
			r.Delete("/{id}", deleteNotificationChannel)
			r.Get("/{id}/edit", getEditNotification)
			r.Post("/{id}/edit", postEditNotification)
			r.Get("/{id}/view", getViewNotification)
			r.Route("/mail-groups", func(r chi.Router) {
				r.Get("/create", getCreateMailGroup)
				r.Post("/create", postCreateMailGroup)
				r.Get("/{id}/edit", getEditMailGroup)
				r.Post("/{id}/edit", postEditMailGroup)
				r.Get("/{id}/view", getViewMailGroup)
				r.Delete("/{id}", deleteMailGroup)
			})
		})
		r.Route("/update", func(r chi.Router) {
			r.Get("/", update)
			r.Get("/check", updateCheck)
			r.Get("/after-update", afterUpdate)
			r.Post("/", postUpdate)
		})
		r.Route("/settings", func(r chi.Router) {
			r.Get("/", getSettings)
			r.Post("/", postSettings)
			r.Post("/cancel-domain", postSettingsCancelDomain)
			r.Get("/users/{id}/edit", getEditUser)
			r.Post("/users/{id}/edit", postEditUser)
			r.Delete("/users/{id}", deleteUser)
			r.Post("/users/invite", postInviteUser)
			r.Delete("/users/invite/{id}", postDeleteInvite)
			r.Post("/config", postConfig)
			r.Route("/config-settings", func(r chi.Router) {
				r.Get("/", getConfigSettings)
				r.Post("/", postConfigSettings)
				r.Post("/generate-webhook-secret", postGenerateWebhookSecret)
			})
			r.Post("/secrets", postSecret)
		})
	})
	r.Route("/login", func(r chi.Router) {
		r.Get("/", getLogin)
		r.Post("/", postLogin)
	})
	r.Route("/logout", func(r chi.Router) {
		r.Use(csrfMiddleware)
		r.Post("/", logout)
	})
	r.Route("/setup", func(r chi.Router) {
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tx, err := db.Begin()
				if err != nil {
					log.Printf("Setup.Begin: %s", err)
					return
				}
				defer tx.Rollback()

				v, err := getMetaValue(tx, "setup")
				if err != nil {
					log.Printf("Setup.getMetaValue: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("Setup.Commit: %s", err)
					return
				}

				if v == "done" {
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}

				if r.URL.Path == "/setup/statusnook" {
					h.ServeHTTP(w, r)
					return
				}

				if v == "domain" && r.URL.Path == "/setup/skip-domain" {
					h.ServeHTTP(w, r)
					return
				}

				endpoints := map[string]string{
					"domain":  "/setup/domain",
					"account": "/setup/account",
					"name":    "/setup/name",
					"done":    "/",
				}

				url, ok := endpoints[v]
				if !ok {
					log.Printf("Setup.endpoints: no endpoint")
					return
				}

				if r.URL.Path == "/setup" || r.URL.Path != url {
					if r.Method == http.MethodGet {
						http.Redirect(w, r, url, http.StatusFound)
					} else {
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}

				h.ServeHTTP(w, r)
			})
		})
		r.Post("/statusnook", postSetupStatusnook)
		r.Options("/statusnook", postSetupStatusnook)
		r.Get("/domain", getSetupDomain)
		r.Post("/domain", postSetupDomain)
		r.Post("/skip-domain", postSetupDomainSkip)
		r.Get("/account", getSetupAccount)
		r.Post("/account", postSetupAccount)
		r.Get("/name", getSetupName)
		r.Post("/name", postSetupName)
	})
	r.Get("/callback/slack", slackOAuth2Callback)
	r.Post("/subscribe/email", postSubscribeEmail)
	r.Get("/subscribe/email/confirm", getSubscribeEmailConfirm)
	r.Post("/subscribe/email/confirm", postSubscribeEmailConfirm)
	appCtx, cancelAppCtx = context.WithCancel(context.Background())

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	appWg.Add(1)
	go monitorLoop(appCtx, &appWg)

	appWg.Add(1)
	go notificationLoop(appCtx, &appWg)

	appWg.Add(1)
	go retentionLoop(appCtx, &appWg)

	var httpServer *http.Server
	var httpsServer *http.Server

	if BUILD == "dev" {
		httpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", 8000))
		if err != nil {
			log.Fatalf("main.ListenHTTPS: %s", err)
		}

		// Same timeouts as the release path below; the dev server had none.
		httpServer = &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      2 * time.Minute,
			IdleTimeout:       5 * time.Minute,
			Handler:           r,
			BaseContext:       func(listener net.Listener) context.Context { return appCtx },
		}

		go httpServer.Serve(httpLn)
	} else {
		host := ""
		if !*dockerFlag && *portFlag != 80 {
			host = "127.0.0.1"
		}

		httpLn, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, *portFlag))
		if err != nil {
			log.Fatalf("main.ListenHTTP: %s", err)
		}

		httpServer = &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      2 * time.Minute,
			IdleTimeout:       5 * time.Minute,
			Handler:           r,
			BaseContext:       func(listener net.Listener) context.Context { return appCtx },
		}

		go httpServer.Serve(httpLn)

		if metaSSL.Load() == "true" {
			certmagic.Default.Storage = &certmagic.FileStorage{Path: "certmagic"}
			certmagic.DefaultACME.Agreed = true
			certmagic.DefaultACME.CA = CA
			certmagic.DefaultACME.Email = " "

			domains := []string{}
			if domain != "" {
				domains = append(domains, metaDomain.Load())
			}

			tlsConfig, err := certmagic.TLS(domains)
			if err != nil {
				log.Fatalf("main.TLS: %s", err)
			}
			tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)
			getCertificateCertMagic := tlsConfig.GetCertificate
			tlsConfig.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				certificate, err := getCertificateCertMagic(clientHello)
				if err != nil {
					certificate, err := tls.LoadX509KeyPair(
						SELF_SIGNED_CERT_NAME,
						SELF_SIGNED_KEY_NAME,
					)
					if err != nil {
						log.Printf("main.LoadX509KeyPair: %s", err)
						return &certificate, err
					}

					return &certificate, nil
				}

				return certificate, nil
			}

			httpsLn, err := tls.Listen("tcp", fmt.Sprintf(":%d", 443), tlsConfig)
			if err != nil {
				log.Fatalf("main.ListenHTTPS: %s", err)
			}

			httpsServer = &http.Server{
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      2 * time.Minute,
				IdleTimeout:       5 * time.Minute,
				Handler:           r,
				BaseContext:       func(listener net.Listener) context.Context { return appCtx },
			}

			go httpsServer.Serve(httpsLn)

			if unconfirmedDomain != "" && unconfirmedDomainProblem == "" {
				appWg.Add(1)
				go monitorUnconfirmedDomainLoop(appCtx, &appWg)
			}
		}
	}

	<-shutdownCh

	// Shutdown BEFORE waiting on the loops: it stops accepting new requests
	// and drains the ones in flight, where the old order kept the listeners
	// open while the background loops were already winding down.
	//
	// Bounded, because Shutdown with a background context waits forever on a
	// single hung request and Docker or systemd then SIGKILLs instead.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("main.ShutdownHTTP: %s", err)
	}

	if httpsServer != nil {
		if err := httpsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("main.ShutdownHTTPS: %s", err)
		}
	}

	cancelAppCtx()
	appWg.Wait()

	err = db.Close()
	if err != nil {
		log.Printf("main.DBClose: %s", err)
	}

	err = rwDB.Close()
	if err != nil {
		log.Printf("main.rwDBClose: %s", err)
	}
}

type AlertSubscription struct {
	ID          int
	Type        string
	Destination string
	Meta        string
	Active      bool
}

func listActiveAlertEmailSubscriptions(tx *sql.Tx) ([]AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and active = true
	`

	var subs []AlertSubscription

	rows, err := tx.Query(query)
	if err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sub AlertSubscription

		err := rows.Scan(
			&sub.ID,
			&sub.Type,
			&sub.Destination,
			&sub.Meta,
			&sub.Active,
		)
		if err != nil {
			return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.Scan: %w", err)
		}

		subs = append(subs, sub)
	}

	if err := rows.Err(); err != nil {
		return subs, fmt.Errorf("listActiveAlertEmailSubscriptions.RowsErr: %w", err)
	}

	return subs, nil
}

func deleteAlertSubscriptionByMeta(tx *sql.Tx, meta string) error {
	const query = `
		delete from alert_subscription where meta = ?
	`

	_, err := tx.Exec(query, meta)
	if err != nil {
		return fmt.Errorf("deleteAlertSubscriptionByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByMeta(tx *sql.Tx, meta string, active bool) error {
	const query = `
		update alert_subscription set active = ? where meta = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, meta)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByMeta.Exec: %w", err)
	}

	return nil
}

func updateEmailAlertSubscriptionActiveByEmail(tx *sql.Tx, email string, active bool) error {
	const query = `
		update alert_subscription set active = ? where destination = ? and type = 'email'
	`

	_, err := tx.Exec(query, active, email)
	if err != nil {
		return fmt.Errorf("updateEmailAlertSubscriptionActiveByEmail.Exec: %w", err)
	}

	return nil
}

func createAlertSubscription(tx *sql.Tx, subscriptionType string, destination string, meta string) error {
	const query = `
		insert into alert_subscription(type, destination, meta) values(?, ?, nullif(?, ''))
		on conflict(type, destination) do update set active = true
	`

	_, err := tx.Exec(query, subscriptionType, destination, meta)
	if err != nil {
		return fmt.Errorf("createAlertSubscription.Exec: %w", err)
	}

	return nil
}

type SlackOAuthAccessResponse struct {
	OK   bool `json:"ok"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	IncomingWebhook struct {
		URL       string `json:"url"`
		ChannelID string `json:"channel_id"`
	} `json:"incoming_webhook"`
}

func slackOAuth2Callback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("slackOAuth2Callback.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	settings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("slackOAuth2Callback.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	slackInstallURL, err := url.ParseRequestURI(settings.SlackInstallURL)
	if err != nil {
		log.Printf("slackOAuth2Callback.ParseRequestURI: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	form := url.Values{}
	form.Add("code", code)
	form.Add("client_id", slackInstallURL.Query().Get("client_id"))
	form.Add("client_secret", settings.SlackClientSecret)

	resp, err := http.PostForm("https://slack.com/api/oauth.v2.access", form)
	if err != nil {
		log.Printf("slackOAuth2Callback.PostForm: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("slackOAuth2Callback.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	accessResponse := SlackOAuthAccessResponse{}

	err = json.Unmarshal(respBody, &accessResponse)
	if err != nil {
		log.Printf("slackOAuth2Callback.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !accessResponse.OK {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	meta := accessResponse.Team.ID + "_" + accessResponse.IncomingWebhook.ChannelID

	err = deleteAlertSubscriptionByMeta(tx, meta)
	if err != nil {
		log.Printf("slackOAuth2Callback.deleteAlertSubscriptionByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertSubscription(
		tx,
		"slack",
		accessResponse.IncomingWebhook.URL,
		meta,
	)
	if err != nil {
		log.Printf("slackOAuth2Callback.createAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("slackOAuth2Callback.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/?slack_app_installed=1", http.StatusFound)
}

func postmarkDeleteSuppression(email string, token string, stream string) error {
	// Marshalled, not Sprintf'd: email arrives from an unauthenticated form,
	// and mail.ParseAddress accepts quoted local parts containing escaped
	// quotes, which broke straight out of the string literal.
	bodyBytes, err := json.Marshal(struct {
		Suppressions []struct {
			EmailAddress string `json:"EmailAddress"`
		} `json:"Suppressions"`
	}{
		Suppressions: []struct {
			EmailAddress string `json:"EmailAddress"`
		}{{EmailAddress: email}},
	})
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.Marshal: %w", err)
	}
	body := string(bodyBytes)

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodPost,
		"https://api.postmarkapp.com/message-streams/"+stream+"/suppressions/delete",
		strings.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.NewRequest: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Postmark-Server-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.Do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("postmarkDeleteSuppression.ReadAll: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("postmarkDeleteSuppression.StatusCode: %s", string(respBody))
	}

	return nil
}

func checkHasRecentPendingEmailAlertSubscription(tx *sql.Tx, email string, now time.Time) (bool, error) {
	const query = `
		select exists(
			select 1 from pending_email_alert_subscription where email = ?
			and created_at > datetime(?, '-10 minutes')
		)
	`

	var hasRecent bool

	err := tx.QueryRow(query, email, now).Scan(&hasRecent)
	if err != nil {
		return hasRecent, fmt.Errorf("checkHasRecentPendingEmailAlertSubscription.Exec: %w", err)
	}

	return hasRecent, nil
}

func createPendingEmailAlertSubscription(tx *sql.Tx, token string, email string, createdAt time.Time) error {
	const query = `
		insert into pending_email_alert_subscription(token, email, created_at)
		values(?, ?, ?)
	`

	_, err := tx.Exec(query, token, email, createdAt)
	if err != nil {
		return fmt.Errorf("createPendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

type SupressionDumpResponse struct {
	Suppressions []Supression
}

type Supression struct {
	EmailAddress      string
	SuppressionReason string
	Origin            string
	CreatedAt         time.Time
}

func postmarkDumpSupressions(token string, stream string) (SupressionDumpResponse, error) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	var supressionsResp SupressionDumpResponse

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.postmarkapp.com/message-streams/"+stream+"/suppressions/dump"+
			"?SupressionReason=ManualSuppression",
		nil,
	)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.NewRequest: %w", err)
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("X-Postmark-Server-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.Do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.ReadAll: %w", err)
	}

	if resp.StatusCode != 200 {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.StatusCode: %s", string(body))
	}

	err = json.Unmarshal(body, &supressionsResp)
	if err != nil {
		return supressionsResp, fmt.Errorf("postmarkDumpSupressions.Unmarshal: %w", err)
	}

	return supressionsResp, nil
}

var lastSuppressionSync time.Time
var supressionSyncMu sync.Mutex

func postSubscribeEmail(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")

	_, err := mail.ParseAddress(email)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	supressionSyncMu.Lock()
	if time.Since(lastSuppressionSync) > time.Second*10 {
		tx, err := db.Begin()
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.BeginSuppressionsPrep: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		alertNotificationChannelID, err := getAlertSMTPNotificationSetting(tx)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getAlertSMTPNotificationSetting: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		notificationChannel, err := getNotificationChannelByID(tx, alertNotificationChannelID)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getNotificationChannelByID: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		smtpDetail, ok := notificationChannel.Details.(SMTPNotificationDetails)
		if !ok {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.ChannelAssert: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		alertSettings, err := getAlertSettings(tx)
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.getAlertSettings: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = tx.Commit()
		if err != nil {
			supressionSyncMu.Unlock()
			log.Printf("postSubscribeEmail.CommitBeginSuppressionsPrep: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if !alertSettings.ManagedSubscriptions &&
			strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
			supressions, err := postmarkDumpSupressions(smtpDetail.Password, smtpDetail.Misc["pm-broadcast"])
			if err != nil {
				supressionSyncMu.Unlock()
				log.Printf("postSubscribeEmail.postmarkDumpSupressions: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			for _, v := range supressions.Suppressions {
				tx, err := rwDB.Begin()
				if err != nil {
					supressionSyncMu.Unlock()
					log.Printf("postSubscribeEmail.BeginSuppressions: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				defer tx.Rollback()

				// v.EmailAddress, not email: this loop deactivates the
				// subscription of each SUPPRESSED address. Looking up the
				// requester instead meant one subscriber gated the whole dump --
				// either every suppressed address was deactivated or none was,
				// depending on whether the person subscribing already had a row.
				subscription, err := getAlertSubscriptionByEmail(tx, v.EmailAddress)
				if err != nil {
					if !errors.Is(err, sql.ErrNoRows) {
						supressionSyncMu.Unlock()
						log.Printf("postSubscribeEmail.getAlertSubscriptionByEmail: %s", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}

				if subscription.ID != 0 {
					err = updateEmailAlertSubscriptionActiveByEmail(tx, v.EmailAddress, false)
					if err != nil {
						supressionSyncMu.Unlock()
						log.Printf(
							"postSubscribeEmail.updateEmailAlertSubscriptionActiveByEmail: %s",
							err,
						)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
				}

				err = tx.Commit()
				if err != nil {
					supressionSyncMu.Unlock()
					log.Printf("postSubscribeEmail.CommitSuppressions: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
		}
		lastSuppressionSync = time.Now().UTC()
	}
	supressionSyncMu.Unlock()

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSubscribeEmail.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	sub, err := getAlertSubscriptionByEmail(tx, email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postSubscribeEmail.getAlertSubscriptionByEmail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if sub.Active {
		w.Write([]byte(`
		<dialog id="email-already-subscribed-modal" class="email-already-subscribed-modal success-modal" hx-swap-oob="true">
			<div>
				<div>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
						<path fill-rule="evenodd" d="M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z" clip-rule="evenodd" />
					</svg>
				</div>
				<span>
					This email address is already subscribed to receive updates
				</span>

				<button onclick="document.querySelector('.email-already-subscribed-modal').close();">Dismiss</button>
			</div>

			<script>
				document.querySelector('.email-updates-modal').close();
				document.querySelector('.email-already-subscribed-modal').showModal();
			</script>
		</dialog>
		`))
		return
	}


	hasRecentPendingSub, err := checkHasRecentPendingEmailAlertSubscription(tx, email, time.Now().UTC())
	if err != nil {
		log.Printf("postSubscribeEmail.checkHasRecentPendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if hasRecentPendingSub {
		w.Write([]byte(postSubscribeEmailMarkup))
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSubscribeEmail.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	token := base64.URLEncoding.EncodeToString(tokenBytes)

	err = createPendingEmailAlertSubscription(tx, token, email, time.Now().UTC())
	if err != nil {
		log.Printf("postSubscribeEmail.createPendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	notificationID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil {
		log.Printf("postSubscribeEmail.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	channel, err := getNotificationChannelByID(tx, notificationID)
	if err != nil {
		log.Printf("postSubscribeEmail.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if !ok {
		// err is nil here, so the old "%s" printed %!s(<nil>).
		log.Printf("postSubscribeEmail.NotificationDetailsAssert: channel %d is not SMTP", channel.ID)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Committed before the SMTP conversation, not after. rwDB is a single
	// connection opened IMMEDIATE, and net/smtp has no dial timeout, so
	// holding the transaction across a send to an unreachable host let one
	// unauthenticated request block every writer -- monitor logging included --
	// for the OS connect timeout. The pending row is all the confirm flow
	// needs; the send is best-effort and already logged when it fails.
	if err := tx.Commit(); err != nil {
		log.Printf("postSubscribeEmail.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	msg := [][]byte{
		[]byte("Subject: Confirm your subscription to " + headerValue(metaName.Load()) + " status alerts"),
		[]byte("To: " + headerValue(email)),
		[]byte("From: " + headerValue(metaName.Load()) + " " + "<" + smtpDetail.From + ">"),
		[]byte("Content-Type: text/html; charset=UTF-8"),
	}
	for k, v := range smtpDetail.Headers {
		if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") &&
			k == "X-PM-Message-Stream" {
			continue
		}
		msg = append(msg, []byte(headerValue(k)+": "+headerValue(v)))
	}
	if strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		msg = append(msg, []byte("X-PM-Message-Stream: "+smtpDetail.Misc["pm-transactional"]))
	}

	const emailTmpl = `Hi,<br><br>
	
To start receiving status alert emails from {{.Name}}, please <a href="{{.Link}}">confirm your subscription</a>.
<br><br>

If this email reached you by mistake, feel free to ignore it and we won't subscribe you.
`

	tmpl, err := parseEmailTmpl("alertConfirm", emailTmpl)
	if err != nil {
		log.Printf("postSubscribeEmail.parseEmailTmpls: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	protocol := "https"
	if BUILD == "dev" {
		protocol = "http"
	}

	emailBytes := bytes.Buffer{}

	err = tmpl.Execute(
		&emailBytes,
		struct {
			Name string
			Link string
		}{
			Name: metaName.Load(),
			Link: protocol + "://" + metaDomain.Load() + "/subscribe/email/confirm?token=" + token,
		},
	)
	if err != nil {
		log.Printf("postSubscribeEmail.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	emailStr := "\r\n" + emailBytes.String()

	msg = append(msg, []byte(emailStr))

	err = smtp.SendMail(
		smtpDetail.Host+":"+strconv.Itoa(smtpDetail.Port),
		PlainOrLoginAuth(
			smtpDetail.Username,
			smtpDetail.Password,
			smtpDetail.Host,
		),
		smtpDetail.From,
		[]string{email},
		bytes.Join(msg, []byte("\r\n")),
	)
	if err != nil {
		log.Printf("postSubscribeEmail.SendMail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write([]byte(postSubscribeEmailMarkup))
}

func updatePendingEmailAlertSubscription(tx *sql.Tx, confirmedAt time.Time, token string) error {
	const query = `
		update pending_email_alert_subscription set confirmed_at = ? where token = ?
	`

	_, err := tx.Exec(query, confirmedAt, token)
	if err != nil {
		return fmt.Errorf("updatePendingEmailAlertSubscription.Exec: %w", err)
	}

	return nil
}

func getAlertSubscriptionByEmail(tx *sql.Tx, email string) (AlertSubscription, error) {
	const query = `
		select id, type, destination, meta, active from alert_subscription
		where type = 'email' and destination = ?
	`

	var sub AlertSubscription
	err := tx.QueryRow(query, email).Scan(
		&sub.ID,
		&sub.Type,
		&sub.Destination,
		&sub.Meta,
		&sub.Active,
	)
	if err != nil {
		return sub, fmt.Errorf("getAlertSubscriptionByEmail.Scan: %w", err)
	}

	return sub, nil
}

// pendingSubscriptionLifetime bounds how long a confirmation link works.
// Without it the token was replayable forever, and the row it belongs to was
// never deleted either.
const pendingSubscriptionLifetime = 24 * time.Hour

// userInvitationLifetime matches the 24h the invitation handlers already
// enforce when validating a token; expired rows were simply never removed.
const userInvitationLifetime = 24 * time.Hour

func getPendingEmailAlertSubscriptionEmailByToken(tx *sql.Tx, token string) (string, error) {
	const query = `
		select email from pending_email_alert_subscription
		where token = ? and confirmed_at is null and created_at > ?
	`

	var email string

	err := tx.QueryRow(query, token, time.Now().UTC().Add(-pendingSubscriptionLifetime)).
		Scan(&email)
	if err != nil {
		return email, fmt.Errorf("getPendingEmailAlertSubscriptionEmailByToken.Scan: %w", err)
	}

	return email, nil
}

func getSubscribeEmailConfirm(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("getSubscribeEmailConfirm", getSubscribeEmailConfirmMarkup)
	if err != nil {
		log.Printf("getSubscribeEmailConfirm.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getSubscribeEmailConfirm.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSubscribeEmailConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notificationChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpNotificationChannel, err := getNotificationChannelByID(tx, notificationChannelID)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	email, err := getPendingEmailAlertSubscriptionEmailByToken(tx, token)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getPendingEmailAlertSubscriptionEmailByToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	sub, err := getAlertSubscriptionByEmail(tx, email)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postSubscribeEmailConfirm.getAlertSubscriptionByEmail: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if sub.Active {
		return
	}

	smtpDetail, ok := smtpNotificationChannel.Details.(SMTPNotificationDetails)
	if !ok {
		log.Printf("postSubscribeEmailConfirm.Details: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.CommitRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !alertSettings.ManagedSubscriptions &&
		strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com") {
		err = postmarkDeleteSuppression(email, smtpDetail.Password, smtpDetail.Misc["pm-broadcast"])
		if err != nil {
			log.Printf("postSubscribeEmailConfirm.postmarkDeleteSuppression: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	tx, err = rwDB.Begin()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.BeginUpdate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updatePendingEmailAlertSubscription(tx, time.Now().UTC(), token)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.updatePendingEmailAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertSubscription(
		tx,
		"email",
		email,
		base64.URLEncoding.EncodeToString(tokenBytes),
	)
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.createAlertSubscription: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSubscribeEmailConfirm.CommitUpdate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/?email_subscribed=1", http.StatusFound)
}

func getUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}


	tmpl, err := parseTmpl("unsubscribe", getUnsubscribeMarkup)
	if err != nil {
		log.Printf("getUnsubscribeEmail.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName.Load(),
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getUnsubscribeEmail.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postUnsubscribe.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateEmailAlertSubscriptionActiveByMeta(tx, token, false)
	if err != nil {
		log.Printf("postUnsubscribe.updateEmailAlertSubscriptionActiveByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postUnsubscribe.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("postUnsubscribe", postUnsubscribeMarkup)
	if err != nil {
		log.Printf("postUnsubscribe.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName.Load(),
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("postUnsubscribe.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postResubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postResubscribe.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateEmailAlertSubscriptionActiveByMeta(tx, token, true)
	if err != nil {
		log.Printf("postResubscribe.updateEmailAlertSubscriptionActiveByMeta: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postResubscribe.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("postResubscribe", postResubscribeMarkup)
	if err != nil {
		log.Printf("postResubscribe.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Name  string
			Token string
			Ctx   pageCtx
		}{
			Name:  metaName.Load(),
			Token: token,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("postResubscribe.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getInvitation(w http.ResponseWriter, r *http.Request) {
	inviteToken := chi.URLParam(r, "token")
	if inviteToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getInvitation.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = validateUserInvitationToken(tx, inviteToken, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.Printf("getInvitation.validateUserInvitationToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getInvitation.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getInvitation", getInvitationMarkup)
	if err != nil {
		log.Printf("getInvitation.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getInvitation.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postInvitation(w http.ResponseWriter, r *http.Request) {
	inviteToken := chi.URLParam(r, "token")
	if inviteToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postInvitation.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	id, err := validateUserInvitationToken(tx, inviteToken, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		log.Printf("postInvitation.validateUserInvitationToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = deleteUserInvitation(tx, id)
	if err != nil {
		log.Printf("postInvitation.deleteUserInvitation: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Username is required
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	passwordConfirmation := r.PostFormValue("password-confirmation")

	if password != passwordConfirmation {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Passwords do not match
			</div>
		`))
		return
	}

	if len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Password must contain at least 8 characters
			</div>
		`))
		return
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("postInvitation.GenerateFromPassword: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	userID, err := createUser(tx, username, string(pwHash))
	if err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) {
			if errors.Is(sqliteErr.Code, sqlite3.ErrConstraint) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`
					<div id="alert" class="alert" hx-swap-oob="true">
						This username is already taken
					</div>
				`))
				return
			}
		}
		log.Printf("postInvitation.createUser: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postInvitation.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postInvitation.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	err = createSession(tx, token, csrfToken, userID)
	if err != nil {
		log.Printf("postInvitation.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postInvitation.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}

func getOngoingAlerts(tx *sql.Tx) ([]AlertDetail, error) {
	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where
			ended_at is null
		order by case 
			when severity = 'red' then 1
			when severity = 'amber' then 2
			else 3
		end asc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertDetail{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErr: %w", err)
	}

	alertIDs := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		alertIDs = append(alertIDs, strconv.Itoa(alert.ID))
	}

	messageQuery := fmt.Sprintf(
		`
			select
				id,
				content,
				created_at,
				last_updated_at,
				alert_id
			from
				alert_message
			where
				alert_id in(%s)
			order by created_at desc
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(messageQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query2: %w", err)
	}
	defer rows.Close()

	messages := map[int][]AlertDetailMessage{}

	for rows.Next() {
		alertID := 0
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan2: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		messages[alertID] = append(messages[alertID], message)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErrMessages: %w", err)
	}

	serviceQuery := fmt.Sprintf(
		`
		select
			service.id,
			service.name,
			service.helper_text,
			alert_id
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id in(%s)
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(serviceQuery)
	if err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.Query3: %w", err)
	}
	defer rows.Close()

	services := map[int][]AlertDetailService{}

	for rows.Next() {
		alertID := 0

		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getOngoingAlerts.Scan3: %w", err)
		}

		// services, not messages: the copy-paste initialised the wrong map, so
		// an alert with services but no messages was handed an empty Messages
		// slice and this guard did nothing it was meant to.
		if _, ok := services[alertID]; !ok {
			services[alertID] = []AlertDetailService{}
		}
		services[alertID] = append(services[alertID], service)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getOngoingAlerts.RowsErr: %w", err)
	}

	for i, alert := range alerts {
		if _, ok := messages[alert.ID]; ok {
			alerts[i].Messages = messages[alert.ID]
		}
		if _, ok := services[alert.ID]; ok {
			alerts[i].Services = services[alert.ID]
		}
	}

	return alerts, nil
}

func index(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("index.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("index.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("index.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("index.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("index.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	err = tx.Commit()
	if err != nil {
		log.Printf("index.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("index", indexMarkup)
	if err != nil {
		log.Printf("index.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  string
	}

	formattedAlerts := make([]FormattedAlertDetail, 0, len(alerts))

	for _, alert := range alerts {
		messages := make([]FormattedAlertDetailMessage, 0, len(alert.Messages))
		for _, message := range alert.Messages {
			createdAt := message.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
			if message.CreatedAt.Year() == time.Now().UTC().Year() {
				createdAt = message.CreatedAt.Format("Jan 2 • 15:04 MST")
			}

			formattedMessage := FormattedAlertDetailMessage{
				ID:        message.ID,
				Content:   message.Content,
				CreatedAt: createdAt,
			}
			if message.LastUpdatedAt != nil {
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04",
				)
			}

			messages = append(messages, formattedMessage)
		}

		serviceNames := make([]string, 0, len(alert.Services))
		for _, service := range alert.Services {
			serviceNames = append(serviceNames, service.Name)
		}

		formattedAlert := FormattedAlertDetail{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: alert.CreatedAt.Format("02/01/2006 15:04 MST"),
			Messages:  messages,
			Services:  strings.Join(serviceNames, " • "),
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	serviceStatuses := make(map[int]string, len(services))
	for _, service := range services {
		serviceStatuses[service.ID] = ""
	}
	for _, alert := range alerts {
		for _, service := range alert.Services {
			if serviceStatuses[service.ID] != "red" {
				serviceStatuses[service.ID] = alert.Severity
			}
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Services             []service
			IncidentAlerts       []FormattedAlertDetail
			ServiceStatuses      map[int]string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			Services:             services,
			IncidentAlerts:       formattedAlerts,
			ServiceStatuses:      serviceStatuses,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("index.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook")
}

// A cross-auth token mints a full admin session, and it travels in a URL
// query string -- proxy logs, browser history, Referer. Short-lived and
// single-use is the only thing keeping that from being a permanent
// credential lying around in logs.
const crossAuthTokenTTL = time.Minute

type crossAuthToken struct {
	userID   int
	issuedAt time.Time
}

var crossAuthTokensMu sync.Mutex
var crossAuthTokens = map[string]crossAuthToken{}

// redeemCrossAuthToken consumes a token, whatever the outcome: a token that
// was presented once is spent, valid or not.
func redeemCrossAuthToken(token string) (int, bool) {
	crossAuthTokensMu.Lock()
	defer crossAuthTokensMu.Unlock()

	now := time.Now().UTC()

	// Sweep here rather than on a timer: the map only grows when someone
	// issues a token, and this runs on the path that follows.
	for k, v := range crossAuthTokens {
		if now.Sub(v.issuedAt) > crossAuthTokenTTL {
			delete(crossAuthTokens, k)
		}
	}

	v, ok := crossAuthTokens[token]
	delete(crossAuthTokens, token)
	if !ok || now.Sub(v.issuedAt) > crossAuthTokenTTL {
		return 0, false
	}

	return v.userID, true
}

func postResolve(w http.ResponseWriter, r *http.Request) {
	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postResolve.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.URLEncoding.EncodeToString(tokenBytes)

	authCtx := getAuthCtx(r)

	crossAuthTokensMu.Lock()
	crossAuthTokens[token] = crossAuthToken{userID: authCtx.ID, issuedAt: time.Now().UTC()}
	crossAuthTokensMu.Unlock()

	w.Write([]byte(token))
}

// safeAfterPath keeps the "after" parameter to a path on this host. Anything
// else is an open redirect: an "@host/x" value terminates the authority's
// userinfo, so the browser lands on evil.example.com while the URL still
// opens with the real status domain.
func safeAfterPath(after string) string {
	if after == "" {
		return "/"
	}

	parsed, err := url.Parse(after)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Opaque != "" {
		return "/"
	}

	path := parsed.EscapedPath()
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "/"
	}

	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}

	return path
}

func getCrossAuth(w http.ResponseWriter, r *http.Request) {
	redirectURL := "https://" + metaDomain.Load() + safeAfterPath(r.URL.Query().Get("after"))

	auth := getAuthCtx(r)
	if auth.ID != 0 {
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	tokenParam := r.URL.Query().Get("token")
	if tokenParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	userID, ok := redeemCrossAuthToken(tokenParam)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("getCrossAuth.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("getCrossAuth.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("getCrossAuth.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if err = createSession(tx, token, csrfToken, userID); err != nil {
		log.Printf("getCrossAuth.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getCrossAuth.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func getOldestAlertDate(tx *sql.Tx) (time.Time, error) {
	const query = `
		select 
			created_at
		from
			alert
		order by 
			created_at asc
		limit 1
	`

	date := time.Time{}
	err := tx.QueryRow(query).Scan(&date)
	if err != nil {
		return date, fmt.Errorf("getOldestAlertDate: %w", err)
	}

	return date, nil
}

// periodStart is the first instant of the month being shown; the query bounds
// on [periodStart, next month) rather than strftime("%Y-%m", created_at) = ?,
// which is not sargable and made this public page scan the whole alert table.
func getAlertHistory(tx *sql.Tx, periodStart time.Time) ([]AlertDetail, error) {
	periodEnd := periodStart.AddDate(0, 1, 0)

	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where 
			created_at >= ? and created_at < ?
		order by created_at desc
	`

	alerts := []AlertDetail{}

	rows, err := tx.Query(alertQuery, periodStart, periodEnd)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertDetail{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErr: %w", err)
	}

	alertIDs := make([]string, 0, len(alerts))
	for _, alert := range alerts {
		alertIDs = append(alertIDs, strconv.Itoa(alert.ID))
	}

	messageQuery := fmt.Sprintf(
		`
			select
				id,
				content,
				created_at,
				last_updated_at,
				alert_id
			from
				alert_message
			where
				alert_id in(%s)
			order by created_at desc
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(messageQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query2: %w", err)
	}
	defer rows.Close()

	messages := map[int][]AlertDetailMessage{}

	for rows.Next() {
		alertID := 0
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan2: %w", err)
		}

		if _, ok := messages[alertID]; !ok {
			messages[alertID] = []AlertDetailMessage{}
		}
		messages[alertID] = append(messages[alertID], message)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErrMessages: %w", err)
	}

	serviceQuery := fmt.Sprintf(
		`
		select
			service.id,
			service.name,
			service.helper_text,
			alert_id
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id in(%s)
		`,
		strings.Join(alertIDs, ", "),
	)

	rows, err = tx.Query(serviceQuery)
	if err != nil {
		return alerts, fmt.Errorf("getAlertHistory.Query3: %w", err)
	}
	defer rows.Close()

	services := map[int][]AlertDetailService{}

	for rows.Next() {
		alertID := 0

		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
			&alertID,
		)
		if err != nil {
			return alerts, fmt.Errorf("getAlertHistory.Scan3: %w", err)
		}

		// services, not messages: the copy-paste initialised the wrong map, so
		// an alert with services but no messages was handed an empty Messages
		// slice and this guard did nothing it was meant to.
		if _, ok := services[alertID]; !ok {
			services[alertID] = []AlertDetailService{}
		}
		services[alertID] = append(services[alertID], service)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("getAlertHistory.RowsErr: %w", err)
	}

	for i, alert := range alerts {
		if _, ok := messages[alert.ID]; ok {
			alerts[i].Messages = messages[alert.ID]
		}
		if _, ok := services[alert.ID]; ok {
			alerts[i].Services = services[alert.ID]
		}
	}

	return alerts, nil
}

func history(w http.ResponseWriter, r *http.Request) {
	periodParam := r.URL.Query().Get("period")

	if len(periodParam) == 0 {
		periodParam = time.Now().UTC().Format("2006-01")
	}

	if len(periodParam) != 7 {
		http.Redirect(w, r, "/history", http.StatusFound)
		return
	}

	periodDate, err := time.Parse("2006-01", periodParam)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("history.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	oldestAlertDate, err := getOldestAlertDate(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getOldestAlertDate: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getAlertHistory(tx, periodDate)
	if err != nil {
		log.Printf("history.listAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	emailAlertChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("history.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasEmailAlertChannel := emailAlertChannelID != 0

	alertSettings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("history.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	hasSlackSetup := ""
	if alertSettings.SlackClientSecret != "" && alertSettings.SlackInstallURL != "" {
		hasSlackSetup = alertSettings.SlackInstallURL
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("history.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("history", historyMarkup)
	if err != nil {
		log.Printf("history.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  string
	}

	formattedAlerts := make([]FormattedAlertDetail, 0, len(alerts))

	for _, alert := range alerts {
		messages := make([]FormattedAlertDetailMessage, 0, len(alert.Messages))
		for _, message := range alert.Messages {
			formattedMessage := FormattedAlertDetailMessage{
				ID:        message.ID,
				Content:   message.Content,
				CreatedAt: message.CreatedAt.Format("Jan 2 • 15:04 MST"),
			}
			if message.LastUpdatedAt != nil {
				formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format(
					"02/01/2006 15:04 MST",
				)
			}

			messages = append(messages, formattedMessage)
		}

		serviceNames := make([]string, 0, len(alert.Services))
		for _, service := range alert.Services {
			serviceNames = append(serviceNames, service.Name)
		}

		formattedAlert := FormattedAlertDetail{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: alert.CreatedAt.Format("02/01/2006 15:04 MST"),
			Messages:  messages,
			Services:  strings.Join(serviceNames, " • "),
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	previousPeriodDate := periodDate.AddDate(0, -1, 0)
	nextPeriodDate := periodDate.AddDate(0, 1, 0)

	previousPeriodStr := previousPeriodDate.Format("2006-01")
	nextPeriodStr := nextPeriodDate.Format("2006-01")

	now := time.Now().UTC()

	if oldestAlertDate.IsZero() ||
		time.Date(previousPeriodDate.Year(), previousPeriodDate.Month(), 1, 0, 0, 0, 0, now.Location()).
			Before(time.Date(oldestAlertDate.Year(), oldestAlertDate.Month(), 1, 0, 0, 0, 0, now.Location())) {
		previousPeriodStr = ""
	}

	if nextPeriodDate.After(time.Now().UTC()) {
		nextPeriodStr = ""
	}

	err = tmpl.Execute(
		w,
		struct {
			IncidentAlerts       []FormattedAlertDetail
			MaintenanceAlerts    []FormattedAlertDetail
			PeriodText           string
			PreviousPeriod       string
			NextPeriod           string
			HasEmailAlertChannel bool
			HasSlackSetup        string
			Ctx                  pageCtx
		}{
			IncidentAlerts:       formattedAlerts,
			PeriodText:           periodDate.Format("Jan 2006"),
			PreviousPeriod:       previousPeriodStr,
			NextPeriod:           nextPeriodStr,
			HasEmailAlertChannel: hasEmailAlertChannel,
			HasSlackSetup:        hasSlackSetup,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("history.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getLogin(w http.ResponseWriter, r *http.Request) {

	authCtx := getAuthCtx(r)
	if authCtx.ID != 0 {
		http.Redirect(w, r, "/admin/alerts", http.StatusFound)
		return
	}

	tmpl, err := parseTmpl("getLogin", getLoginMarkup)
	if err != nil {
		log.Printf("getLogin.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tmpl.Execute(w, nil); err != nil {
		log.Printf("getLogin.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	rateLimitKey := loginRateLimitKey(r)
	if loginRateLimited(rateLimitKey) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Too many failed attempts. Try again later.
			</div>
		`))
		return
	}

	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Enter a username and password
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	if password == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Enter a username and password
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postLogin.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	pwHash, userID, err := getPasswordHash(tx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Hash against a throwaway so an unknown username costs the same
			// ~60ms as a known one. Returning early made the difference a
			// clean user-enumeration oracle.
			bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(password))

			recordLoginFailure(rateLimitKey)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="alert" class="alert" hx-swap-oob="true">
					Incorrect credentials
				</div>
			`))
			return
		}
		log.Printf("postLogin.getPasswordHash: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = bcrypt.CompareHashAndPassword([]byte(pwHash), []byte(password)); err != nil {
		recordLoginFailure(rateLimitKey)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Incorrect credentials
			</div>
		`))
		return
	}

	clearLoginFailures(rateLimitKey)

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postLogin.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postLogin.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	if err = createSession(tx, token, csrfToken, userID); err != nil {
		log.Printf("postLogin.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postLogin.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/admin/alerts")
}

func adminIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("HX-Location", "/admin/alerts")
}

type AlertListing struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
}

func listAlerts(tx *sql.Tx) ([]AlertListing, error) {
	const query = `
		select id, title, type, severity, created_at, ended_at from alert
		order by created_at desc
	`

	alerts := []AlertListing{}

	rows, err := tx.Query(query)
	if err != nil {
		return alerts, fmt.Errorf("listAlerts.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		alert := AlertListing{}
		err = rows.Scan(
			&alert.ID,
			&alert.Title,
			&alert.AlertType,
			&alert.Severity,
			&alert.CreatedAt,
			&alert.EndedAt,
		)
		if err != nil {
			return alerts, fmt.Errorf("listAlerts.Scan: %w", err)
		}

		alerts = append(alerts, alert)
	}

	if err := rows.Err(); err != nil {
		return alerts, fmt.Errorf("listAlerts.RowsErr: %w", err)
	}

	return alerts, nil
}

func alerts(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("alerts.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alerts, err := listAlerts(tx)
	if err != nil {
		log.Printf("alerts.listAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("alerts.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("alerts", alertsMarkup)
	if err != nil {
		log.Printf("alerts.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlert struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
	}

	formattedAlerts := make([]FormattedAlert, 0, len(alerts))
	for _, alert := range alerts {
		createdAt := alert.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
		if alert.CreatedAt.Year() == time.Now().UTC().Year() {
			createdAt = alert.CreatedAt.Format("Jan 2 • 15:04 MST")
		}

		formattedAlert := FormattedAlert{
			ID:        alert.ID,
			Title:     alert.Title,
			AlertType: alert.AlertType,
			Severity:  alert.Severity,
			CreatedAt: createdAt,
		}
		if alert.EndedAt != nil {
			formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlerts = append(formattedAlerts, formattedAlert)
	}

	err = tmpl.Execute(
		w,
		struct {
			Alerts []FormattedAlert
			Ctx    pageCtx
		}{

			Alerts: formattedAlerts,
			Ctx:    getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("alerts.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

type Monitor struct {
	ID             int
	Slug           string
	Name           string
	URL            string
	Method         string
	Frequency      int
	Timeout        int
	Attempts       int
	RequestHeaders map[string]string
	BodyFormat     sql.NullString
	Body           sql.NullString
}

func listMonitors(tx *sql.Tx) ([]Monitor, error) {
	const query = `
		select id, slug, name, url, method, frequency, timeout, attempts, request_headers, 
			body_format, body
		from monitor
	`

	monitorListings := []Monitor{}

	rows, err := tx.Query(query)
	if err != nil {
		return monitorListings, fmt.Errorf("listMonitors.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var serializedRequestHeaders sql.NullString

		monitor := Monitor{}
		err = rows.Scan(
			&monitor.ID,
			&monitor.Slug,
			&monitor.Name,
			&monitor.URL,
			&monitor.Method,
			&monitor.Frequency,
			&monitor.Timeout,
			&monitor.Attempts,
			&serializedRequestHeaders,
			&monitor.BodyFormat,
			&monitor.Body,
		)
		if err != nil {
			return monitorListings, fmt.Errorf("listMonitors.Scan: %w", err)
		}

		requestHeaders := map[string]string{}
		if serializedRequestHeaders.Valid {
			err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
			if err != nil {
				return monitorListings, fmt.Errorf("listMonitors.Unmarshal: %w", err)
			}
		}

		monitor.RequestHeaders = requestHeaders
		monitorListings = append(monitorListings, monitor)
	}

	if err := rows.Err(); err != nil {
		return monitorListings, fmt.Errorf("listMonitors.RowsErr: %w", err)
	}

	return monitorListings, nil
}

func monitors(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("monitors.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	if err != nil {
		log.Printf("monitors.listMonitors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastCheckedLogs, err := listAllMonitorLogLastChecked(tx)
	if err != nil {
		log.Printf("monitors.listAllMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorHappy := make(map[int]bool, len(lastCheckedLogs))
	for _, v := range lastCheckedLogs {
		monitorHappy[v.ID] = v.ResponseCode.Int32 != 0 && v.ResponseCode.Int32 < 400
	}


	tmpl, err := parseTmpl("monitors", monitorsMarkup)
	if err != nil {
		log.Printf("monitors.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitors     []Monitor
			MonitorHappy map[int]bool
			Ctx          pageCtx
		}{

			Monitors:     monitors,
			MonitorHappy: monitorHappy,
			Ctx:          getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("monitors.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorByID(tx *sql.Tx, id int) (Monitor, error) {
	const query = `
		select
			id,
			name,
			url,
			method,
			frequency,
			timeout,
			attempts,
			request_headers,
			body_format,
			body
		from
			monitor
		where
			id = ?
	`

	monitor := Monitor{}

	var serializedRequestHeaders sql.NullString

	err := tx.QueryRow(query, id).Scan(
		&monitor.ID,
		&monitor.Name,
		&monitor.URL,
		&monitor.Method,
		&monitor.Frequency,
		&monitor.Timeout,
		&monitor.Attempts,
		&serializedRequestHeaders,
		&monitor.BodyFormat,
		&monitor.Body,
	)
	if err != nil {
		return monitor, fmt.Errorf("getMonitorByID.QueryRow: %w", err)
	}

	requestHeaders := map[string]string{}

	if serializedRequestHeaders.Valid {
		err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
		if err != nil {
			return monitor, fmt.Errorf("getMonitorByID.Unmarshal: %w", err)
		}
	}

	monitor.RequestHeaders = requestHeaders

	return monitor, nil
}

type MonitorLog struct {
	ID           int
	StartedAt    time.Time
	EndedAt      time.Time
	ResponseCode sql.NullInt64
	ErrorMessage sql.NullString
	Attempts     int
	Result       string
	MonitorID    int
}

// monitorPollLimit caps the poll handler's query. High enough that a normal
// poll never notices; low enough that a crafted cursor cannot ask for a day.
const monitorPollLimit = 500

func listMonitorLogs(tx *sql.Tx, monitorID int, limit int, after int, before int, date time.Time) ([]MonitorLog, error) {
	query := `
		select
			id,
			started_at,
			ended_at,
			response_code,
			error_message,
			attempts,
			result,
			monitor_id
		from
			monitor_log
		where
			monitor_id = ?
	`

	if after != 0 {
		query += "and id < ?"
	}

	if before != 0 {
		query += " and id >= ?"
	}

	query += " and started_at >= ? and started_at < ?"

	query += "\norder by id desc"

	if limit > 0 {
		query += "\nlimit " + strconv.Itoa(limit)
	}

	monitorLogs := make([]MonitorLog, 0, limit)

	params := []any{monitorID}

	if after != 0 {
		params = append(params, after)
	}

	if before != 0 {
		params = append(params, before)
	}

	endOfDay := date.Add(time.Hour * 24)
	params = append(params, date, endOfDay)

	rows, err := tx.Query(query, params...)
	if err != nil {
		return monitorLogs, fmt.Errorf("listMonitorLogs.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		monitorLog := MonitorLog{}
		err = rows.Scan(
			&monitorLog.ID,
			&monitorLog.StartedAt,
			&monitorLog.EndedAt,
			&monitorLog.ResponseCode,
			&monitorLog.ErrorMessage,
			&monitorLog.Attempts,
			&monitorLog.Result,
			&monitorLog.MonitorID,
		)
		if err != nil {
			return monitorLogs, fmt.Errorf("listMonitorLogs.Scan: %w", err)
		}
		monitorLogs = append(monitorLogs, monitorLog)
	}

	if err := rows.Err(); err != nil {
		return monitorLogs, fmt.Errorf("listMonitorLogs.RowsErr: %w", err)
	}

	return monitorLogs, nil
}

type MonitorLogView struct {
	ID           int
	StartedAt    string
	Latency      time.Duration
	ResponseCode sql.NullInt64
	ErrorMessage sql.NullString
	Attempts     int
	Result       string
	MonitorID    int
}

func getMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		dateParam = time.Now().UTC().Format("2006-01-02")
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	const logLimit = 100

	monitor, err := getMonitorByID(tx, id)
	if err != nil {
		log.Printf("getMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorLogs, err := listMonitorLogs(tx, id, logLimit, 0, 0, date)
	if err != nil {
		log.Printf("getMonitor.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastChecked, err := getMonitorLogLastChecked(tx, monitor.ID)
	if err != nil {
		log.Printf("getMonitor.getMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if _, ok := r.URL.Query()["ready"]; ok {
		if len(monitorLogs) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}


	tmpl, err := parseTmpl("getMonitor", getMonitorMarkup)
	if err != nil {
		log.Printf("getMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	refreshDelay := 5

	nextRefreshMsg := fmt.Sprintf(
		"Checking for updates in %ds",
		refreshDelay,
	)

	timeIDs := make(map[int]string, logLimit)
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() || log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
	}

	formattedMonitorLogs := make([]MonitorLogView, 0, len(monitorLogs))
	for _, log := range monitorLogs {
		formattedMonitorLogs = append(
			formattedMonitorLogs,
			MonitorLogView{
				ID:        log.ID,
				StartedAt: log.StartedAt.Format("2006/01/02 15:04:05 MST"),
				Latency: log.EndedAt.Sub(log.StartedAt).
					Round(time.Millisecond * 1),
				ResponseCode: log.ResponseCode,
				ErrorMessage: log.ErrorMessage,
				Attempts:     log.Attempts,
				Result:       log.Result,
				MonitorID:    log.MonitorID,
			},
		)
	}

	if dateParam != "" {
		w.Header().Set("HX-Push-Url", r.URL.Path+"?date="+dateParam)
	}

	lastLogID := 0
	if len(monitorLogs) > 0 {
		lastLogID = monitorLogs[len(monitorLogs)-1].ID
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitor            Monitor
			Logs               []MonitorLogView
			NextRefreshMsg     string
			LastCheckedSuccess bool
			LastLogID          int
			TimeIDs            map[int]string
			RefreshDelay       int
			Ctx                pageCtx
			Date               string
		}{
			Monitor:            monitor,
			Logs:               formattedMonitorLogs,
			NextRefreshMsg:     nextRefreshMsg,
			LastCheckedSuccess: lastChecked.ResponseCode.Int32 != 0 && lastChecked.ResponseCode.Int32 < 400,
			LastLogID:          lastLogID,
			TimeIDs:            timeIDs,
			RefreshDelay:       refreshDelay,
			Date:               dateParam,
			Ctx:                getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorAllLogs(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	afterParam := r.URL.Query().Get("after")
	if afterParam == "" {
		// A bare return sends 200 with an empty body, which reads as "no logs"
		// rather than "you asked wrong".
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	after, err := strconv.Atoi(afterParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitorAllLogs.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	const logLimit = 2160

	monitorLogs, err := listMonitorLogs(tx, id, logLimit, after, 0, date)
	if err != nil {
		log.Printf("getMonitorAllLogs.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitorAllLogs.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastLogID := 0
	if len(monitorLogs) > 0 {
		lastLogID = monitorLogs[len(monitorLogs)-1].ID
	}


	tmpl, err := parseTmpl("getMonitorAllLogs", getMonitorAllLogsMarkup)
	if err != nil {
		log.Printf("getMonitorAllLogs.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	formattedMonitorLogs := make([]MonitorLogView, 0, len(monitorLogs))
	for _, log := range monitorLogs {
		formattedMonitorLogs = append(
			formattedMonitorLogs,
			MonitorLogView{
				ID:        log.ID,
				StartedAt: log.StartedAt.Format("2006/01/02 15:04:05 MST"),
				Latency: log.EndedAt.Sub(log.StartedAt).
					Round(time.Millisecond * 1),
				ResponseCode: log.ResponseCode,
				ErrorMessage: log.ErrorMessage,
				Attempts:     log.Attempts,
				Result:       log.Result,
				MonitorID:    log.MonitorID,
			},
		)
	}

	timeIDs := make(map[int]string, logLimit)
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() ||
				log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Logs      []MonitorLogView
			LastLogID int
			MonitorID int
			TimeIDs   map[int]string
			Date      string
		}{
			Logs:      formattedMonitorLogs,
			LastLogID: lastLogID,
			MonitorID: id,
			TimeIDs:   timeIDs,
			Date:      dateParam,
		},
	)
	if err != nil {
		log.Printf("getMonitorAllLogs.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorPoll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	beforeParam := r.URL.Query().Get("before")
	if beforeParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	before, err := strconv.Atoi(beforeParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitorPoll.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// Bounded: limit 0 means "no limit" in listMonitorLogs, and this handler
	// is polled by the browser every 5s. Normal use returns 0-1 rows, but a
	// crafted ?before= returned every log for the day -- 8,640 at the minimum
	// frequency.
	monitorLogs, err := listMonitorLogs(tx, id, monitorPollLimit, 0, before, date)
	if err != nil {
		log.Printf("getMonitorPoll.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastChecked, err := getMonitorLogLastChecked(tx, id)
	if err != nil {
		log.Printf("getMonitorPoll.getMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitorPoll.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	refreshDelay := 5

	nextRefreshMsg := fmt.Sprintf(
		"Checking for updates in %ds",
		refreshDelay,
	)


	tmpl, err := parseTmpl("getMonitorPoll", getMonitorPollMarkup)
	if err != nil {
		log.Printf("getMonitorPoll.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	formattedMonitorLogs := make([]MonitorLogView, 0, len(monitorLogs))
	for _, log := range monitorLogs {
		formattedMonitorLogs = append(
			formattedMonitorLogs,
			MonitorLogView{
				ID:        log.ID,
				StartedAt: log.StartedAt.Format("2006/01/02 15:04:05 MST"),
				Latency: log.EndedAt.Sub(log.StartedAt).
					Round(time.Millisecond * 1),
				ResponseCode: log.ResponseCode,
				ErrorMessage: log.ErrorMessage,
				Attempts:     log.Attempts,
				Result:       log.Result,
				MonitorID:    log.MonitorID,
			},
		)
	}

	timeIDs := make(map[int]string, len(formattedMonitorLogs))
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() ||
				log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Logs               []MonitorLogView
			LastLogID          int
			MonitorID          int
			TimeIDs            map[int]string
			RefreshDelay       int
			NextRefreshMsg     string
			LastCheckedSuccess bool
			Date               string
		}{
			Logs:               formattedMonitorLogs,
			MonitorID:          id,
			TimeIDs:            timeIDs,
			RefreshDelay:       5,
			NextRefreshMsg:     nextRefreshMsg,
			LastCheckedSuccess: lastChecked.ResponseCode.Int32 != 0 && lastChecked.ResponseCode.Int32 < 400,
			Date:               dateParam,
		},
	)
	if err != nil {
		log.Printf("getMonitorPoll.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getEditMonitor(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	refreshID := r.URL.Query().Get("refresh")

	// A non-numeric id became 0, which matches no monitor, and the handler
	// then rendered a page for a monitor that does not exist.
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitor, err := getMonitorByID(tx, id)
	if err != nil {
		log.Printf("getEditMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("getEditMonitor.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorNotificationChannels, err := listNotificationChannelsByMonitorID(tx, monitor.ID)
	if err != nil {
		log.Printf("getEditMonitor.listNotificationChannelsByMonitorID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	monitorNotificationsMap := map[int]bool{}
	for _, v := range monitorNotificationChannels {
		monitorNotificationsMap[v.ID] = true
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("getEditMonitor.mailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	selectedMailGroups, err := listMailGroupIDsByMonitorID(tx, monitor.ID)
	if err != nil {
		log.Printf("getEditMonitor.listMailGroupIDsByMonitorID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	selectedMailGroupsMap := map[int]bool{}
	for _, v := range selectedMailGroups {
		selectedMailGroupsMap[v.ID] = true
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getEditMonitor", getEditMonitorMarkup)
	if err != nil {
		log.Printf("getEditMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	textBody := ""
	if monitor.BodyFormat.String == "text" {
		textBody = monitor.Body.String
	}

	formData := url.Values{}
	if monitor.BodyFormat.String == "form" {
		data, err := url.ParseQuery(monitor.Body.String)
		if err != nil {
			log.Printf("getEditMonitor.ParseQuery: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		formData = data
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitor              Monitor
			TextBody             string
			FormData             url.Values
			Notifications        []NotificationChannel
			MonitorNotifications map[int]bool
			MailGroups           []MailGroup
			SelectedMailGroups   map[int]bool
			RefreshID            string
			ReadOnly             bool
			Ctx                  pageCtx
		}{
			Monitor:              monitor,
			TextBody:             textBody,
			FormData:             formData,
			Notifications:        channels,
			MonitorNotifications: monitorNotificationsMap,
			MailGroups:           mailGroups,
			SelectedMailGroups:   selectedMailGroupsMap,
			RefreshID:            refreshID,
			ReadOnly:             readOnly,
			Ctx:                  getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getDetailsMonitor(w http.ResponseWriter, r *http.Request) {
	getEditMonitor(w, r)
}

func editMonitor(
	tx *sql.Tx,
	id int,
	name string,
	url string,
	method string,
	frequency int,
	timeout int,
	attempts int,
	requestHeaders sql.NullString,
	bodyFormat sql.NullString,
	body sql.NullString,
) (int, error) {
	const query = `
		update monitor set name = ?, url = ?, method = ?, frequency = ?, timeout = ?, 
			attempts = ?, request_headers = ?, body_format = ?, body = ?
		where id = ?
	`

	var monitorID int
	_, err := tx.Exec(
		query,
		name,
		url,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
		id,
	)
	if err != nil {
		return monitorID, fmt.Errorf("editMonitor.QueryRow: %w", err)
	}

	return id, nil
}

func updateMonitorSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update monitor set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateMonitorSlug.QueryRow: %w", err)
	}

	return id, nil
}

func postEditMonitor(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	reqURL := r.PostFormValue("url")
	validURL := true
	parsedReqURL, err := url.Parse(reqURL)
	if err != nil {
		validURL = false
	} else if parsedReqURL.Scheme == "" || parsedReqURL.Host == "" {
		validURL = false
	} else if parsedReqURL.Scheme != "http" && parsedReqURL.Scheme != "https" {
		validURL = false
	}

	if !validURL {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<span id="alert-error" hx-swap-oob="true">
				Invalid URL
			</span>
		`))
		return
	}

	method := r.PostFormValue("method")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if method != http.MethodGet &&
		method != http.MethodPost &&
		method != http.MethodPatch &&
		method != http.MethodPut &&
		method != http.MethodDelete {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	frequency, err := strconv.Atoi(r.PostFormValue("frequency"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if frequency != 10 && frequency != 30 && frequency != 60 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	timeout, err := strconv.Atoi(r.PostFormValue("timeout"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if timeout != 5 && timeout != 10 && timeout != 15 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	attempts, err := strconv.Atoi(r.PostFormValue("attempts"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if attempts != 1 && attempts != 2 && attempts != 3 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	requestHeaders := sql.NullString{}
	requestHeadersMap := map[string]string{}
	if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
		// PostForm, not Form: Form merges the query string in, so
		// ?header-key=x on the URL injected an entry and skewed the two
		// slices against each other. Bounded by the shorter of the two --
		// indexing values by the keys' length panicked on any mismatch.
		keys := r.PostForm["header-key"]
		values := r.PostForm["header-value"]
		for i := 0; i < len(keys) && i < len(values); i++ {
			requestHeadersMap[keys[i]] = values[i]
		}
	}
	requestHeadersSerialized, err := json.Marshal(requestHeadersMap)
	if err != nil {
		log.Printf("postEditMonitor.Marshal: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(requestHeadersMap) > 0 {
		requestHeaders = sql.NullString{
			Valid:  true,
			String: string(requestHeadersSerialized),
		}
	}

	bodyFormat := sql.NullString{}
	if r.PostFormValue("format") != "" {
		bodyFormat = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("format"),
		}
	}

	body := sql.NullString{}
	if r.PostFormValue("body") != "" {
		body = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("body"),
		}
	}

	if r.PostFormValue("form-key") != "" && r.PostFormValue("form-value") != "" {
		urlValues := url.Values{}
		formKeys := r.PostForm["form-key"]
		formValues := r.PostForm["form-value"]
		for i := 0; i < len(formKeys) && i < len(formValues); i++ {
			urlValues.Add(formKeys[i], formValues[i])
		}
		body = sql.NullString{
			Valid:  true,
			String: urlValues.Encode(),
		}
	}

	notificationChannelsParam := r.PostForm["notification-channels"]
	notificationChannels := make([]int, 0, len(notificationChannelsParam))
	for _, channelID := range notificationChannelsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		notificationChannels = append(notificationChannels, id)
	}

	mailGroupsParam := r.PostForm["mail-groups"]
	mailGroups := make([]int, 0, len(mailGroupsParam))
	for _, channelID := range mailGroupsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		mailGroups = append(mailGroups, id)
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	idParam := chi.URLParam(r, "id")
	monitorID, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	_, err = getMonitorByID(tx, monitorID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		log.Printf("postEditMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, err = editMonitor(
		tx,
		monitorID,
		name,
		reqURL,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
	)
	if err != nil {
		log.Printf("postEditMonitor.createMonitor: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorNotificationChannels(tx, monitorID, notificationChannels)
	if err != nil {
		log.Printf("postEditMonitor.updateMonitorNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorMailGroups(tx, monitorID, mailGroups)
	if err != nil {
		log.Printf("postEditMonitor.updateMonitorMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postEditMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors/"+idParam)
}

func deleteMonitorByID(tx *sql.Tx, id int) error {
	const query = `
		delete from monitor where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteMonitorByID.Exec: %w", err)
	}

	return nil
}

func deleteMonitor(w http.ResponseWriter, r *http.Request) {
	// Every create and edit handler has this gate; the four delete handlers
	// did not, so config-file mode hid the buttons while the routes still
	// worked -- and under GitHub-managed config the deleted entity does not
	// come back, because the webhook skips a config whose SHA is unchanged.
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	err = deleteMonitorByID(tx, id)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.deleteMonitorByID: %s", err)
		return
	}

	err = tx.Commit()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Printf("deleteMonitor.Commit: %s", err)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors")
}

func getCreateMonitor(w http.ResponseWriter, r *http.Request) {
	refreshID := r.URL.Query().Get("refresh")

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notifications, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("getCreateMonitor.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("getEditMonitor.mailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getCreateMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getCreateMonitor", getCreateMonitorMarkup)
	if err != nil {
		log.Printf("getCreateMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Notifications []NotificationChannel
			MailGroups    []MailGroup
			RefreshID     string
			Ctx           pageCtx
		}{
			Notifications: notifications,
			MailGroups:    mailGroups,
			RefreshID:     refreshID,
			Ctx:           getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func updateMonitorNotificationChannels(tx *sql.Tx, monitorID int, channelIDs []int) error {
	const deleteQuery = `
		delete from monitor_notification_channel where monitor_id = ?
	`

	_, err := tx.Exec(deleteQuery, monitorID)
	if err != nil {
		return fmt.Errorf("updateMonitorNotificationChannels.DeleteExec: %w", err)
	}

	if len(channelIDs) > 0 {
		const baseInsertQuery = `
			insert into monitor_notification_channel(monitor_id, notification_channel_id)
			values
		`

		insertQuery := baseInsertQuery

		for i := range channelIDs {
			insertQuery += "(?, ?)"

			if i != len(channelIDs)-1 {
				insertQuery += ","
			}
		}

		params := []any{}
		for _, v := range channelIDs {
			params = append(params, monitorID, v)
		}

		_, err = tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMonitorNotificationChannels.InsertExec: %w", err)
		}
	}

	return nil
}

func createMonitor(
	tx *sql.Tx,
	slug string,
	name string,
	url string,
	method string,
	frequency int,
	timeout int,
	attempts int,
	requestHeaders sql.NullString,
	bodyFormat sql.NullString,
	body sql.NullString,
) (int, error) {
	const query = `
		insert into
			monitor(slug, name, url, method, frequency, timeout, attempts, request_headers, 
				body_format, body)
			values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id
	`

	var monitorID int
	err := tx.QueryRow(
		query,
		slug,
		name,
		url,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		bodyFormat,
		body,
	).Scan(&monitorID)
	if err != nil {
		return monitorID, fmt.Errorf("createMonitor.QueryRow: %w", err)
	}

	return monitorID, nil
}

func postCreateMonitor(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	reqURL := r.PostFormValue("url")
	validURL := true
	parsedReqURL, err := url.Parse(reqURL)
	if err != nil {
		validURL = false
	} else if parsedReqURL.Scheme == "" || parsedReqURL.Host == "" {
		validURL = false
	} else if parsedReqURL.Scheme != "http" && parsedReqURL.Scheme != "https" {
		validURL = false
	}

	if !validURL {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<span id="alert-error" hx-swap-oob="true">
				Invalid URL
			</span>
		`))
		return
	}

	method := r.PostFormValue("method")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if method != http.MethodGet &&
		method != http.MethodPost &&
		method != http.MethodPatch &&
		method != http.MethodPut &&
		method != http.MethodDelete {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	frequency, err := strconv.Atoi(r.PostFormValue("frequency"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if frequency != 10 && frequency != 30 && frequency != 60 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	timeout, err := strconv.Atoi(r.PostFormValue("timeout"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if timeout != 5 && timeout != 10 && timeout != 15 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	attempts, err := strconv.Atoi(r.PostFormValue("attempts"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if attempts != 1 && attempts != 2 && attempts != 3 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	requestHeaders := sql.NullString{}
	requestHeadersMap := map[string]string{}
	if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
		// PostForm, not Form: Form merges the query string in, so
		// ?header-key=x on the URL injected an entry and skewed the two
		// slices against each other. Bounded by the shorter of the two --
		// indexing values by the keys' length panicked on any mismatch.
		keys := r.PostForm["header-key"]
		values := r.PostForm["header-value"]
		for i := 0; i < len(keys) && i < len(values); i++ {
			requestHeadersMap[keys[i]] = values[i]
		}
	}
	requestHeadersSerialized, err := json.Marshal(requestHeadersMap)
	if err != nil {
		log.Printf("postEditMonitor.Marshal: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(requestHeadersMap) > 0 {
		requestHeaders = sql.NullString{
			Valid:  true,
			String: string(requestHeadersSerialized),
		}
	}

	body := sql.NullString{}
	if r.PostFormValue("body") != "" {
		body = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("body"),
		}
	}

	if r.PostFormValue("form-key") != "" && r.PostFormValue("form-value") != "" {
		urlValues := url.Values{}
		formKeys := r.PostForm["form-key"]
		formValues := r.PostForm["form-value"]
		for i := 0; i < len(formKeys) && i < len(formValues); i++ {
			urlValues.Add(formKeys[i], formValues[i])
		}
		body = sql.NullString{
			Valid:  true,
			String: urlValues.Encode(),
		}
	}

	format := sql.NullString{}
	if r.PostFormValue("format") != "" {
		format = sql.NullString{
			Valid:  true,
			String: r.PostFormValue("format"),
		}
	}

	notificationChannelsParam := r.PostForm["notification-channels"]
	notificationChannels := make([]int, 0, len(notificationChannelsParam))
	for _, channelID := range notificationChannelsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		notificationChannels = append(notificationChannels, id)
	}

	mailGroupsParam := r.PostForm["mail-groups"]
	mailGroups := make([]int, 0, len(mailGroupsParam))
	for _, channelID := range mailGroupsParam {
		id, err := strconv.Atoi(channelID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		mailGroups = append(mailGroups, id)
	}
	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	if err != nil {
		log.Printf("postCreateMonitor.listMonitors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorSlugs := map[string]bool{}
	for _, v := range monitors {
		monitorSlugs[v.Slug] = true
	}

	monitorID, err := createMonitor(
		tx,
		generateSlug(name, monitorSlugs),
		name,
		reqURL,
		method,
		frequency,
		timeout,
		attempts,
		requestHeaders,
		format,
		body,
	)
	if err != nil {
		log.Printf("postCreateMonitor.createMonitor: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorNotificationChannels(tx, monitorID, notificationChannels)
	if err != nil {
		log.Printf("postCreateMonitor.updateMonitorNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMonitorMailGroups(tx, monitorID, mailGroups)
	if err != nil {
		log.Printf("postEditMonitor.updateMonitorMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postCreateMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/monitors/"+strconv.Itoa(monitorID))
}

type AlertDetailMessage struct {
	ID            int
	Content       string
	CreatedAt     *time.Time
	LastUpdatedAt *time.Time
}

type AlertDetailService struct {
	ID         int
	Name       string
	HelperText string
}

type AlertDetail struct {
	ID        int
	Title     string
	AlertType string
	Severity  string
	CreatedAt *time.Time
	EndedAt   *time.Time
	Messages  []AlertDetailMessage
	Services  []AlertDetailService
}

func getAlertByID(tx *sql.Tx, id int) (AlertDetail, error) {
	const alertQuery = `
		select 
			id,
			title,
			type,
			severity,
			created_at,
			ended_at
		from
			alert
		where
			id = ?
	`

	alert := AlertDetail{}

	err := tx.QueryRow(alertQuery, id).Scan(
		&alert.ID,
		&alert.Title,
		&alert.AlertType,
		&alert.Severity,
		&alert.CreatedAt,
		&alert.EndedAt,
	)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.QueryRow: %w", err)
	}

	const messageQuery = `
		select
			id,
			content,
			created_at,
			last_updated_at
		from
			alert_message
		where
			alert_id = ?
		order by created_at desc
	`

	rows, err := tx.Query(messageQuery, id)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		message := AlertDetailMessage{}
		err = rows.Scan(
			&message.ID,
			&message.Content,
			&message.CreatedAt,
			&message.LastUpdatedAt,
		)
		if err != nil {
			return alert, fmt.Errorf("getAlertByID.Scan: %w", err)
		}

		alert.Messages = append(alert.Messages, message)
	}

	if err := rows.Err(); err != nil {
		return alert, fmt.Errorf("getAlertByID.RowsErr: %w", err)
	}

	const serviceQuery = `
		select
			service.id,
			service.name,
			service.helper_text
		from
			alert_service
		left join
			service on service.id = alert_service.service_id
		where
			alert_id = ?
	`

	rows, err = tx.Query(serviceQuery, id)
	if err != nil {
		return alert, fmt.Errorf("getAlertByID.Query2: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		service := AlertDetailService{}
		err = rows.Scan(
			&service.ID,
			&service.Name,
			&service.HelperText,
		)
		if err != nil {
			return alert, fmt.Errorf("getAlertByID.Scan2: %w", err)
		}

		alert.Services = append(alert.Services, service)
	}

	if err := rows.Err(); err != nil {
		return alert, fmt.Errorf("getAlertByID.RowsErr: %w", err)
	}

	return alert, nil
}

type AlertSettings struct {
	SlackInstallURL      string
	SlackClientSecret    string
	ManagedSubscriptions bool
}

func getAlertSettings(tx *sql.Tx) (AlertSettings, error) {
	const query = `
		select name, value from alert_setting
	`

	settings := AlertSettings{}

	rows, err := tx.Query(query)
	if err != nil {
		return settings, fmt.Errorf("getAlertSettings.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string

		err = rows.Scan(&k, &v)
		if err != nil {
			return settings, fmt.Errorf("getAlertSettings.Scan: %w", err)
		}

		if k == "slack-install-url" {
			settings.SlackInstallURL = v
		} else if k == "slack-client-secret" {
			settings.SlackClientSecret = v
		} else if k == "managed-subscriptions" {
			parsedV, err := strconv.ParseBool(v)
			if err != nil {
				return settings, fmt.Errorf("getAlertSettings.ParseBool: %w", err)
			}
			settings.ManagedSubscriptions = parsedV
		}
	}

	if err := rows.Err(); err != nil {
		return settings, fmt.Errorf("getAlertSettings.RowsErr: %w", err)
	}

	return settings, nil
}

func getAlertSMTPNotificationSetting(tx *sql.Tx) (int, error) {
	const query = `
		select notification_channel_id from alert_setting_smtp_notification limit 1
	`

	v := 0

	err := tx.QueryRow(query).Scan(&v)
	if err != nil {
		return v, fmt.Errorf("getAlertSMTPNotificationSetting.QueryRow: %w", err)
	}

	return v, nil
}

func getAlertNotifications(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAlertNotifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	notifications, err := listNotificationChannels(tx, listNotificationsOptions{Type: "smtp"})
	if err != nil {
		log.Printf("getAlertNotifications.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	settings, err := getAlertSettings(tx)
	if err != nil {
		log.Printf("getAlertNotifications.getAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	smtpNotificationChannelID, err := getAlertSMTPNotificationSetting(tx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getAlertNotifications.getAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getAlertNotifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getAlertNotifications", getAlertNotificationsMarkup)
	if err != nil {
		log.Printf("getAlertNotifications.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, struct {
		Notifications           []NotificationChannel
		Settings                AlertSettings
		SMTPNotificationChannel int
		Domain                  string
		ConfigFileEnabled       bool
		Ctx                     pageCtx
	}{
		Notifications:           notifications,
		Settings:                settings,
		SMTPNotificationChannel: smtpNotificationChannelID,
		Domain:                  metaDomain.Load(),
		ConfigFileEnabled:       metaConfigFileEnabled.Load(),
		Ctx:                     getPageCtx(r),
	})
	if err != nil {
		log.Printf("getAlertNotifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func updateAlertSettings(
	tx *sql.Tx,
	slackInstallURL string,
	slackClientSecret string,
	managedSubscriptions bool,
) error {
	const query = `
		insert into alert_setting(name, value) values(?, ?), (?, ?), (?, ?)
		on conflict(name) do update set value = excluded.value
	`

	_, err := tx.Exec(
		query,
		"slack-install-url",
		slackInstallURL,
		"slack-client-secret",
		slackClientSecret,
		"managed-subscriptions",
		managedSubscriptions,
	)
	if err != nil {
		return fmt.Errorf("updateAlertSettings.Exec: %w", err)
	}

	return nil
}

func updateAlertSMTPNotificationSetting(tx *sql.Tx, notificationID int) error {
	const deleteQuery = `
		delete from alert_setting_smtp_notification
	`

	_, err := tx.Exec(deleteQuery)
	if err != nil {
		return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecDelete: %w", err)
	}

	if notificationID != 0 {
		const insertQuery = `
			insert into alert_setting_smtp_notification(notification_channel_id) values(?)
		`

		_, err = tx.Exec(insertQuery, notificationID)
		if err != nil {
			return fmt.Errorf("updateAlertSMTPNotificationSetting.ExecInsert: %w", err)
		}
	}

	return nil
}

func postAlertNotifications(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	slackInstallURLParam := r.FormValue("slack-install-url")
	slackInstallURL := ""
	if slackInstallURLParam != "" {
		url, err := url.ParseRequestURI(slackInstallURLParam)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		slackInstallURL = url.String()
	}

	slackClientSecretParam := r.FormValue("slack-client-secret")

	smtpNotificationChannelIDParam := r.FormValue("smtp-notification-channel")
	smtpNotificationChannelID := 0
	if smtpNotificationChannelIDParam != "" {
		id, err := strconv.Atoi(r.FormValue("smtp-notification-channel"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		smtpNotificationChannelID = id
	}

	managedSubscriptions := false
	managedSubscriptionsParam := r.FormValue("managed-subscriptions")
	if managedSubscriptionsParam == "on" {
		managedSubscriptions = true
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postAlertNotifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, smtpNotificationChannelID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postAlertNotifications.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if channel.ID != 0 && channel.Type != "smtp" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	err = updateAlertSMTPNotificationSetting(tx, channel.ID)
	if err != nil {
		log.Printf("postAlertNotifications.updateAlertSMTPNotificationSetting: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateAlertSettings(tx, slackInstallURL, slackClientSecretParam, managedSubscriptions)
	if err != nil {
		log.Printf("postAlertNotifications.updateAlertSettings: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postAlertNotifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

func getAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, id)
	if err != nil {
		log.Printf("getAlert.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getAlert", getAlertMarkup)
	if err != nil {
		log.Printf("getAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedAlertDetailMessage struct {
		ID            int
		Content       string
		CreatedAt     string
		LastUpdatedAt string
	}

	type FormattedAlertDetailService struct {
		ID         int
		Name       string
		HelperText string
	}

	type FormattedAlertDetail struct {
		ID        int
		Title     string
		AlertType string
		Severity  string
		CreatedAt string
		EndedAt   string
		Messages  []FormattedAlertDetailMessage
		Services  []FormattedAlertDetailService
	}

	formattedAlert := FormattedAlertDetail{}
	formattedAlert.ID = alert.ID
	formattedAlert.Title = alert.Title
	formattedAlert.AlertType = alert.AlertType
	formattedAlert.Severity = alert.Severity
	formattedAlert.CreatedAt = alert.CreatedAt.Format("02/01/2006 15:04 MST")
	if alert.EndedAt != nil {
		formattedAlert.EndedAt = alert.EndedAt.Format("02/01/2006 15:04 MST")
	}

	for _, message := range alert.Messages {
		createdAt := message.CreatedAt.Format("Jan 2 2006 • 15:04 MST")
		if message.CreatedAt.Year() == time.Now().UTC().Year() {
			createdAt = message.CreatedAt.Format("Jan 2 • 15:04 MST")
		}

		formattedMessage := FormattedAlertDetailMessage{
			ID:        message.ID,
			Content:   message.Content,
			CreatedAt: createdAt,
		}
		if message.LastUpdatedAt != nil {
			formattedMessage.LastUpdatedAt = message.LastUpdatedAt.Format("02/01/2006 15:04 MST")
		}

		formattedAlert.Messages = append(
			formattedAlert.Messages,
			formattedMessage,
		)
	}

	serviceNames := make([]string, 0, len(alert.Services))
	for _, service := range alert.Services {
		serviceNames = append(serviceNames, service.Name)
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert    FormattedAlertDetail
			Services string
			Ctx      pageCtx
		}{
			Alert:    formattedAlert,
			Services: strings.Join(serviceNames, " • "),
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func deleteAlertByID(tx *sql.Tx, id int) error {
	const query = `
		delete from alert where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteAlertByID.Exec: %w", err)
	}

	return nil
}

func deleteAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteAlertByID(tx, id)
	if err != nil {
		log.Printf("deleteAlert.deleteAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

func getCreateAlert(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("getCreateAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("getCreateAlert.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getCreateAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getCreateAlert", getCreateAlertMarkup)
	if err != nil {
		log.Printf("getCreateAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Services []service
			Ctx      pageCtx
		}{
			Services: services,
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createAlert(
	tx *sql.Tx,
	title string,
	services []int,
	alertType string,
	severity string,
) (int, error) {
	const alertQuery = `
		insert into alert(title, type, severity, created_at) values(?, ?, ?, ?) returning id
	`

	alertID := 0
	err := tx.QueryRow(alertQuery, title, alertType, severity, time.Now().UTC()).Scan(&alertID)
	if err != nil {
		return alertID, fmt.Errorf("createAlert.Scan: %w", err)
	}

	const baseServiceQuery = `
		insert into alert_service(alert_id, service_id) values
	`

	serviceQuery := baseServiceQuery

	params := []any{}

	for i, serviceID := range services {
		serviceQuery += "(?, ?)"
		if i < len(services)-1 {
			serviceQuery += ", "
		}
		params = append(params, alertID, serviceID)
	}

	_, err = tx.Exec(serviceQuery, params...)
	if err != nil {
		return alertID, fmt.Errorf("createAlert.Exec: %w", err)
	}

	return alertID, nil
}

func createAlertMessageNotifications(tx *sql.Tx, createdAt time.Time, alertMessageID int) error {
	const query = `
		insert into alert_notification(created_at, alert_subscription_id, alert_message_id)
		select ?, id, ? from alert_subscription where alert_subscription.active = true
	`

	_, err := tx.Exec(query, time.Now().UTC(), alertMessageID)
	if err != nil {
		return fmt.Errorf("createAlertMessageNotifications.Exec: %w", err)
	}

	return nil
}

func updateAlertSentAtByID(tx *sql.Tx, now time.Time, ids []int) error {
	const baseQuery = `
		update alert_notification set sent_at = ?
		where id in(
	`

	query := baseQuery

	params := []any{time.Now().UTC()}
	for i, destination := range ids {
		query += "?"
		if i < len(ids)-1 {
			query += ","
		}
		params = append(params, destination)
	}
	query += ")"

	_, err := tx.Exec(query, params...)
	if err != nil {
		return fmt.Errorf("updateAlertSentAtByID.Exec: %w", err)
	}

	return nil
}

func postCreateAlert(w http.ResponseWriter, r *http.Request) {
	title := r.PostFormValue("title")
	if title == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")
	if message == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	services := []int{}
	for _, service := range r.PostForm["services"] {
		num, err := strconv.Atoi(service)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		services = append(services, num)
	}
	if len(services) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	alertType := r.PostFormValue("type")
	if alertType != "incident" && alertType != "maintenance" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	severity := r.PostFormValue("severity")
	if alertType == "incident" {
		if severity != "red" && severity != "amber" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		alertType = "maintenance"
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alertID, err := createAlert(tx, title, services, alertType, severity)
	if err != nil {
		log.Printf("postCreateAlert.createAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertMessageID, err := createAlertMessage(tx, alertID, message)
	if err != nil {
		log.Printf("postCreateAlert.createAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postCreateAlert.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	newSeverity := "blue"
	for _, alert := range alerts {
		if alert.Severity == "amber" {
			newSeverity = "amber"
			continue
		}

		if alert.Severity == "red" {
			newSeverity = "red"
			break
		}
	}

	err = updateSeverity(tx, newSeverity)
	if err != nil {
		log.Printf("postCreateAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertMessageNotifications(tx, time.Now().UTC(), alertMessageID)
	if err != nil {
		log.Printf("postCreateAlert.createAlertMessageNotifications: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postCreateAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

func getEditAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("getEditAlert.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alert, err := getAlertByID(tx, id)
	if err != nil {
		log.Printf("getEditAlert.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getEditAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getEditAlert", getEditAlertMarkup)
	if err != nil {
		log.Printf("getEditAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	checkedServices := map[int]bool{}
	for _, service := range services {
		checkedServices[service.ID] = false
	}
	for _, service := range alert.Services {
		checkedServices[service.ID] = true
	}

	err = tmpl.Execute(w, struct {
		Alert           AlertDetail
		Services        []service
		CheckedServices map[int]bool
		Ctx             pageCtx
	}{
		Alert:           alert,
		Services:        services,
		CheckedServices: checkedServices,
		Ctx:             getPageCtx(r),
	})
	if err != nil {
		log.Printf("getEditAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editAlert(
	tx *sql.Tx,
	id int,
	title string,
	services []int,
	alertType string,
	severity string,
) error {
	const alertQuery = `
		update alert set title = ?, type = ?, severity = ? where id = ?
	`

	_, err := tx.Exec(alertQuery, title, alertType, severity, id)
	if err != nil {
		return fmt.Errorf("editAlert.Exec: %w", err)
	}

	const serviceDeleteQuery = `
		delete from alert_service where alert_id = ?
	`

	_, err = tx.Exec(serviceDeleteQuery, id)
	if err != nil {
		return fmt.Errorf("editAlert.Exec2: %w", err)
	}

	const baseServiceInsertQuery = `
		insert into alert_service(alert_id, service_id) values
	`

	serviceInsertQuery := baseServiceInsertQuery

	params := []any{}

	for i, serviceID := range services {
		serviceInsertQuery += "(?, ?)"
		if i < len(services)-1 {
			serviceInsertQuery += ", "
		}
		params = append(params, id, serviceID)
	}

	_, err = tx.Exec(serviceInsertQuery, params...)
	if err != nil {
		return fmt.Errorf("editAlert.Exec3: %w", err)
	}

	return nil
}

func postEditAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	title := r.PostFormValue("title")
	if title == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	services := []int{}
	for _, service := range r.PostForm["services"] {
		num, err := strconv.Atoi(service)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		services = append(services, num)
	}
	if len(services) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	alertType := r.PostFormValue("type")
	if alertType != "incident" && alertType != "maintenance" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	severity := r.PostFormValue("severity")
	if alertType == "incident" {
		if severity != "red" && severity != "amber" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		alertType = "maintenance"
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editAlert(tx, id, title, services, alertType, severity)
	if err != nil {
		log.Printf("postEditAlert.editAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postEditAlert.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	newSeverity := "blue"
	for _, alert := range alerts {
		if alert.Severity == "amber" {
			newSeverity = "amber"
			continue
		}

		if alert.Severity == "red" {
			newSeverity = "red"
			break
		}
	}

	err = updateSeverity(tx, newSeverity)
	if err != nil {
		log.Printf("postEditAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postEditAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

func resolveAlert(tx *sql.Tx, id int) error {
	const query = `
		update alert set ended_at = ? where id = ?
	`

	_, err := tx.Exec(query, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("resolveAlert.Exec: %w", err)
	}

	return nil
}

func getSeverity(tx *sql.Tx) (string, error) {
	const query = `
		select severity from severity limit 1
	`

	var severity string

	err := tx.QueryRow(query).Scan(&severity)
	if err != nil {
		return severity, fmt.Errorf("getSeverity.QueryRow: %w", err)
	}

	return severity, nil
}

func updateSeverity(tx *sql.Tx, severity string) error {
	const query = `
		update severity set severity = ?
	`

	_, err := tx.Exec(query, severity)
	if err != nil {
		return fmt.Errorf("updateSeverity.Exec: %w", err)
	}

	return nil
}

func postResolveAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postResolveAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = resolveAlert(tx, id)
	if err != nil {
		log.Printf("postResolveAlert.resolveAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postResolveAlert.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	newSeverity := "blue"
	for _, alert := range alerts {
		if alert.Severity == "amber" {
			newSeverity = "amber"
			continue
		}

		if alert.Severity == "red" {
			newSeverity = "red"
			break
		}
	}

	err = updateSeverity(tx, newSeverity)
	if err != nil {
		log.Printf("postResolveAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postResolveAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}

func unresolveAlert(tx *sql.Tx, id int) error {
	const query = `
		update alert set ended_at = null where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("unresolveAlert.Exec: %w", err)
	}

	return nil
}

func postUnresolveAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postUnresolveAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = unresolveAlert(tx, id)
	if err != nil {
		log.Printf("postUnresolveAlert.resolveAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postUnresolveAlert.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	newSeverity := "blue"
	for _, alert := range alerts {
		if alert.Severity == "amber" {
			newSeverity = "amber"
			continue
		}

		if alert.Severity == "red" {
			newSeverity = "red"
			break
		}
	}

	err = updateSeverity(tx, newSeverity)
	if err != nil {
		log.Printf("postUnresolveAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postUnresolveAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}

func getAddAlertMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getAddAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("getAddAlertMessage.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getAddAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getAddAlertMessage", getAddAlertMessageMarkup)
	if err != nil {
		log.Printf("getAddAlertMessage.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert AlertDetail
			Ctx   pageCtx
		}{
			Alert: alert,
			Ctx:   getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getAddAlertMessage.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createAlertMessage(tx *sql.Tx, alertID int, content string) (int, error) {
	const query = `
		insert into
			alert_message(content, created_at, alert_id)
		values(?, ?, ?)
		returning id
	`

	var id int

	err := tx.QueryRow(query, content, time.Now().UTC(), alertID).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createAlertMessage.Scan: %w", err)
	}

	return id, nil
}

func postAddAlertMessage(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postAddAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alertMessageID, err := createAlertMessage(tx, id, message)
	if err != nil {
		log.Printf("postAddAlertMessage.createAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertMessageNotifications(tx, time.Now().UTC(), alertMessageID)
	if err != nil {
		log.Printf("postAddAlertMessage.createAlertMessageNotifications: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postAddAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}

func deleteAlertMessageByID(tx *sql.Tx, alertID int, messageID int) error {
	const query = `
		delete from alert_message where alert_id = ? and id = ?
	`

	_, err := tx.Exec(query, alertID, messageID)
	if err != nil {
		return fmt.Errorf("deleteAlertMessageByID.Exec: %w", err)
	}

	return nil
}

func deleteAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertIDParam := chi.URLParam(r, "id")
	alertID, err := strconv.Atoi(alertIDParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteAlertMessageByID(tx, alertID, messageID)
	if err != nil {
		log.Printf("deleteAlertMessage.deleteAlertMessageByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+alertIDParam)
}

func getEditAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alert, err := getAlertByID(tx, alertID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		log.Printf("getEditAlertMessage.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	message := AlertDetailMessage{}
	for _, msg := range alert.Messages {
		if msg.ID == messageID {
			message = msg
			break
		}
	}

	if message.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}


	tmpl, err := parseTmpl("getEditAlertMessage", getEditAlertMessageMarkup)
	if err != nil {
		log.Printf("getEditAlertMessage.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Alert   AlertDetail
			Message AlertDetailMessage
			Ctx     pageCtx
		}{
			Alert:   alert,
			Message: message,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditAlertMessage.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editAlertMessage(tx *sql.Tx, alertID int, messageID int, content string) error {
	const query = `
		update alert_message 
		set 
			content = ?,
			last_updated_at = ? 
		where 
			alert_id = ? and id = ?
	`

	_, err := tx.Exec(query, content, time.Now().UTC(), alertID, messageID)
	if err != nil {
		return fmt.Errorf("editAlertMessage.Exec: %w", err)
	}

	return nil
}

func postEditAlertMessage(w http.ResponseWriter, r *http.Request) {
	alertIDParam := chi.URLParam(r, "id")

	alertID, err := strconv.Atoi(alertIDParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	messageID, err := strconv.Atoi(chi.URLParam(r, "messageID"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")
	if message == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditAlertMessage.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editAlertMessage(tx, alertID, messageID, message)
	if err != nil {
		log.Printf("postEditAlertMessage.editAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditAlertMessage.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+alertIDParam)
}

type service struct {
	ID         int
	Slug       string
	Name       string
	HelperText string
}

func listServices(tx *sql.Tx) ([]service, error) {
	const query = `
		select 
			id, slug, name, helper_text
		from
			service
	`

	services := []service{}

	rows, err := tx.Query(query)
	if err != nil {
		return services, fmt.Errorf("listServices.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		svc := service{}
		err = rows.Scan(
			&svc.ID,
			&svc.Slug,
			&svc.Name,
			&svc.HelperText,
		)
		if err != nil {
			return services, fmt.Errorf("listServices.Scan: %w", err)
		}

		services = append(services, svc)
	}

	if err := rows.Err(); err != nil {
		return services, fmt.Errorf("listServices.RowsErr: %w", err)
	}

	return services, nil
}

func services(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("services.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("services.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("services.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("services", servicesMarkup)
	if err != nil {
		log.Printf("services.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Services []service
			Ctx      pageCtx
		}{
			Services: services,
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("services.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getCreateService(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getCreateService", getCreateServiceMarkup)
	if err != nil {
		log.Printf("getCreateService.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateService.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createService(tx *sql.Tx, slug string, name string, helperText string) error {
	const query = `
		insert into service(slug, name, helper_text) values(?, ?, ?)
	`

	_, err := tx.Exec(query, slug, name, helperText)
	if err != nil {
		return fmt.Errorf("createService.Exec: %w", err)
	}

	return nil
}

func postCreateService(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	helperText := r.PostFormValue("helper")

	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("postCreateService.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	serviceSlugs := map[string]bool{}
	for _, v := range services {
		serviceSlugs[v.Slug] = true
	}

	err = createService(tx, generateSlug(name, serviceSlugs), name, helperText)
	if err != nil {
		log.Printf("postCreateService.createService: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postCreateService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}

func deleteServiceByID(tx *sql.Tx, id int) error {
	const query = `
		delete from service where id = $1
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteServiceByID.Exec: %w", err)
	}

	return nil
}

func deleteService(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteServiceByID(tx, id)
	if err != nil {
		// Without the return this committed an empty transaction and
		// redirected as though the delete had worked.
		log.Printf("deleteService.deleteServiceByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}

func getServiceByID(tx *sql.Tx, id int) (service, error) {
	const query = `
		select id, name, helper_text from service where id = $1
	`

	service := service{}

	err := tx.QueryRow(query, id).Scan(
		&service.ID,
		&service.Name,
		&service.HelperText,
	)
	if err != nil {
		return service, fmt.Errorf("getServiceByID.Scan: %w", err)
	}

	return service, nil
}

func getEditService(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	svc, err := getServiceByID(tx, id)
	if err != nil {
		log.Printf("getEditService.getServiceByID: %s", err)
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getEditService", getEditServiceMarkup)
	if err != nil {
		log.Printf("getEditService.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Service service
			Ctx     pageCtx
		}{
			Service: svc,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditService.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editService(tx *sql.Tx, id int, name string, helperText string) error {
	const query = `
		update service set name = ?, helper_text = ? where id = ?
	`

	_, err := tx.Exec(query, name, helperText, id)
	if err != nil {
		return fmt.Errorf("editService.Exec: %w", err)
	}

	return nil
}

func updateServiceSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update service set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateServiceSlug.Exec: %w", err)
	}

	return id, nil
}

func postEditService(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	helperText := r.PostFormValue("helper")

	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editService(tx, id, name, helperText)
	if err != nil {
		log.Printf("postEditService.createService: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}

func notifications(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("notifications.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		log.Printf("notifications.listNotificationChannels: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("notifications.listMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("notifications.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("notifications", notificationsMarkup)
	if err != nil {
		log.Printf("notifications.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Notifications []NotificationChannel
			MailGroups    []MailGroup
			Ctx           pageCtx
		}{
			Notifications: channels,
			MailGroups:    mailGroups,
			Ctx:           getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("notifications.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getCreateNotification(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getCreateNotification", getCreateNotificationMarkup)
	if err != nil {
		log.Printf("getCreateNotification.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateNotification.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

type SMTPNotificationDetails struct {
	Host     string            `json:"host"`
	Port     int               `json:"port"`
	Username string            `json:"username"`
	Password string            `json:"password"`
	From     string            `json:"from"`
	Headers  map[string]string `json:"headers"`
	Misc     map[string]string `json:"misc"`
}

type SlackNotificationDetails struct {
	WebhookURL string `json:"webhookURL"`
}

type NotificationChannel struct {
	ID      int
	Slug    string
	Name    string
	Type    string
	Details any
}

type listNotificationsOptions struct {
	Type string
}

func listNotificationChannels(tx *sql.Tx, options listNotificationsOptions) ([]NotificationChannel, error) {
	const baseQuery = `
		select id, slug, name, type, details from notification_channel
	`

	query := baseQuery

	params := []any{}

	if options.Type != "" {
		query += " where type = ?"
		params = append(params, options.Type)
	}

	var channels []NotificationChannel

	rows, err := tx.Query(query, params...)
	if err != nil {
		return channels, fmt.Errorf("listNotificationChannels.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var detailsStr string
		var channel NotificationChannel

		err := rows.Scan(&channel.ID, &channel.Slug, &channel.Name, &channel.Type, &detailsStr)
		if err != nil {
			return channels, fmt.Errorf("listNotificationChannels.Scan: %w", err)
		}

		if channel.Type == "smtp" {
			var details SMTPNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return channels, fmt.Errorf("listNotificationChannels.UnmarshalSMTP: %w", err)
			}

			channel.Details = details
		} else if channel.Type == "slack" {
			var details SlackNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return channels, fmt.Errorf("listNotificationChannels.UnmarshalSlack: %w", err)
			}

			channel.Details = details
		}

		channels = append(channels, channel)
	}

	if err := rows.Err(); err != nil {
		return channels, fmt.Errorf("listNotificationChannels.RowsErr: %w", err)
	}

	return channels, nil
}

func listNotificationChannelsByMonitorID(tx *sql.Tx, monitorID int) ([]NotificationChannel, error) {
	const query = `
		select notification_channel.id, notification_channel.slug, 
		notification_channel.name, notification_channel.type, notification_channel.details 
		from monitor_notification_channel
		left join notification_channel on 
			notification_channel.id = monitor_notification_channel.notification_channel_id
		where monitor_notification_channel.monitor_id = ?
	`

	var notifications []NotificationChannel

	rows, err := tx.Query(query, monitorID)
	if err != nil {
		return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var detailsStr string
		var channel NotificationChannel

		err := rows.Scan(&channel.ID, &channel.Slug, &channel.Name, &channel.Type, &detailsStr)
		if err != nil {
			return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.Scan: %w", err)
		}

		if channel.Type == "smtp" {
			var details SMTPNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.UnmarshalSMTP: %w", err)
			}

			channel.Details = details
		} else if channel.Type == "slack" {
			var details SlackNotificationDetails

			err := json.Unmarshal([]byte(detailsStr), &details)
			if err != nil {
				return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.UnmarshalSlack: %w", err)
			}

			channel.Details = details
		}

		notifications = append(notifications, channel)
	}

	if err := rows.Err(); err != nil {
		return notifications, fmt.Errorf("listNotificationChannelsByMonitorID.RowsErr: %w", err)
	}

	return notifications, nil
}

func createNotification(
	tx *sql.Tx,
	slug string,
	name string,
	notificationType string,
	details string,
) error {
	const query = `
		insert into notification_channel(slug, name, type, details) values(?, ?, ?, ?)
	`

	_, err := tx.Exec(query, slug, name, notificationType, details)
	if err != nil {
		return fmt.Errorf("createNotification.Exec: %w", err)
	}

	return nil
}

func postCreateNotification(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	notificationType := r.PostFormValue("type")
	if notificationType != "smtp" && notificationType != "slack" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	displayName := r.PostFormValue("display-name")
	if displayName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if notificationType == "smtp" {
		host := r.PostFormValue("host")
		if host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		port := r.PostFormValue("port")
		if port == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		portNum, err := strconv.Atoi(port)
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

		from := r.PostFormValue("from")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err = mail.ParseAddress(from)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		headers := map[string]string{}
		if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
			keys := r.PostForm["header-key"]
			values := r.PostForm["header-value"]
			for i := 0; i < len(keys) && i < len(values); i++ {
				headers[keys[i]] = values[i]
			}
		}

		misc := map[string]string{}
		if strings.EqualFold(host, "smtp.postmarkapp.com") {
			txStream := r.PostFormValue("pm-transactional")
			bStream := r.PostFormValue("pm-broadcast")

			if txStream == "" || bStream == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			misc["pm-transactional"] = txStream
			misc["pm-broadcast"] = bStream
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postCreateNotification.BeginSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		details := SMTPNotificationDetails{
			Host:     host,
			Port:     portNum,
			Username: username,
			Password: password,
			From:     from,
			Headers:  headers,
			Misc:     misc,
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		channels, err := listNotificationChannels(tx, listNotificationsOptions{})
		if err != nil {
			log.Printf("postCreateNotification.listNotificationChannelsSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		channelSlugs := map[string]bool{}
		for _, v := range channels {
			channelSlugs[v.Slug] = true
		}

		err = createNotification(
			tx,
			generateSlug(displayName, channelSlugs),
			displayName,
			notificationType,
			string(serializedDetails),
		)
		if err != nil {
			log.Printf("postCreateNotification.createNotificationSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("postCreateNotification.CommitSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if notificationType == "slack" {
		webhookURL, err := url.ParseRequestURI(r.PostFormValue("webhook-url"))
		if err != nil {
			// ParseRequestURI returns a nil URL alongside the error, and the
			// code below calls String() on it.
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postCreateNotification.BeginSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		details := SlackNotificationDetails{
			WebhookURL: webhookURL.String(),
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		channels, err := listNotificationChannels(tx, listNotificationsOptions{})
		if err != nil {
			log.Printf("postCreateNotification.listNotificationChannelsSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		channelSlugs := map[string]bool{}
		for _, v := range channels {
			channelSlugs[v.Slug] = true
		}

		err = createNotification(
			tx,
			generateSlug(displayName, channelSlugs),
			displayName,
			notificationType,
			string(serializedDetails),
		)
		if err != nil {
			log.Printf("postCreateNotification.createNotificationSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("postCreateNotification.CommitSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getNotificationChannelByID(tx *sql.Tx, id int) (NotificationChannel, error) {
	const query = `
		select id, slug, name, type, details from notification_channel
		where id = ?
	`

	var channel NotificationChannel
	var detailsStr string

	err := tx.QueryRow(query, id).Scan(
		&channel.ID,
		&channel.Slug,
		&channel.Name,
		&channel.Type,
		&detailsStr,
	)
	if err != nil {
		return channel, fmt.Errorf("getNotificationChannelByID.QueryRow: %w", err)
	}

	if channel.Type == "smtp" {
		var details SMTPNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelByID.UnmarshalSMTP: %w", err)
		}

		channel.Details = details
	} else if channel.Type == "slack" {
		var details SlackNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelByID.UnmarshalSlack: %w", err)
		}

		channel.Details = details
	}

	return channel, nil
}

func getNotificationChannelBySlug(tx *sql.Tx, slug string) (NotificationChannel, error) {
	const query = `
		select id, slug, name, type, details from notification_channel
		where slug = ?
	`

	var channel NotificationChannel
	var detailsStr string

	err := tx.QueryRow(query, slug).Scan(
		&channel.ID,
		&channel.Slug,
		&channel.Name,
		&channel.Type,
		&detailsStr,
	)
	if err != nil {
		return channel, fmt.Errorf("getNotificationChannelBySlug.QueryRow: %w", err)
	}

	if channel.Type == "smtp" {
		var details SMTPNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelBySlug.UnmarshalSMTP: %w", err)
		}

		channel.Details = details
	} else if channel.Type == "slack" {
		var details SlackNotificationDetails

		err := json.Unmarshal([]byte(detailsStr), &details)
		if err != nil {
			return channel, fmt.Errorf("getNotificationChannelBySlug.UnmarshalSlack: %w", err)
		}

		channel.Details = details
	}

	return channel, nil
}

func getEditNotification(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditNotification.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, id)
	if err != nil {
		log.Printf("getEditNotification.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditNotification.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getEditNotification", getEditNotificationMarkup)
	if err != nil {
		log.Printf("getEditNotification.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	isPostmark := false

	smtpDetail, ok := channel.Details.(SMTPNotificationDetails)
	if ok {
		isPostmark = strings.EqualFold(smtpDetail.Host, "smtp.postmarkapp.com")
	}

	err = tmpl.Execute(
		w,
		struct {
			Notification NotificationChannel
			IsPostmark   bool
			ReadOnly     bool
			Ctx          pageCtx
		}{
			Notification: channel,
			IsPostmark:   isPostmark,
			ReadOnly:     readOnly,
			Ctx:          getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditNotification.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editNotificationChannel(tx *sql.Tx, channel NotificationChannel) error {
	const query = `
		update notification_channel set name = ?, details = ?
		where id = ?
	`

	_, err := tx.Exec(query, channel.Name, channel.Details, channel.ID)
	if err != nil {
		return fmt.Errorf("editNotificationChannel.Exec: %w", err)
	}

	return nil
}

func updateNotificationChannelSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update notification_channel set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateNotificationChannelSlug.QueryRow: %w", err)
	}

	return id, nil
}

func postEditNotification(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	idParam := chi.URLParam(r, "id")
	notificationID, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditNotification.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, notificationID)
	if err != nil {
		log.Printf("postEditNotification.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	displayName := r.PostFormValue("display-name")
	if displayName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if channel.Type == "smtp" {
		host := r.PostFormValue("host")
		if host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		port := r.PostFormValue("port")
		if port == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		portNum, err := strconv.Atoi(port)
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

		from := r.PostFormValue("from")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err = mail.ParseAddress(from)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		headers := map[string]string{}
		if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
			keys := r.PostForm["header-key"]
			values := r.PostForm["header-value"]
			for i := 0; i < len(keys) && i < len(values); i++ {
				headers[keys[i]] = values[i]
			}
		}

		misc := map[string]string{}
		if strings.EqualFold(host, "smtp.postmarkapp.com") {
			txStream := r.PostFormValue("pm-transactional")
			bStream := r.PostFormValue("pm-broadcast")

			if txStream == "" || bStream == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			misc["pm-transactional"] = txStream
			misc["pm-broadcast"] = bStream
		}

		details := SMTPNotificationDetails{
			Host:     host,
			Port:     portNum,
			Username: username,
			Password: password,
			Headers:  headers,
			From:     from,
			Misc:     misc,
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if channel.Type == "slack" {
		webhookURL, err := url.ParseRequestURI(r.PostFormValue("webhook-url"))
		if err != nil {
			// ParseRequestURI returns a nil URL alongside the error, and the
			// code below calls String() on it.
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		details := SlackNotificationDetails{
			WebhookURL: webhookURL.String(),
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditNotification.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getViewNotification(w http.ResponseWriter, r *http.Request) {
	getEditNotification(w, r)
}

func deleteNotificationChannelByID(tx *sql.Tx, id int) error {
	const query = `
		delete from notification_channel where id = $1
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteNotificationChannelByID.Exec: %w", err)
	}

	return nil
}

func deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteNotificationChannel.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteNotificationChannelByID(tx, id)
	if err != nil {
		log.Printf("deleteNotificationChannel.deleteNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteNotificationChannel.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getCreateMailGroup(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getCreateMailGroup", getCreateMailGroupMarkup)
	if err != nil {
		log.Printf("getCreateMailGroup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getEditMailGroup(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	mailGroup, err := getMailGroupByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.getMailGroupByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroupMembers, err := listMailGroupMembersByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.listMailGroupMembersByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getEditMailGroup", getEditMailGroupMarkup)
	if err != nil {
		log.Printf("getEditMailGroup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			MailGroup        MailGroup
			MailGroupMembers []MailGroupMember
			ReadOnly         bool
			Ctx              pageCtx
		}{
			MailGroup:        mailGroup,
			MailGroupMembers: mailGroupMembers,
			ReadOnly:         readOnly,
			Ctx:              getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getViewMailGroup(w http.ResponseWriter, r *http.Request) {
	getEditMailGroup(w, r)
}

type MailGroup struct {
	ID          int
	Slug        string
	Name        string
	Description string
}

func listMailGroups(tx *sql.Tx) ([]MailGroup, error) {
	const query = `
		select id, slug, name, description from mail_group
	`

	mailGroups := []MailGroup{}

	rows, err := tx.Query(query)
	if err != nil {
		return mailGroups, fmt.Errorf("listMailGroups.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		mailGroup := MailGroup{}
		err = rows.Scan(&mailGroup.ID, &mailGroup.Slug, &mailGroup.Name, &mailGroup.Description)
		if err != nil {
			return mailGroups, fmt.Errorf("listMailGroups.Scan: %w", err)
		}

		mailGroups = append(mailGroups, mailGroup)
	}

	if err := rows.Err(); err != nil {
		return mailGroups, fmt.Errorf("listMailGroups.RowsErr: %w", err)
	}

	return mailGroups, nil
}

func updateMonitorMailGroups(tx *sql.Tx, monitorID int, mailGroupIDs []int) error {
	const deleteQuery = `
		delete from mail_group_monitor where monitor_id = ?
	`

	_, err := tx.Exec(deleteQuery, monitorID)
	if err != nil {
		return fmt.Errorf("updateMonitorMailGroups.ExecDelete: %w", err)
	}

	if len(mailGroupIDs) > 0 {
		const baseInsertQuery = `
			insert into mail_group_monitor(mail_group_id, monitor_id) values
		`

		insertQuery := baseInsertQuery

		params := []any{}

		for i, v := range mailGroupIDs {
			insertQuery += "(?, ?)"
			if i < len(mailGroupIDs)-1 {
				insertQuery += ","
			}
			params = append(params, v, monitorID)
		}

		_, err = tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMonitorMailGroups.ExecInsert: %w", err)
		}
	}

	return nil
}

type MailGroupIDs struct {
	ID   int
	Slug string
}

func listMailGroupIDsByMonitorID(tx *sql.Tx, monitorID int) ([]MailGroupIDs, error) {
	const query = `
		select mail_group_id, slug from mail_group_monitor 
		left join mail_group on mail_group.id = mail_group_id
		where monitor_id = ?
	`

	allIds := []MailGroupIDs{}

	rows, err := tx.Query(query, monitorID)
	if err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ids MailGroupIDs
		err = rows.Scan(&ids.ID, &ids.Slug)
		if err != nil {
			return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Scan: %w", err)
		}

		allIds = append(allIds, ids)
	}

	if err := rows.Err(); err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.RowsErr: %w", err)
	}

	return allIds, nil
}

type MailGroupMember struct {
	ID           int
	EmailAddress string
}

func listMailGroupMembersByID(tx *sql.Tx, id int) ([]MailGroupMember, error) {
	const query = `
		select id, email_address from mail_group_member where mail_group_id = ?
	`

	members := []MailGroupMember{}

	rows, err := tx.Query(query, id)
	if err != nil {
		return members, fmt.Errorf("listMailGroupMembersByID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		member := MailGroupMember{}
		err = rows.Scan(&member.ID, &member.EmailAddress)
		if err != nil {
			return members, fmt.Errorf("listMailGroupMembersByID.Scan: %w", err)
		}

		members = append(members, member)
	}

	if err := rows.Err(); err != nil {
		return members, fmt.Errorf("listMailGroupMembersByID.RowsErr: %w", err)
	}

	return members, nil
}

func listMailGroupMembersEmailsByMonitorID(tx *sql.Tx, id int) ([]string, error) {
	const query = `
		select distinct email_address from mail_group_member
		left join mail_group on mail_group.id = mail_group_member.mail_group_id
		left join mail_group_monitor on mail_group_monitor.mail_group_id = mail_group.id
		where mail_group_monitor.monitor_id = ?
	`

	emails := []string{}

	rows, err := tx.Query(query, id)
	if err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var email string
		err = rows.Scan(&email)
		if err != nil {
			return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Scan: %w", err)
		}

		emails = append(emails, email)
	}

	if err := rows.Err(); err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.RowsErr: %w", err)
	}

	return emails, nil
}

func getMailGroupByID(tx *sql.Tx, id int) (MailGroup, error) {
	const query = `
		select id, name, description from mail_group where id = ?
	`

	mailGroup := MailGroup{}

	err := tx.QueryRow(query, id).Scan(&mailGroup.ID, &mailGroup.Name, &mailGroup.Description)
	if err != nil {
		return mailGroup, fmt.Errorf("getMailGroupByID.QueryRow: %w", err)
	}

	return mailGroup, nil
}

func createMailGroup(tx *sql.Tx, slug string, name string, description string) (int, error) {
	const query = `
		insert into mail_group(slug, name, description) values(?, ?, ?) returning id
	`

	var id int

	err := tx.QueryRow(query, slug, name, description).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createMailGroup.QueryRow: %w", err)
	}

	return id, nil
}

func updateMailGroup(tx *sql.Tx, id int, name string, description string) error {
	const query = `
		update mail_group set name = ?, description = ? where id = ?
	`

	_, err := tx.Exec(query, name, description, id)
	if err != nil {
		return fmt.Errorf("updateMailGroup.Exec: %w", err)
	}

	return nil
}

func updateMailGroupSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update mail_group set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateMailGroupSlug.QueryRow: %w", err)
	}

	return id, nil
}

func updateMailGroupMembers(tx *sql.Tx, id int, members []string) error {
	const deleteQuery = `
		delete from mail_group_member where mail_group_id = ?
	`

	_, err := tx.Exec(deleteQuery, id)
	if err != nil {
		return fmt.Errorf("updateMailGroupMembers.ExecDelete: %w", err)
	}

	if len(members) > 0 {
		const baseInsertQuery = `
			insert into mail_group_member(email_address, mail_group_id)
			values
		`

		insertQuery := baseInsertQuery

		for i := range members {
			insertQuery += "(?, ?)"

			if i != len(members)-1 {
				insertQuery += ","
			}
		}

		params := []any{}
		for _, v := range members {
			params = append(params, v, id)
		}

		insertQuery += " on conflict (mail_group_id, email_address) do nothing"

		_, err := tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMailGroupMembers.ExecInsert: %w", err)
		}
	}

	return nil
}

func postCreateMailGroup(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	description := r.PostFormValue("description")

	if r.PostFormValue("members") != "" {
		for _, v := range r.Form["members"] {
			_, err := mail.ParseAddress(v)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("postCreateMailGroup.listMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroupSlugs := map[string]bool{}
	for _, v := range mailGroups {
		mailGroupSlugs[v.Slug] = true
	}

	id, err := createMailGroup(tx, generateSlug(name, mailGroupSlugs), name, description)
	if err != nil {
		log.Printf("postCreateMailGroup.createMailGroup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMailGroupMembers(tx, id, r.Form["members"])
	if err != nil {
		log.Printf("postCreateMailGroup.updateMailGroupMembers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postCreateMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func deleteMailGroupByID(tx *sql.Tx, id int) error {
	const query = `
		delete from mail_group where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteMailGroupByID.Exec: %w", err)
	}

	return nil
}

func deleteMailGroup(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteMailGroupByID(tx, id)
	if err != nil {
		log.Printf("deleteMailGroup.deleteMailGroupByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func postEditMailGroup(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	description := r.PostFormValue("description")

	if r.PostFormValue("members") != "" {
		for _, v := range r.Form["members"] {
			_, err := mail.ParseAddress(v)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMailGroup(tx, id, name, description)
	if err != nil {
		log.Printf("postEditMailGroup.updateMailGroup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMailGroupMembers(tx, id, r.Form["members"])
	if err != nil {
		log.Printf("postEditMailGroup.updateMailGroupMembers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func update(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("update", updateMarkup)
	if err != nil {
		log.Printf("update.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			CurrentVersion string
			Ctx            pageCtx
		}{
			CurrentVersion: VERSION,
			Ctx:            getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("update.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func updateCheck(w http.ResponseWriter, r *http.Request) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/goksan/statusnook/releases/latest", nil,
	)
	if err != nil {
		log.Printf("updateCheck.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("updateCheck.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("updateCheck.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// A 403 rate-limit body unmarshals cleanly into an empty release, whose
	// blank tag semver.Compare ranks below any real version -- so a failed
	// check rendered as "Statusnook is up to date".
	if resp.StatusCode != http.StatusOK {
		log.Printf("updateCheck.StatusCode %d: %s", resp.StatusCode, string(body))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type GitHubReleaseAsset struct {
		Name string `json:"name"`
	}

	type GitHubRelease struct {
		TagName     string               `json:"tag_name"`
		Assets      []GitHubReleaseAsset `json:"assets"`
		Body        string               `json:"body"`
		PublishedAt time.Time            `json:"published_at"`
	}

	latestRelease := GitHubRelease{}

	err = json.Unmarshal(body, &latestRelease)
	if err != nil {
		log.Printf("updateCheck.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	updateAvailable := semver.Compare(latestRelease.TagName, VERSION) > 0
	latestVersion := latestRelease.TagName


	tmpl, err := parseTmpl("updateCheck", updateCheckMarkup)
	if err != nil {
		log.Printf("updateCheck.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			UpdateAvailable bool
			LatestVersion   string
			PublishedAt     string
			UpdateBody      string
			Docker          bool
			Ctx             pageCtx
		}{

			UpdateAvailable: updateAvailable,
			LatestVersion:   latestVersion,
			PublishedAt:     latestRelease.PublishedAt.Format("2006/01/02"),
			UpdateBody:      latestRelease.Body,
			Docker:          *dockerFlag,
			Ctx:             getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("updateCheck.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func afterUpdate(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("afterUpdate", afterUpdateMarkup)
	if err != nil {
		log.Printf("afterUpdate.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Version string
			Ctx     pageCtx
		}{
			Version: VERSION,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("afterUpdate.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postUpdate(w http.ResponseWriter, r *http.Request) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/goksan/statusnook/releases/latest",
		nil,
	)
	if err != nil {
		log.Printf("postUpdate.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("postUpdate.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("postUpdate.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("postUpdate.StatusCode %d: %s", resp.StatusCode, string(body))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type GitHubReleaseAsset struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}

	type GitHubRelease struct {
		TagName string               `json:"tag_name"`
		Assets  []GitHubReleaseAsset `json:"assets"`
	}

	latestRelease := GitHubRelease{}

	err = json.Unmarshal(body, &latestRelease)
	if err != nil {
		log.Printf("postUpdate.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	updateAvailable := semver.Compare(latestRelease.TagName, VERSION) > 0
	latestVersion := latestRelease.TagName

	if !updateAvailable {
		return
	}

	downloadURL := ""

	for _, asset := range latestRelease.Assets {
		if strings.Contains(asset.Name, runtime.GOOS+"_"+runtime.GOARCH) {
			downloadURL = asset.URL
		}
	}

	if downloadURL == "" {
		log.Printf("postUpdate.downloadURL: no download URL")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	downloadReq, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		log.Printf("postUpdate.NewRequestDownload: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	downloadReq.Header.Add("Accept", "application/octet-stream")

	// Its own client: httpClient's 10s Timeout is a whole-request deadline
	// that covers the body read, so it aborted the copy partway through a
	// multi-megabyte binary on any link slower than ~2 MB/s.
	downloadClient := http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}

	resp, err = downloadClient.Do(downloadReq)
	if err != nil {
		log.Printf("postUpdate.DoDownload: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// The status was never checked, so a 403 rate-limit body was written over
	// the binary, chmod 0700, and the process then SIGINT'd itself into a
	// restart loop on a JSON file.
	if resp.StatusCode != http.StatusOK {
		log.Printf("postUpdate.DownloadStatusCode: %d", resp.StatusCode)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("postUpdate.Executable: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Download beside the binary, then rename over it. os.Remove first left
	// nothing to fall back to the moment anything after it failed, and rename
	// within a directory is atomic.
	tmpPath := exePath + ".new"

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		log.Printf("postUpdate.Create: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		file.Close()
		os.Remove(tmpPath)
		log.Printf("postUpdate.Copy: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Close: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Rename: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("postUpdate", postUpdateMarkup)
	if err != nil {
		log.Printf("postUpdate.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	svg, err := staticFS.ReadFile("static/images/statusnook.svg")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx             pageCtx
			Svg             template.HTML
			Version         string
			UpdateAvailable bool
			LatestVersion   string
		}{

			Ctx:             getPageCtx(r),
			Svg:             template.HTML(svg),
			UpdateAvailable: updateAvailable,
			LatestVersion:   latestVersion,
		},
	)
	if err != nil {
		log.Printf("postUpdate.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	syscall.Kill(syscall.Getpid(), syscall.SIGINT)
}

func getSettings(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") != ""

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getSettings.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	users, err := listUsers(tx)
	if err != nil {
		log.Printf("getSettings.listUsers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	invitations, err := listActiveUserInvitations(tx, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		log.Printf("getSettings.listActiveUserInvitations: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configFileEnabledStr, err := getMetaValue(tx, "configFileEnabled")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueConfigFileEnabled: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configFileEnabled := false
	if configFileEnabledStr != "" {
		configFileEnabled, err = strconv.ParseBool(configFileEnabledStr)
		if err != nil {
			log.Printf("getSettings.ParseBoolConfigFileEnabled: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	configFile := ""
	if configFileEnabled {
		cfg, err := getMetaValue(tx, "configFile")
		if err != nil {
			log.Printf("getSettings.getMetaValueConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		configFile = cfg
	}

	githubManagedConfigStr, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubManagedConfig := false
	if githubManagedConfigStr != "" {
		githubManagedConfig, err = strconv.ParseBool(githubManagedConfigStr)
		if err != nil {
			log.Printf("getSettings.ParseBoolGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubConfigSHA := ""
	githubRepoURL := ""
	githubConfigPath := ""
	githubConfigBranch := ""
	githubConfigErrors := []string{}
	if githubManagedConfig {
		githubConfigSHA, err = getMetaValue(tx, "githubConfigSHA")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("getSettings.getMetaValueGitHubConfigSHA: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if githubConfigSHA != "" {
			githubConfigSHA = githubConfigSHA[0:7]
		}

		githubRepoURL, err = getMetaValue(tx, "githubRepoURL")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubRepoURL: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		githubConfigPath, err = getMetaValue(tx, "githubConfigPath")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubConfigPath: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		githubConfigBranch, err = getMetaValue(tx, "githubConfigBranch")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubConfigBranch: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubConfigErrorsStr, err := getMetaValue(tx, "githubConfigErrors")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueGitHubConfigErrors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if githubConfigErrorsStr != "" {
		err = json.Unmarshal([]byte(githubConfigErrorsStr), &githubConfigErrors)
		if err != nil {
			log.Printf("getSettings.UnmarshalGitHubConfigErrors: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getSettings.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedInvitation struct {
		ID        int
		Token     string
		ExpiresIn string
	}

	formattedInvitations := make([]FormattedInvitation, 0, len(invitations))

	for _, invitation := range invitations {
		expiresIn := invitation.CreatedAt.Add(time.Hour * 24).Sub(time.Now().UTC())
		h := int(expiresIn.Truncate(time.Hour).Hours())
		m := int(expiresIn.Truncate(time.Minute).Minutes()) - (h * 60)
		formattedInvitations = append(
			formattedInvitations,
			FormattedInvitation{
				ID:        invitation.ID,
				Token:     invitation.Token,
				ExpiresIn: fmt.Sprintf("%dh %dm", h, m),
			},
		)
	}


	tmpl, err := parseTmpl("getSettings", getSettingsMarkup)
	if err != nil {
		log.Printf("getSettings.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			CurrentVersion      string
			Domain              string
			Users               []SettingsUser
			Invitations         []FormattedInvitation
			Refresh             bool
			ConfigFileEnabled   bool
			ConfigFile          string
			GitHubManagedConfig bool
			GitHubConfigSHA     string
			GitHubCommitLink    string
			GitHubConfigErrors  []string
			Ctx                 pageCtx
		}{
			CurrentVersion:      VERSION,
			Domain:              metaDomain.Load(),
			Users:               users,
			Invitations:         formattedInvitations,
			Refresh:             refresh,
			ConfigFileEnabled:   configFileEnabled,
			ConfigFile:          configFile,
			GitHubManagedConfig: githubManagedConfig,
			GitHubConfigSHA:     githubConfigSHA,
			GitHubCommitLink: githubRepoURL + "/blob/" + githubConfigBranch + "/" +
				githubConfigPath,
			GitHubConfigErrors: githubConfigErrors,
			Ctx:                getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getSettings.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSettings(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	domain := strings.ToLower(r.PostFormValue("domain"))

	if name != "" {
		if metaConfigFileEnabled.Load() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postSettings.BeginName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()

		err = updateMetaValue(tx, "name", name)
		if err != nil {
			log.Printf("postSettings.updateMetaValueName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("postSettings.CommitName: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		metaName.Store(name)

		escapedName := html.EscapeString(metaName.Load())

		w.Write([]byte(
			fmt.Sprintf(`
				<input id="name" name="name" value="%s" hx-swap-oob="true" disabled>
				<a id="nook-name" href="/" hx-boost="true" hx-swap-oob="true">%s</a>
			`,
				escapedName,
				escapedName,
			),
		))

		return
	}

	if domain != "" {
		if metaSSL.Load() != "true" {
			tx, err := rwDB.Begin()
			if err != nil {
				log.Printf("postSettings.BeginUnmanagedDomain: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}
			defer tx.Rollback()

			err = updateMetaValue(tx, "domain", domain)
			if err != nil {
				log.Printf("postSettings.updateMetaValueDomainUnmanaged: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			err = tx.Commit()
			if err != nil {
				log.Printf("postSettings.CommitUnmanagedDomain: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			metaDomain.Store(domain)

			w.Header().Add("HX-Location", "/admin/settings")
			return
		}

		if metaDomain.Load() == "" {
			tx, err := rwDB.Begin()
			if err != nil {
				log.Printf("postSettings.BeginUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}
			defer tx.Rollback()

			err = updateMetaValue(tx, "unconfirmedDomain", domain)
			if err != nil {
				log.Printf("postSettings.updateMetaValueUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			if err := tx.Commit(); err != nil {
				log.Printf("postSettings.CommitUnconfirmedDomainUpdate: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(
					`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
				))
				return
			}

			metaUnconfirmedDomain.Store(domain)
		}

		domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

		if strings.Contains(domain, "/") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					It looks like you've entered a URL, please enter a domain
				</div>
			`))
			return
		}

		if net.ParseIP(domain).String() != "<nil>" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					It looks like you've entered an IP address, please enter a domain
				</div>
			`))
			return
		}

		if !domainPattern.MatchString(domain) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="banner" class="banner" hx-swap-oob="true">
					Invalid domain
				</div>
			`))
			return
		}

		found, err := lookupDomain(domain)
		if err != nil {
			log.Printf("postSettings.lookupDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		if !found {
			notFoundMsg := "We didn't find your domain's A record, verify it exists and then retry. " +
				"If your domain and A record is correct, you might need to wait a few minutes before retrying."

			if metaDomain.Load() == "" {
				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("postSettings.BeginUnconfirmedDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}
				defer tx.Rollback()

				err = updateMetaValue(tx, "unconfirmedDomainProblem", notFoundMsg)
				if err != nil {
					log.Printf("postSettings.updateMetaValueDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("postSettings.CommitUnconfirmedDomainProblemNotFound: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}

				metaUnconfirmedDomainProblem.Store(notFoundMsg)
			}

			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(
				fmt.Sprintf(`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>%s</span>
					</div>
				`,
					notFoundMsg,
				),
			))
			return
		}

		err = attemptCertificateAcquisition(r.Context(), domain)
		if err != nil {
			errMsg := "An unexpected error occurred"

			var acmeProblem acme.Problem
			if errors.As(err, &acmeProblem) {
				var ok bool
				errMsg, ok = acmeProblemTypeMessages[acmeProblem.Type]
				if !ok {
					errMsg = "An unhandled error occurred " +
						acmeProblem.Type
				}
			} else {
				log.Printf("postSettings.attemptCertificateAcquisition: %s", err)
			}

			if metaDomain.Load() == "" {
				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("postSettings.BeginUnconfirmedDomainProblem: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unexpected error occurred</span>
						</div>
					`,
					))
					return
				}
				defer tx.Rollback()

				err = updateMetaValue(tx, "unconfirmedDomainProblem", errMsg)
				if err != nil {
					log.Printf("postSettings.updateMetaValueDomainProblem: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unhandled error occurred</span>
						</div>
					`,
					))
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("postSettings.CommitUnconfirmedDomainProblem %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(
						`
						<div id="banner" class="banner" hx-swap-oob="true">
							<span>An unhandled error occurred</span>
						</div>
					`,
					))
					return
				}

				metaUnconfirmedDomainProblem.Store(errMsg)
			}

			if errMsg == "An unexpected error occurred" {
				w.WriteHeader(http.StatusInternalServerError)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}

			w.Write([]byte(
				fmt.Sprintf(`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>%s</span>
					</div>
				`,
					errMsg,
				),
			))
			return
		}

		tx, err := rwDB.Begin()
		if err != nil {
			log.Printf("postSettings.BeginDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}
		defer tx.Rollback()

		err = updateMetaValue(tx, "domain", domain)
		if err != nil {
			log.Printf("postSettings.updateMetaValueDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		err = updateMetaValue(tx, "unconfirmedDomain", "")
		if err != nil {
			log.Printf("postSettings.updateMetaValueUnconfirmedDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		err = updateMetaValue(tx, "unconfirmedDomainProblem", "")
		if err != nil {
			log.Printf("postSettings.updateMetaValueUnconfirmedDomainProblem: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unexpected error occurred</span>
					</div>
				`,
			))
			return
		}

		if err := tx.Commit(); err != nil {
			log.Printf("postSettings.CommitDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`
					<div id="banner" class="banner" hx-swap-oob="true">
						<span>An unhandled error occurred</span>
					</div>
				`,
			))
			return
		}

		metaDomain.Store(domain)
		metaUnconfirmedDomain.Store("")
		metaUnconfirmedDomainProblem.Store("")

		w.Header().Add("HX-Location", "/admin/settings")
	}
}

func postSettingsCancelDomain(w http.ResponseWriter, r *http.Request) {
	v := "You cancelled the domain verification process. Please enter a new domain."

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "unconfirmedDomainProblem", v)
	if err != nil {
		log.Printf("postSettingsCancelDomain.updateMetaValueProblem: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaUnconfirmedDomainProblem.Store(v)

	w.Header().Add("HX-Location", "/admin/settings")
}

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

func getConfigSettings(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("getConfigSettings.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	configFileStr, err := getMetaValue(tx, "configFileEnabled")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueConfigFileEnabled: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	configFile := false
	if configFileStr != "" {
		configFile, err = strconv.ParseBool(configFileStr)
		if err != nil {
			log.Printf("getConfigSettings.ParseBoolConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubManagedConfigStr, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	githubManagedConfig := false
	if githubManagedConfigStr != "" {
		githubManagedConfig, err = strconv.ParseBool(githubManagedConfigStr)
		if err != nil {
			log.Printf("getConfigSettings.ParseBoolGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubRepoURL, err := getMetaValue(tx, "githubRepoURL")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubRepoURL %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubBranch, err := getMetaValue(tx, "githubConfigBranch")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubConfigBranch %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubFilePath, err := getMetaValue(tx, "githubConfigPath")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubConfigPath %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubToken, err := getMetaValue(tx, "githubConfigToken")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubConfigToken %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubWebhookSecret, err := getMetaValue(tx, "githubConfigWebhookSecret")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getConfigSettings.getMetaValueGitHubConfigWebhookSecret %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getConfigSettings.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}


	tmpl, err := parseTmpl("getConfigSettings", getConfigSettingsMarkup)
	if err != nil {
		log.Printf("getConfigSettings.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, struct {
		ConfigFile          bool
		GitHubManagedConfig bool
		GitHubRepoURL       string
		GitHubConfigBranch  string
		GitHubConfigPath    string
		GitHubToken         string
		GitHubWebhookSecret string
		Domain              string
		Ctx                 pageCtx
	}{
		ConfigFile:          configFile,
		GitHubManagedConfig: githubManagedConfig,
		GitHubRepoURL:       githubRepoURL,
		GitHubConfigBranch:  githubBranch,
		GitHubConfigPath:    githubFilePath,
		GitHubToken:         githubToken,
		GitHubWebhookSecret: githubWebhookSecret,
		Domain:              metaDomain.Load(),
		Ctx:                 getPageCtx(r),
	})
	if err != nil {
		log.Printf("getConfigSettings.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postConfigSettings(w http.ResponseWriter, r *http.Request) {
	configFile := r.PostFormValue("config-file") == "on"
	githubManaged := r.PostFormValue("github-managed") == "on"

	githubRepoURL := strings.TrimSuffix(r.PostFormValue("github-repo-url"), "/")
	githubBranch := r.PostFormValue("github-branch")
	githubConfigPath := r.PostFormValue("github-config-path")
	githubToken := r.PostFormValue("github-token")
	githubWebhookSecret := r.PostFormValue("github-webhook-secret")

	if !configFile && githubManaged {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if githubManaged {
		if githubRepoURL == "" || githubBranch == "" || githubConfigPath == "" ||
			githubToken == "" || githubWebhookSecret == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postConfigSettings.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "configFileEnabled", strconv.FormatBool(configFile))
	if err != nil {
		log.Printf("postConfigSettings.updateMetaValueConfigFileEnabled: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "githubManagedConfig", strconv.FormatBool(githubManaged))
	if err != nil {
		log.Printf("postConfigSettings.updateMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if githubManaged {
		parsedRepoURL, err := url.Parse(githubRepoURL)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div id="alert" class="alert" hx-swap-oob="true">Invalid repo url</div>`))
			return
		}

		if parsedRepoURL.Path == "" {
			// WriteHeader has to precede Write; the other order sent 200 and
			// logged "superfluous response.WriteHeader call".
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="alert" class="alert" hx-swap-oob="true">
					Invalid GitHub repository URL
				</div>`,
			))
			return
		}

		repoPath := parsedRepoURL.Path[1:]

		httpClient := http.Client{
			Timeout: time.Second * 10,
		}

		req, err := http.NewRequest(
			http.MethodGet,
			"https://api.github.com/repos/"+repoPath,
			nil,
		)
		if err != nil {
			log.Printf("postConfigSettings.NewRequestRepo: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`<div id="alert" class="alert" hx-swap-oob="true">An unexpected error occurred</div>`,
			))
			return
		}
		req.Header.Add("Accept", "application/vnd.github+json")
		req.Header.Add("Authorization", "Bearer "+githubToken)

		resp, err := httpClient.Do(req)
		if err != nil {
			log.Printf("postConfigSettings.DoRepo: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`<div id="alert" class="alert" hx-swap-oob="true">An unexpected error occurred</div>`,
			))
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("postConfigSettings.ReadAllNon200Repo: %s", err)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`<div id="alert" class="alert" hx-swap-oob="true">An error occurred when checking for your config</div>`))
			return
		}

		if resp.StatusCode != 200 {
			if string(respBody) != "" {
				log.Printf("postConfigSettings.StatusCodeRepo: %s", string(respBody))
			}

			w.WriteHeader(http.StatusBadRequest)
			if resp.StatusCode == 404 {
				w.Write([]byte(`
					<div id="alert" class="alert" hx-swap-oob="true">
						Your GitHub repository could not be found.
						Please double-check your repository URL and token permissions, then try again
					</div>`,
				))
			} else if resp.StatusCode == 401 {
				w.Write([]byte(
					`<div id="alert" class="alert" hx-swap-oob="true">
						There's an issue with your personal access token. 
						Please double-check your personal access token, then try again
					</div>`,
				))

			}
			return
		}

		req, err = http.NewRequest(
			http.MethodGet,
			"https://api.github.com/repos/"+path.Join(repoPath, "contents", githubConfigPath)+"?ref="+
				githubBranch,
			nil,
		)
		if err != nil {
			log.Printf("postConfigSettings.NewRequestConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`<div id="alert" class="alert" hx-swap-oob="true">An unexpected error occurred</div>`,
			))
			return
		}
		req.Header.Add("Accept", "application/vnd.github+json")
		req.Header.Add("Authorization", "Bearer "+githubToken)

		resp, err = httpClient.Do(req)
		if err != nil {
			log.Printf("postConfigSettings.DoConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`<div id="alert" class="alert" hx-swap-oob="true">An unexpected error occurred</div>`,
			))
			return
		}
		defer resp.Body.Close()

		respBody, err = io.ReadAll(r.Body)
		if err != nil {
			log.Printf("postConfigSettings.ReadAllNon200Config: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(
				`<div id="alert" class="alert" hx-swap-oob="true">
					An unexpected error occurred
				</div>`,
			))
			return
		}

		if resp.StatusCode != 200 {
			if string(respBody) != "" {
				log.Printf("postConfigSettings.StatusCodeConfig: %s", string(respBody))
			}

			w.WriteHeader(http.StatusBadRequest)
			if resp.StatusCode == 404 {
				w.Write([]byte(`
					<div id="alert" class="alert" hx-swap-oob="true">
						Your Statusnook configuration could not be found.
						Please double-check the path and branch, then try again.
					</div>`,
				))
			} else {
				w.Write([]byte(`
					<div id="alert" class="alert" hx-swap-oob="true">
						Your Statusnook configuration could not be found. An unexpcted error occurred.
					</div>`,
				))
			}
			return
		}

		err = updateMetaValue(tx, "githubRepoURL", githubRepoURL)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigBranch: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigBranch", githubBranch)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigBranch: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigPath", githubConfigPath)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigPath: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigToken", githubToken)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigToken: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigWebhookSecret", githubWebhookSecret)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigWebhookSecret: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if !githubManaged {
		err = updateMetaValue(tx, "githubConfigSHA", "")
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueGitHubConfigSHA: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if !metaConfigFileEnabled.Load() && configFile {
		cfg, err := generateConfig(tx)
		if err != nil {
			log.Printf("postConfigSettings.generateConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "configFile", cfg)
		if err != nil {
			log.Printf("postConfigSettings.updateMetaValueConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postConfigSettings.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaConfigFileEnabled.Store(configFile)

	w.Header().Add("HX-Location", "/admin/settings")
}

func postGenerateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	tokenBytes := make([]byte, 32)
	_, err := rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postGenerateWebhookSecret.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)

	w.Write(
		[]byte(fmt.Sprintf(
			`<input 
				id="github-webhook-secret"
				name="github-webhook-secret"
				type="password"
				readonly="true"
				value="%s"
				hx-swap-oob="true"
			>
			
			<script>
				document.getElementById("generate-new-webhook-secret").close();
			</script>`,
			token,
		)),
	)
}

func configWebhook(w http.ResponseWriter, r *http.Request) {
	// Public route, and the whole body is buffered before the signature is
	// checked. GitHub caps webhook payloads at 25 MB; without a cap here an
	// unauthenticated client streams until the 30s read timeout, repeatedly.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	tx, err := db.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	githubManagedConfig, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if githubManagedConfig != "true" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sig = strings.TrimPrefix(sig, "sha256=")

	key, err := getMetaValue(tx, "githubConfigWebhookSecret")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubWebhookSecret: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	repoURL, err := getMetaValue(tx, "githubRepoURL")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubRepoURL: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	branch, err := getMetaValue(tx, "githubConfigBranch")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigBranch: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configPath, err := getMetaValue(tx, "githubConfigPath")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigPath: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token, err := getMetaValue(tx, "githubConfigToken")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configSHA, err := getMetaValue(tx, "githubConfigSHA")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("configWebhook.getMetaValueGitHubConfigSHA: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("configWebhook.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	payloadMac := mac.Sum(nil)

	headerMac, err := hex.DecodeString(sig)
	if err != nil {
		log.Printf("configWebhook.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !hmac.Equal(headerMac, payloadMac) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("configWebhook.CommitRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	parsedRepoURL, err := url.Parse(repoURL)
	if err != nil {
		log.Printf("configWebhook.Parse: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	repoPath := parsedRepoURL.Path[1:]

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/"+path.Join(repoPath, "contents", configPath)+"?ref="+branch,
		nil,
	)
	if err != nil {
		log.Printf("configWebhook.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header.Add("Accept", "application/vnd.github+json")
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("configWebhook.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// resp, not r: the request body was drained long ago, so reading it
		// here logged an empty string and threw away GitHub's reason.
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("configWebhook.ReadAllNon200: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if string(respBody) != "" {
			log.Printf("configWebhook.StatusCode %d: %s", resp.StatusCode, string(respBody))
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type repositoryContentResponse struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}

	var contentResp repositoryContentResponse

	jsonDecoder := json.NewDecoder(resp.Body)
	err = jsonDecoder.Decode(&contentResp)
	if err != nil {
		log.Printf("configWebhook.Decode: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	content, err := base64.StdEncoding.DecodeString(contentResp.Content)
	if err != nil {
		log.Printf("configWebhook.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err = rwDB.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginWrite: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if contentResp.SHA != configSHA {
		msgs, err := applyConfig(tx, content)
		if err != nil {
			unwrappedErr := errors.Unwrap(err)
			if unwrappedErr == nil || !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
				log.Printf("configWebhook.applyConfig: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			msgs = append(msgs, strings.TrimPrefix(unwrappedErr.Error(), "yaml: "))
		}

		configErrors := ""

		if len(msgs) > 0 {
			msgsBytes, err := json.Marshal(msgs)
			if err != nil {
				log.Printf("configWebhook.Marshal: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			configErrors = string(msgsBytes)

			// applyConfig writes as it validates, deletes included, so by the
			// time one bad monitor produces a message the services, channels
			// and monitors the file no longer mentions are already gone. Throw
			// the whole apply away and record only the errors, the way the
			// admin editor path does. githubConfigSHA deliberately stays
			// unchanged so a corrected push re-applies instead of being
			// skipped as already-seen.
			if err := tx.Rollback(); err != nil {
				log.Printf("configWebhook.RollbackInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			errTx, err := rwDB.Begin()
			if err != nil {
				log.Printf("configWebhook.BeginInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer errTx.Rollback()

			if err := updateMetaValue(errTx, "githubConfigErrors", configErrors); err != nil {
				log.Printf("configWebhook.updateMetaValueGitHubConfigErrorsInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if err := updateMetaValue(errTx, "configFile", string(content)); err != nil {
				log.Printf("configWebhook.updateMetaValueConfigFileInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if err := errTx.Commit(); err != nil {
				log.Printf("configWebhook.CommitInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			// 422 rather than 200: GitHub records the response in the webhook
			// delivery log, which is the only place a pusher looks.
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write(msgsBytes)
			return
		}

		err = updateMetaValue(tx, "githubConfigErrors", string(configErrors))
		if err != nil {
			log.Printf("configWebhook.updateMetaValueGitHubConfigErrors: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "configFile", string(content))
		if err != nil {
			log.Printf("configWebhook.updateMetaValueConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigSHA", contentResp.SHA)
		if err != nil {
			log.Printf("configWebhook.updateMetaValueGitHubConfigSHA: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// log.Fatalf here let any DB error on a webhook GitHub delivers kill
		// the process outright, abandoning the open write transaction.
		log.Printf("configWebhook.getMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("configWebhook.CommitWrite: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)
}

func postConfig(w http.ResponseWriter, r *http.Request) {
	config := r.PostFormValue("config")

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postConfig.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	githubManagedConfigStr, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postConfig.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubManagedConfig := false
	if githubManagedConfigStr != "" {
		githubManagedConfig, err = strconv.ParseBool(githubManagedConfigStr)
		if err != nil {
			log.Printf("postConfig.ParseBoolGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if githubManagedConfig {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msgs, err := applyConfig(tx, []byte(config))
	if err != nil {
		unwrappedErr := errors.Unwrap(err)

		if unwrappedErr == nil || !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
			log.Printf("postConfig.applyConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		formattedErr := strings.TrimPrefix(unwrappedErr.Error(), "yaml: ")

		errMsg := fmt.Sprintf(
			`<div class="save-overlay save-overlay--error">%s</div>`,
			html.EscapeString(formattedErr),
		)
		w.WriteHeader(http.StatusBadRequest)
		w.Write(
			[]byte(fmt.Sprintf(
				`<div 
					id="save-overlay-errors"
					class="save-overlay-errors"
					hx-swap-oob="true"
				>
					%s
				</div>`,
				errMsg,
			)),
		)
		return
	}

	if len(msgs) > 0 {
		errors := ""
		for _, v := range msgs {
			errors += fmt.Sprintf(
				`<div class="save-overlay save-overlay--error">%s</div>`,
				html.EscapeString(v),
			)
		}

		w.WriteHeader(http.StatusBadRequest)
		w.Write(
			[]byte(fmt.Sprintf(
				`<div id="save-overlay-errors" class="save-overlay-errors" hx-swap-oob="true">%s</div>`,
				errors,
			)),
		)
		return
	}

	err = updateMetaValue(tx, "githubConfigErrors", "")
	if err != nil {
		log.Printf("postConfig.updateMetaValueGitHubConfigErrors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postConfig.getMetaValueSetupName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postConfig.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)

	w.Write(
		[]byte(
			fmt.Sprintf(`
			<div id="save-overlay-errors" class="save-overlay-errors" hx-swap-oob="true"></div>
			<div id="update-config" hx-swap-oob="true">
				<script>
					document.querySelector("#save-overlay").style.display = "none";
					window.configFile = "%s";
				</script>
			</div>
			`,
				template.JSEscapeString(config),
			),
		),
	)
}

func postSecret(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	if action != "encrypt" && action != "decrypt" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	input := r.PostFormValue("input")
	if input == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSecret.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	key, err := getMetaValue(tx, "secretKey")
	if err != nil {
		log.Printf("postSecret.getMetaValue: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSecret.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		log.Printf("postSecret.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		log.Printf("postSecret.NewCipher: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		log.Printf("postSecret.NewGCM: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if action == "encrypt" {
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			log.Printf("postSecret.ReadFull: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ciphertext := aesGCM.Seal(nil, nonce, []byte(input), nil)

		b64Ciphertext := base64.StdEncoding.EncodeToString(ciphertext) + "." +
			base64.StdEncoding.EncodeToString(nonce)

		w.Write(
			[]byte(fmt.Sprintf(
				`<input id="output" placeholder="Output" value="%s" hx-swap-oob="true" disabled>`,
				"secret_"+html.EscapeString(b64Ciphertext),
			)),
		)
	} else if action == "decrypt" {
		nonceSplit := strings.Split(input, ".")
		if len(nonceSplit) != 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ciphertext, err := base64.StdEncoding.DecodeString(
			strings.TrimPrefix(nonceSplit[0], "secret_"),
		)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		nonce, err := base64.StdEncoding.DecodeString(nonceSplit[1])
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Write(
			[]byte(fmt.Sprintf(
				`<input id="output" placeholder="Output" value="%s" hx-swap-oob="true" disabled>`,
				html.EscapeString(string(plaintext)),
			)),
		)
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

func postSetupStatusnook(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("X-Statusnook-Setup", "true")
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Expose-Headers", "X-Statusnook-Setup")
}

func getSetupDomain(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupDomain", getSetupDomainMarkup)
	if err != nil {
		log.Printf("getSetupDomain.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	prefillURLText := ""

	prefillURL, err := url.ParseRequestURI("http://" + r.Host)
	if err != nil {
		log.Printf("getSetupDomain.ParseRequestURI: %s", err)
		return
	}

	prefillIP := net.ParseIP(prefillURL.Hostname())
	if prefillIP.String() == "<nil>" {
		prefillURLText = prefillURL.Hostname()
	}

	dev := "false"
	if BUILD == "dev" {
		dev = "true"
	}

	err = tmpl.Execute(
		w,
		struct {
			DEV            template.JS
			SSL            string
			PrefillURLText string
			Ctx            map[string]string
		}{
			DEV:            template.JS(dev),
			SSL:            metaSSL.Load(),
			PrefillURLText: prefillURLText,
			Ctx:            map[string]string{},
		},
	)
	if err != nil {
		log.Printf("getSetupDomain.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// randomNS picks one NS record from an authority section. The section is not
// guaranteed to hold any: an authoritative NOERROR answer carries none, and a
// NODATA answer carries an SOA. Indexing it blind panicked twice over --
// mathRand.Intn(0), and an unchecked assertion on *dns.SOA -- and one caller
// is monitorUnconfirmedDomainLoop, a bare goroutine where a panic takes the
// process with it.
func randomNS(section []dns.RR) (string, bool) {
	nsRecords := []*dns.NS{}
	for _, rr := range section {
		if ns, ok := rr.(*dns.NS); ok {
			nsRecords = append(nsRecords, ns)
		}
	}

	if len(nsRecords) == 0 {
		return "", false
	}

	return nsRecords[mathRand.Intn(len(nsRecords))].Ns, true
}

func lookupDomain(domain string) (bool, error) {
	rootServers := []string{
		"a.root-servers.net",
		"b.root-servers.net",
		"c.root-servers.net",
		"d.root-servers.net",
		"e.root-servers.net",
		"f.root-servers.net",
		"g.root-servers.net",
		"h.root-servers.net",
		"i.root-servers.net",
		"j.root-servers.net",
		"k.root-servers.net",
		"l.root-servers.net",
		"m.root-servers.net",
	}

	rootNS := rootServers[mathRand.Intn(len(rootServers))]
	c := &dns.Client{}
	m := &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err := c.Exchange(m, rootNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.rootNS %s %s: %w", domain, rootNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	authorityNS, ok := randomNS(r.Ns)
	if !ok {
		return false, nil
	}
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, authorityNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.authorityNS %s %s: %w", domain, authorityNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	domainNS, ok := randomNS(r.Ns)
	if !ok {
		return false, nil
	}
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, domainNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.domainNS %s %s: %w", domain, domainNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	return len(r.Answer) > 0, nil
}

var acmeProblemTypeMessages = map[string]string{
	acme.ProblemTypeDNS:                "Let's Encrypt can't find your domain's DNS record, verify it exists and then retry",
	acme.ProblemTypeConnection:         "Let's Encrypt could not reach your server, ensure your server is publicly accessible on ports 80 and 443, then try again",
	acme.ProblemTypeRejectedIdentifier: "Let's Encrypt will not issue certificates for this domain",
}

func postSetupDomain(w http.ResponseWriter, r *http.Request) {
	domainParam := strings.ToLower(r.PostFormValue("domain"))
	if domainParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				Domain is required
			</div>
		`))
		return
	}

	domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

	if strings.Contains(domainParam, "/") {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				It looks like you've entered a URL, please enter a domain
			</div>
		`))
		return
	}

	if net.ParseIP(domainParam).String() != "<nil>" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				It looks like you've entered an IP address, please enter a domain
			</div>
		`))
		return
	}

	if !domainPattern.MatchString(domainParam) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert domain-alert" hx-swap-oob="true">
				Invalid domain
			</div>
		`))
		return
	}

	if BUILD == "release" && metaSSL.Load() == "true" {
		found, err := lookupDomain(domainParam)
		if err != nil {
			log.Printf("postSetupDomain.lookupDomain: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					An unhandled error occurred
				</div>
			`))
			return
		}

		if !found {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					<span>
						We didn't find your domain's A record, verify it exists and then retry
					</span>

					<span>
						If your domain and A record is correct, you might need to wait a few minutes before retrying
					</span>
				</div>

				<div id="skip-domain-setup" class="skip-domain-setup" hx-swap-oob="true">
					<p>We can also monitor things in the background and redirect you when your domain is ready</p>
					<form onsubmit="onSubmitSkipDomain(this);" hx-post="/setup/skip-domain" hx-swap="none">
						<input name="domain" type="hidden">
						<button>Skip ahead</button>
					</form>

					<script>
						function onSubmitSkipDomain(form) {
							const domain = document.querySelector(".setup-domain").elements.domain.value;
							form.elements.domain.value = domain;
						}
					</script>
				</div>
			`))
			return
		}

		err = certmagic.ManageSync(r.Context(), []string{domainParam})
		if err != nil {
			var acmeProblem acme.Problem
			if errors.As(err, &acmeProblem) {
				if msg, ok := acmeProblemTypeMessages[acmeProblem.Type]; ok {
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(
						fmt.Sprintf(`
							<div id="alert" class="alert domain-alert" hx-swap-oob="true">
								%s
							</div>
						`,
							msg,
						),
					))
					return
				}

				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(
					fmt.Sprintf(`
						<div id="alert" class="alert domain-alert" hx-swap-oob="true">
							An unhandled error occurred %s
						</div>
						`,
						acmeProblem.Type,
					),
				))
				return
			}

			log.Printf("postSetupDomain.ManageSync: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`
				<div id="alert" class="alert domain-alert" hx-swap-oob="true">
					An unexpected error occurred
				</div>
			`))
			return
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupDomain.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "domain", domainParam)
	if err != nil {
		log.Printf("postSetupDomain.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "account")
	if err != nil {
		log.Printf("postSetupDomain.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("postSetupDomain.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("account")
	metaDomain.Store(domainParam)

	if BUILD == "dev" || metaSSL.Load() == "false" {
		w.Header().Add("HX-Location", "/setup/account")
	}
}

func postSetupDomainSkip(w http.ResponseWriter, r *http.Request) {
	domainParam := strings.ToLower(r.PostFormValue("domain"))
	if domainParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	domainPattern := regexp.MustCompile(`^[a-z0-9]+(?:[\-.][a-z0-9]+)*\.[a-z]+$`)

	if !domainPattern.MatchString(domainParam) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupDomainSkip.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "unconfirmedDomain", domainParam)
	if err != nil {
		log.Printf("postSetupDomainSkip.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "account")
	if err != nil {
		log.Printf("postSetupDomainSkip.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("postSetupDomainSkip.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("account")
	metaUnconfirmedDomain.Store(domainParam)

	appWg.Add(1)
	go monitorUnconfirmedDomainLoop(appCtx, &appWg)

	w.Header().Add("HX-Location", "/setup/account")
}

func getSetupAccount(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupAccount", getSetupAccountMarkup)
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
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

func postSetupAccount(w http.ResponseWriter, r *http.Request) {
	username := r.PostFormValue("username")
	if username == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Username is required
			</div>
		`))
		return
	}

	password := r.PostFormValue("password")
	passwordConfirmation := r.PostFormValue("password-confirmation")

	if password != passwordConfirmation {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Passwords do not match
			</div>
		`))
		return
	}

	if len(password) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Password must contain at least 8 characters
			</div>
		`))
		return
	}

	pwHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("postSetup.GenerateFromPassword: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	userID, err := createUser(tx, username, string(pwHash))
	if err != nil {
		log.Printf("postSetup.createUser: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tokenBytes := make([]byte, 32)
	_, err = rand.Read(tokenBytes)
	if err != nil {
		log.Printf("postSetup.Read: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	csrfTokenBytes := make([]byte, 32)
	_, err = rand.Read(csrfTokenBytes)
	if err != nil {
		log.Printf("postSetup.Read2: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token := base64.StdEncoding.EncodeToString(tokenBytes)
	csrfToken := base64.StdEncoding.EncodeToString(csrfTokenBytes)

	err = createSession(tx, token, csrfToken, userID)
	if err != nil {
		log.Printf("postSetup.createSession: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "name")
	if err != nil {
		log.Printf("postSetup.updateMetaValue: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaSetup.Store("name")

	http.SetCookie(
		w,
		&http.Cookie{
			Name:     "session",
			Value:    token,
			Path:     "/",
			Expires:  time.Now().UTC().Add(sessionLifetime),
			Secure:   BUILD == "release",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		},
	)

	w.Header().Add("HX-Location", "/setup/name")
}

func getSetupName(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getSetupName", getSetupNameMarkup)
	if err != nil {
		log.Printf("getSetup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		log.Printf("getSetup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postSetupName(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`
			<div id="alert" class="alert" hx-swap-oob="true">
				Name is required
			</div>
		`))
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSetupName.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "name", name)
	if err != nil {
		log.Printf("postSetupName.updateMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMetaValue(tx, "setup", "done")
	if err != nil {
		log.Printf("postSetupName.updateMetaValueSetup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postSetupName.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)
	metaSetup.Store("done")

	w.Header().Add("HX-Location", "/")
}
