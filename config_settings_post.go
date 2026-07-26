package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// alertOOB renders an out-of-band alert box for htmx to swap in.
func alertOOB(message string) []byte {
	return []byte(fmt.Sprintf(
		`<div id="alert" class="alert" hx-swap-oob="true">%s</div>`,
		message,
	))
}

func postConfigSettings(w http.ResponseWriter, r *http.Request) {
	if env.GitHub.Managed() {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB(
			"Configuration is managed through the environment " +
				"(STATUSNOOK_GITHUB_REPO) and cannot be changed here",
		))
		return
	}

	configFile := r.PostFormValue("config-file") == "on"
	githubManaged := r.PostFormValue("github-managed") == "on"

	githubRepoURL := strings.TrimSuffix(strings.TrimSpace(r.PostFormValue("github-repo-url")), "/")
	githubBranch := strings.TrimSpace(r.PostFormValue("github-branch"))
	githubConfigPath := strings.TrimSpace(r.PostFormValue("github-config-path"))
	githubToken := strings.TrimSpace(r.PostFormValue("github-token"))
	githubWebhookSecret := r.PostFormValue("github-webhook-secret")

	if !configFile && githubManaged {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if githubManaged {
		// The form never renders the stored credentials, so a blank field
		// means "keep what is already saved".
		stored, err := storedGitHubCredentials()
		if err != nil {
			log.Printf("postConfigSettings.storedGitHubCredentials: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if githubToken == "" {
			githubToken = stored.token
		}
		if githubWebhookSecret == "" {
			githubWebhookSecret = stored.webhookSecret
		}

		if githubRepoURL == "" || githubBranch == "" || githubConfigPath == "" ||
			githubToken == "" || githubWebhookSecret == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB(
				"Repository URL, branch, config path, token and webhook secret are all required",
			))
			return
		}
	}

	src := gitHubConfigSource{
		Branch: githubBranch,
		Path:   githubConfigPath,
		Token:  githubToken,
	}

	if githubManaged {
		repo, err := normalizeRepo(githubRepoURL)
		if err != nil || repo == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB(
				"Invalid GitHub repository URL. Use " +
					"https://github.com/owner/repository",
			))
			return
		}
		src.Repo = repo

		if err := checkGitHubRepo(r.Context(), src); err != nil {
			log.Printf("postConfigSettings.checkGitHubRepo: %s", describeGitHubError(err))

			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB(gitHubSetupMessage(err, "repository")))
			return
		}

		if _, err := fetchGitHubConfig(r.Context(), src); err != nil {
			log.Printf("postConfigSettings.fetchGitHubConfig: %s", describeGitHubError(err))

			w.WriteHeader(http.StatusBadRequest)
			w.Write(alertOOB(gitHubSetupMessage(err, "configuration")))
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

	values := map[string]string{
		"configFileEnabled":   strconv.FormatBool(configFile),
		"githubManagedConfig": strconv.FormatBool(githubManaged),
	}

	if githubManaged {
		values["githubRepoURL"] = "https://github.com/" + src.Repo
		values["githubConfigBranch"] = src.Branch
		values["githubConfigPath"] = src.Path
		values["githubConfigToken"] = src.Token
		values["githubConfigWebhookSecret"] = githubWebhookSecret
	} else {
		// Forget the credentials along with the mode they belonged to.
		values["githubConfigToken"] = ""
		values["githubConfigWebhookSecret"] = ""
		values["githubConfigSHA"] = ""
		values["githubConfigErrors"] = ""
	}

	for name, value := range values {
		if err := updateMetaValue(tx, name, value); err != nil {
			log.Printf("postConfigSettings.updateMetaValue %s: %s", name, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if !metaConfigFileEnabled && configFile {
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

	metaConfigFileEnabled = configFile

	w.Header().Add("HX-Location", "/admin/settings")
}

type gitHubCredentials struct {
	token         string
	webhookSecret string
}

// storedGitHubCredentials reads the credentials already saved for the GitHub
// managed config.
func storedGitHubCredentials() (gitHubCredentials, error) {
	creds := gitHubCredentials{}

	tx, err := db.Begin()
	if err != nil {
		return creds, fmt.Errorf("storedGitHubCredentials.Begin: %w", err)
	}
	defer tx.Rollback()

	for name, target := range map[string]*string{
		"githubConfigToken":         &creds.token,
		"githubConfigWebhookSecret": &creds.webhookSecret,
	} {
		value, err := getMetaValue(tx, name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return creds, fmt.Errorf("storedGitHubCredentials.%s: %w", name, err)
		}
		*target = value
	}

	return creds, nil
}

// gitHubSetupMessage explains a failed connection attempt in terms of the field
// the operator most likely got wrong.
func gitHubSetupMessage(err error, subject string) string {
	var apiErr *gitHubAPIError

	if errors.As(err, &apiErr) {
		switch {
		case apiErr.unauthorized():
			return "There's an issue with your personal access token. " +
				"Please double-check it grants read access to the repository contents"
		case apiErr.notFound() && subject == "repository":
			return "Your GitHub repository could not be found. Please double-check " +
				"the repository URL and your token's permissions"
		case apiErr.notFound():
			return "Your Statusnook configuration could not be found. " +
				"Please double-check the path and branch"
		}
	}

	return "Could not reach GitHub. Please try again"
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
