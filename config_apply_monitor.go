package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func (a *configApplier) applyMonitors() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	existingMonitorSlugs := map[string]int{}
	a.existingMonitorSlugs = existingMonitorSlugs

	monitors, err := listMonitors(tx)
	if err != nil {
		return fmt.Errorf("applyConfig.listMailGroups: %w", err)
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
			return fmt.Errorf("applyConfig.MarshalRequestHeaders: %w", err)
		}

		requestBodyNullStr := sql.NullString{Valid: false}
		bodyFormat := sql.NullString{Valid: false}

		if v.RequestBody != nil {
			if _, ok := v.RequestBody.(map[string]any); ok {
				vMap := v.RequestBody.(map[string]any)

				values := url.Values{}
				for k, v := range vMap {
					str := ""
					if vs, ok := v.(int); ok {
						str = strconv.Itoa(vs)
					}
					if vs, ok := v.(string); ok {
						str = vs
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
				return fmt.Errorf("applyConfig.editMonitor: %w", err)
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
				return fmt.Errorf("applyConfig.createMonitor: %w", err)
			}
		}
	}

	return nil
}

func (a *configApplier) applyMonitorLinks() error {
	tx, cfg := a.tx, a.cfg
	msgs := a.msgs
	defer func() { a.msgs = msgs }()

	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
	}

	existingMonitorSlugs := a.existingMonitorSlugs

	monitors, err := listMonitors(tx)
	if err != nil {
		return fmt.Errorf("applyConfig.listMonitors2: %w", err)
	}

	channels, err := listNotificationChannels(tx, listNotificationsOptions{})
	if err != nil {
		return fmt.Errorf("applyConfig.listNotificationChannels2: %w", err)
	}

	channelSlugs := map[string]int{}
	a.channelSlugs = channelSlugs
	for _, v := range channels {
		channelSlugs[v.Slug] = v.ID
	}

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		return fmt.Errorf("applyConfig.listMailGroups3: %w", err)
	}

	mailGroupSlugs := map[string]int{}
	for _, v := range mailGroups {
		mailGroupSlugs[v.Slug] = v.ID
	}

	for _, v := range monitors {
		if _, ok := cfg.Monitors[v.Slug]; !ok {
			err := deleteMonitorByID(tx, v.ID)
			if err != nil {
				return fmt.Errorf("applyConfig.deleteMonitorByID: %w", err)
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
			return fmt.Errorf("applyConfig.updateMonitorNotificationChannels: %w", err)
		}

		err = updateMonitorMailGroups(
			tx,
			existingMonitorSlugs[slug],
			mailGroupIDs,
		)
		if err != nil {
			return fmt.Errorf("applyConfig.updateMonitorMailGroups: %w", err)
		}
	}

	return nil
}
