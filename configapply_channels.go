package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
)

func applyConfigNotificationChannels(tx *sql.Tx, cfg StatusnookConfig, msgs []string) ([]string, error) {
	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
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

	return msgs, nil
}
