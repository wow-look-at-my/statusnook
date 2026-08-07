package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mattn/go-sqlite3"
	"gopkg.in/yaml.v3"
)

// slugPattern is package level so every apply stage shares one compiled
// regexp instead of each rebuilding it.
var slugPattern = regexp.MustCompile(`^-+|[^\p{Ll}\d-]+|-+$`)

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

		// Open PANICS on a nonce of the wrong length rather than returning an
		// error, and this runs over a config file a push can put in front of
		// it -- so a truncated secret_ value crashed the request that applied
		// it, webhook included.
		if len(nonce) != aesGCM.NonceSize() {
			continue
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

	// Each stage validates and writes its own entity type. They run in order
	// because a later stage's cross-references resolve against what the
	// earlier ones just wrote.
	for _, stage := range []func(*sql.Tx, StatusnookConfig, []string) ([]string, error){
		applyConfigServices,
		applyConfigMonitors,
		applyConfigNotificationChannels,
		applyConfigMailGroups,
		applyConfigReferences,
	} {
		msgs, err = stage(tx, cfg, msgs)
		if err != nil {
			return msgs, err
		}
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
