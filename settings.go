package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getSettings(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") != ""

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getSettings.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	users, err := listUsers(tx)
	if err != nil {
		log.Printf("getSettings.listUsers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	invitations, err := listActiveUserInvitations(tx, time.Now().UTC().Add(-time.Hour*24))
	if err != nil {
		log.Printf("getSettings.listActiveUserInvitations: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configFileEnabledStr, err := getMetaValue(tx, "configFileEnabled")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueConfigFileEnabled: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configFileEnabled := false
	if configFileEnabledStr != "" {
		configFileEnabled, err = strconv.ParseBool(configFileEnabledStr)
		if err != nil {
			log.Printf("getSettings.ParseBoolConfigFileEnabled: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	configFile := ""
	if configFileEnabled {
		cfg, err := getMetaValue(tx, "configFile")
		if err != nil {
			log.Printf("getSettings.getMetaValueConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		configFile = cfg
	}

	githubManagedConfigStr, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubManagedConfig := false
	if githubManagedConfigStr != "" {
		githubManagedConfig, err = strconv.ParseBool(githubManagedConfigStr)
		if err != nil {
			log.Printf("getSettings.ParseBoolGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubConfigSHA := ""
	githubRepoURL := ""
	githubConfigPath := ""
	githubConfigBranch := ""
	githubConfigErrors := []string{}
	if githubManagedConfig {
		githubConfigSHA, err = getMetaValue(tx, "githubConfigSHA")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("getSettings.getMetaValueGitHubConfigSHA: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if len(githubConfigSHA) > 7 {
			githubConfigSHA = githubConfigSHA[0:7]
		}

		githubRepoURL, err = getMetaValue(tx, "githubRepoURL")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubRepoURL: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		githubConfigPath, err = getMetaValue(tx, "githubConfigPath")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubConfigPath: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		githubConfigBranch, err = getMetaValue(tx, "githubConfigBranch")
		if err != nil {
			log.Printf("getSettings.getMetaValueGitHubConfigBranch: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	githubConfigErrorsStr, err := getMetaValue(tx, "githubConfigErrors")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("getSettings.getMetaValueGitHubConfigErrors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if githubConfigErrorsStr != "" {
		err = json.Unmarshal([]byte(githubConfigErrorsStr), &githubConfigErrors)
		if err != nil {
			log.Printf("getSettings.UnmarshalGitHubConfigErrors: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getSettings.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type FormattedInvitation struct {
		ID        int
		Token     string
		ExpiresIn string
	}

	formattedInvitations := make([]FormattedInvitation, 0, len(invitations))

	for _, invitation := range invitations {
		expiresIn := invitation.CreatedAt.Add(time.Hour * 24).Sub(time.Now().UTC())
		h := int(expiresIn.Truncate(time.Hour).Hours())
		m := int(expiresIn.Truncate(time.Minute).Minutes()) - (h * 60)
		formattedInvitations = append(
			formattedInvitations,
			FormattedInvitation{
				ID:        invitation.ID,
				Token:     invitation.Token,
				ExpiresIn: fmt.Sprintf("%dh %dm", h, m),
			},
		)
	}

	tmpl, err := parseTmpl("get_settings.html")
	if err != nil {
		log.Printf("getSettings.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			CurrentVersion      string
			Domain              string
			Users               []SettingsUser
			Invitations         []FormattedInvitation
			Refresh             bool
			ConfigFileEnabled   bool
			ConfigFile          string
			GitHubManagedConfig bool
			GitHubConfigSHA     string
			GitHubCommitLink    string
			GitHubConfigErrors  []string
			EnvName             bool
			Ctx                 pageCtx
		}{
			CurrentVersion:      VERSION,
			Domain:              metaDomain,
			Users:               users,
			Invitations:         formattedInvitations,
			Refresh:             refresh,
			ConfigFileEnabled:   configFileEnabled,
			ConfigFile:          configFile,
			GitHubManagedConfig: githubManagedConfig,
			GitHubConfigSHA:     githubConfigSHA,
			GitHubCommitLink:    githubBlobLink(githubRepoURL, githubConfigBranch, githubConfigPath),
			GitHubConfigErrors:  githubConfigErrors,
			EnvName:             env.Name != "",
			Ctx:                 getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getSettings.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

// githubBlobLink points at the config file on GitHub. An empty branch means the
// repository's default branch, which GitHub resolves from "HEAD".
func githubBlobLink(repoURL string, branch string, path string) string {
	if repoURL == "" {
		return ""
	}

	if branch == "" {
		branch = "HEAD"
	}

	return repoURL + "/blob/" + branch + "/" + path
}
