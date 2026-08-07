package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

func applyConfigMailGroups(tx *sql.Tx, cfg StatusnookConfig, msgs []string) ([]string, error) {
	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
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

	return msgs, nil
}

func applyConfigReferences(tx *sql.Tx, cfg StatusnookConfig, msgs []string) ([]string, error) {
	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	monitors, err := listMonitors(tx)
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

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		return msgs, fmt.Errorf("applyConfig.listMailGroups3: %w", err)
	}

	mailGroupSlugs := map[string]int{}
	for _, v := range mailGroups {
		mailGroupSlugs[v.Slug] = v.ID
	}

	// Rebuilt from the listMonitors above rather than carried over from the
	// monitor stage: the only lookups below are for slugs in cfg.Monitors,
	// and this loop adds every one of those.
	existingMonitorSlugs := map[string]int{}

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
	return msgs, nil
}
