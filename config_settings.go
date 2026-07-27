package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
)

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

	// Only whether a credential exists is sent to the browser. Rendering the
	// token and webhook secret into the page put them in the page source, in
	// the browser's form history and in any screen share of this page.
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

	tmpl, err := parseTmpl("get_config_settings.html")
	if err != nil {
		log.Printf("getConfigSettings.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	envPollInterval := ""
	if env.GitHub.PollInterval > 0 {
		envPollInterval = env.GitHub.PollInterval.String()
	}

	err = tmpl.Execute(w, struct {
		ConfigFile             bool
		GitHubManagedConfig    bool
		GitHubRepoURL          string
		GitHubConfigBranch     string
		GitHubConfigPath       string
		GitHubTokenSet         bool
		GitHubWebhookSecretSet bool
		EnvManaged             bool
		EnvBranch              string
		EnvPollInterval        string
		Domain                 string
		Ctx                    pageCtx
	}{
		ConfigFile:             configFile,
		GitHubManagedConfig:    githubManagedConfig,
		GitHubRepoURL:          githubRepoURL,
		GitHubConfigBranch:     githubBranch,
		GitHubConfigPath:       githubFilePath,
		GitHubTokenSet:         githubToken != "" || env.GitHub.Managed(),
		GitHubWebhookSecretSet: githubWebhookSecret != "",
		EnvManaged:             env.GitHub.Managed(),
		EnvBranch:              branchLabel(env.GitHub.Branch),
		EnvPollInterval:        envPollInterval,
		Domain:                 metaDomain,
		Ctx:                    getPageCtx(r),
	})
	if err != nil {
		log.Printf("getConfigSettings.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
