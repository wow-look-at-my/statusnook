package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"regexp"
	"strings"
)

// slugPattern matches every character sequence that is not allowed in a
// resource key.
var slugPattern = regexp.MustCompile(`^-+|[^\p{Ll}\d-]+|-+$`)

// configApplier carries the state shared by the per-section apply steps.
type configApplier struct {
	tx       *sql.Tx
	cfg      StatusnookConfig
	cfgBytes []byte
	msgs     []string

	// populated by applyMonitors, consumed by applyMonitorLinks
	existingMonitorSlugs map[string]int
	// populated by applyMonitorLinks, consumed by applyAlertSettings
	channelSlugs map[string]int
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

	a := &configApplier{tx: tx, cfg: cfg, cfgBytes: cfgBytes, msgs: msgs}

	steps := []func() error{
		a.applyRenames,
		a.applyGeneralSettings,
		a.applyServices,
		a.applyMonitors,
		a.applyNotificationChannels,
		a.applyMailGroups,
		a.applyMonitorLinks,
		a.applyAlertSettings,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return a.msgs, err
		}
	}

	return a.msgs, nil
}

func (a *configApplier) applyGeneralSettings() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	if cfg.GeneralSettings.Name != "" {
		err := updateMetaValue(tx, "name", cfg.GeneralSettings.Name)
		if err != nil {
			return fmt.Errorf("applyConfig.updateMetaValueGeneralSettingsName: %w", err)
		}
	} else {
		msgs = append(msgs, "general-settings.name"+": is required")
	}

	return nil
}

func (a *configApplier) applyAlertSettings() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	channelSlugs := a.channelSlugs
	cfgBytes := a.cfgBytes

	managedSubscriptions := cfg.AlertNotificationSettings.ManagedSubscriptions

	err := updateAlertSettings(
		tx,
		cfg.AlertNotificationSettings.SlackInstallURL,
		cfg.AlertNotificationSettings.SlackClientSecret,
		managedSubscriptions,
	)
	if err != nil {
		return fmt.Errorf("applyConfig.updateAlertSettings: %w", err)
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
		return fmt.Errorf("applyConfig.getNotificationChannelBySlug: %w", err)
	}

	err = updateAlertSMTPNotificationSetting(tx, channel.ID)
	if err != nil {
		return fmt.Errorf("applyConfig.updateAlertSMTPNotificationSetting: %w", err)
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
		return fmt.Errorf("applyConfig.updateMetaValueConfigFile: %w", err)
	}

	msgs = finalMsgs

	return nil
}
