package main

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"strings"
)

func (a *configApplier) applyRenames() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	renameSrcMsg := func(k string) {
		msgs = append(msgs, "rename: '"+k+
			"' does not exist, drop this rename if you've already completed it")
	}

	renameDstMsg := func(entityType string, src string, dst string) {
		msgs = append(msgs, "rename: replace "+entityType+" '"+src+"' with "+" '"+dst+
			"' to perform a rename")
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
					return fmt.Errorf("applyConfig.updateServiceSlug: %w", err)
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
					return fmt.Errorf("applyConfig.updateNotificationChannelSlug: %w", err)
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
					return fmt.Errorf("applyConfig.updateMailGrouplug: %w", err)
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
					return fmt.Errorf("applyConfig.updateMonitorSlug: %w", err)
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

	return nil
}
