package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func applyConfigServices(tx *sql.Tx, cfg StatusnookConfig, msgs []string) ([]string, error) {
	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
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

	return msgs, nil
}

func applyConfigMonitors(tx *sql.Tx, cfg StatusnookConfig, msgs []string) ([]string, error) {
	invalidSlugMsg := func(entityType string, slug string) {
		msgs = append(msgs, entityType+"."+slug+
			": must only contain lower-case letters, numbers, and hyphens")
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

	return msgs, nil
}
