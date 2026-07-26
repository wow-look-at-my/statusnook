package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

func configWebhook(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	githubManagedConfig, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if githubManagedConfig != "true" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sig = strings.TrimPrefix(sig, "sha256=")

	key, err := getMetaValue(tx, "githubConfigWebhookSecret")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubWebhookSecret: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	repoURL, err := getMetaValue(tx, "githubRepoURL")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubRepoURL: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	branch, err := getMetaValue(tx, "githubConfigBranch")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigBranch: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configPath, err := getMetaValue(tx, "githubConfigPath")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigPath: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	token, err := getMetaValue(tx, "githubConfigToken")
	if err != nil {
		log.Printf("configWebhook.getMetaValueGitHubConfigToken: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	configSHA, err := getMetaValue(tx, "githubConfigSHA")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("configWebhook.getMetaValueGitHubConfigSHA: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("configWebhook.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(payload)
	payloadMac := mac.Sum(nil)

	headerMac, err := hex.DecodeString(sig)
	if err != nil {
		log.Printf("configWebhook.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !hmac.Equal(headerMac, payloadMac) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("configWebhook.CommitRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	parsedRepoURL, err := url.Parse(repoURL)
	if err != nil {
		log.Printf("configWebhook.Parse: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	repoPath := parsedRepoURL.Path[1:]

	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/"+path.Join(repoPath, "contents", configPath)+"?ref="+branch,
		nil,
	)
	if err != nil {
		log.Printf("configWebhook.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	req.Header.Add("Accept", "application/vnd.github+json")
	req.Header.Add("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("configWebhook.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("configWebhook.ReadAllNon200: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if string(respBody) != "" {
			log.Printf("configWebhook.StatusCode: %s", string(respBody))
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type repositoryContentResponse struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}

	var contentResp repositoryContentResponse

	jsonDecoder := json.NewDecoder(resp.Body)
	err = jsonDecoder.Decode(&contentResp)
	if err != nil {
		log.Printf("configWebhook.Decode: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	content, err := base64.StdEncoding.DecodeString(contentResp.Content)
	if err != nil {
		log.Printf("configWebhook.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tx, err = rwDB.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginWrite: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	if contentResp.SHA != configSHA {
		msgs, err := applyConfig(tx, content)
		if err != nil {
			unwrappedErr := errors.Unwrap(err)
			if !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
				log.Printf("configWebhook.applyConfig: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			msgs = append(msgs, strings.TrimPrefix(unwrappedErr.Error(), "yaml: "))
		}

		configErrors := ""

		if len(msgs) > 0 {
			msgsBytes, err := json.Marshal(msgs)
			if err != nil {
				log.Printf("configWebhook.Marshal: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			configErrors = string(msgsBytes)
		}

		err = updateMetaValue(tx, "githubConfigErrors", string(configErrors))
		if err != nil {
			log.Printf("configWebhook.updateMetaValueGitHubConfigErrors: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "configFile", string(content))
		if err != nil {
			log.Printf("configWebhook.updateMetaValueConfigFile: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = updateMetaValue(tx, "githubConfigSHA", contentResp.SHA)
		if err != nil {
			log.Printf("configWebhook.updateMetaValueGitHubConfigSHA: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("configWebhook.getMetaValueName: %s", err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("configWebhook.CommitWrite: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName = name
}
