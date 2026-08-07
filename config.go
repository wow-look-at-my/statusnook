package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

var metaConfigFileEnabled atomic.Bool

type StatusnookConfigGeneralSettings struct {
	Name string `json:"name" yaml:"name"`
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
			githubAPIBaseURL+"/repos/"+repoPath,
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

		// resp, not r: the request body was drained by PostFormValue long ago,
		// so this logged an empty string and threw away GitHub's reason.
		respBody, err := io.ReadAll(resp.Body)
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
			} else {
				// Anything else wrote a 400 with an empty body, so the form
				// reported nothing at all and looked like it had ignored the save.
				w.Write([]byte(
					`<div id="alert" class="alert" hx-swap-oob="true">
						GitHub answered ` + strconv.Itoa(resp.StatusCode) +
						` when checking your repository. Please try again.
					</div>`,
				))
			}
			return
		}

		req, err = http.NewRequest(
			http.MethodGet,
			githubAPIBaseURL+"/repos/"+path.Join(repoPath, "contents", githubConfigPath)+"?ref="+
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

		respBody, err = io.ReadAll(resp.Body)
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
						Your Statusnook configuration could not be found. An unexpected error occurred.
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
