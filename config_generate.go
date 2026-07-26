package main

import (
	"database/sql"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

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
		GeneralSettings: StatusnookConfigGeneralSettings{Name: metaName},
	}

	cfgBytes, err := yaml.Marshal(cfg)
	if err != nil {
		return cfgStr, fmt.Errorf("generateConfig.Marshal: %w", err)
	}

	cfgStr = string(cfgBytes)

	return cfgStr, nil
}
