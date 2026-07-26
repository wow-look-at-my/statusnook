package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

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
			w.Write([]byte(`
				<div id="alert" class="alert" hx-swap-oob="true">
					Invalid GitHub repository URL
				</div>`,
			))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		repoPath := parsedRepoURL.Path[1:]

		httpClient := http.Client{
			Timeout: time.Second * 10,
		}

		req, err := http.NewRequest(
			http.MethodGet,
			"https://api.github.com/repos/"+repoPath,
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

		respBody, err := io.ReadAll(r.Body)
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

			}
			return
		}

		req, err = http.NewRequest(
			http.MethodGet,
			"https://api.github.com/repos/"+path.Join(repoPath, "contents", githubConfigPath)+"?ref="+
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

		respBody, err = io.ReadAll(r.Body)
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
						Your Statusnook configuration could not be found. An unexpcted error occurred.
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
