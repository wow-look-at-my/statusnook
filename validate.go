package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

var validateConfigFlag = flag.String(
	"validate-config",
	"",
	"validate a config file and exit; status 0 when it would apply cleanly",
)

// validateConfig applies a config to a throwaway in-memory database and
// reports what applyConfig objected to, without touching the real one.
//
// Running the real apply is the point. A separate schema check would drift
// from the rules the instance actually enforces on a GitHub push, and the
// whole reason to validate offline is that that push has no other feedback
// channel.
func validateConfig(path string) error {
	cfgBytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// cache=shared with a single connection: a plain :memory: DSN gives every
	// pooled connection its own empty database, so the schema would vanish.
	db, err := sql.Open("sqlite3", "file:validate?mode=memory&cache=shared&_foreign_keys=on")
	if err != nil {
		return fmt.Errorf("validateConfig.Open: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(sqlSchema); err != nil {
		return fmt.Errorf("validateConfig.ExecSchema: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("validateConfig.Begin: %w", err)
	}
	// Always discarded: this database exists only to be reported on.
	defer tx.Rollback()

	// applyConfig needs a key to attempt `secret_` decryption with. A random
	// one never decrypts, which is the correct offline outcome -- the real key
	// lives in the instance's database. Undecryptable values stay verbatim, so
	// the field-shape checks still see a non-empty string.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("validateConfig.RandKey: %w", err)
	}
	if err := updateMetaValue(tx, "secretKey", base64.StdEncoding.EncodeToString(key)); err != nil {
		return fmt.Errorf("validateConfig.updateMetaValue: %w", err)
	}

	if err := seedRenameSources(tx, cfgBytes); err != nil {
		return err
	}

	msgs, err := applyConfig(tx, cfgBytes)
	if err != nil {
		// A decode failure arrives here rather than in msgs, so unwrap the
		// call-site prefix and leave the yaml error itself intact.
		return fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "applyConfig."))
	}

	if len(msgs) > 0 {
		return fmt.Errorf("%s", strings.Join(msgs, "\n"))
	}

	return nil
}

// seedRenameSources inserts a placeholder row for every `rename:` source, so
// the rename checks resolve against this database the way they would against
// the instance's. Without it every rename reports its source as missing --
// the one part of applyConfig that genuinely reads live state.
//
// Only the slug is load-bearing; the rest of each row is filler that the
// config's own entry overwrites moments later in the same transaction.
func seedRenameSources(tx *sql.Tx, cfgBytes []byte) error {
	cfg, err := decodeConfig(cfgBytes)
	if err != nil {
		// Not this function's error to report: applyConfig decodes the same
		// bytes and returns the message the user needs.
		return nil
	}

	for k := range cfg.Rename {
		entityType, src, found := strings.Cut(k, ".")
		if !found {
			continue
		}

		var query string
		switch entityType {
		case "services":
			query = `insert or ignore into service(slug, name, helper_text) values(?, '', '')`
		case "monitors":
			query = `insert or ignore into monitor(slug, name, url, method, frequency, timeout, attempts)
				values(?, '', '', 'GET', 60, 5, 1)`
		case "notification-channels":
			query = `insert or ignore into notification_channel(slug, name, type, details)
				values(?, '', 'smtp', '{}')`
		case "mail-groups":
			query = `insert or ignore into mail_group(slug, name) values(?, '')`
		default:
			// applyConfig reports the unknown entity type itself.
			continue
		}

		if _, err := tx.Exec(query, src); err != nil {
			return fmt.Errorf("seedRenameSources.Exec: %w", err)
		}
	}

	return nil
}
