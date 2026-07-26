package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// maxWebhookPayload bounds a webhook body. GitHub's own limit is 25 MB, but a
// push event that matters here is a few KB, and anyone can POST to this
// endpoint before the signature is checked.
const maxWebhookPayload = 1 << 20

// webhookSecret resolves the shared secret, preferring the environment.
func webhookSecret(tx *sql.Tx) (string, error) {
	if env.GitHub.WebhookSecret != "" {
		return env.GitHub.WebhookSecret, nil
	}

	if env.GitHub.Managed() {
		// The environment owns the GitHub configuration but did not set a
		// webhook secret: webhook delivery stays off, polling does the work.
		return "", nil
	}

	secret, err := getMetaValue(tx, "githubConfigWebhookSecret")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	return secret, nil
}

// configWebhook applies the repository's config file when GitHub reports a push
// to the watched branch. Polling covers instances GitHub cannot reach; this
// endpoint just makes the update immediate for instances it can.
func configWebhook(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	src, err := gitHubConfigSourceFromDB(tx)
	if err != nil {
		tx.Rollback()
		log.Printf("configWebhook.gitHubConfigSourceFromDB: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	managed := src.FromEnv
	if !managed {
		githubManagedConfig, err := getMetaValue(tx, "githubManagedConfig")
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			tx.Rollback()
			log.Printf("configWebhook.getMetaValueGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		managed = githubManagedConfig == "true"
	}

	secret, err := webhookSecret(tx)
	if err != nil {
		tx.Rollback()
		log.Printf("configWebhook.webhookSecret: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("configWebhook.CommitRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if !managed || !src.configured() || secret == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookPayload+1))
	if err != nil {
		log.Printf("configWebhook.ReadAll: %s", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(payload) > maxWebhookPayload {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	if !validWebhookSignature(r.Header.Get("X-Hub-Signature-256"), secret, payload) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Everything below this line is authenticated.

	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if event != "" && event != "push" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !pushTouchesBranch(payload, src.Branch) {
		w.WriteHeader(http.StatusOK)
		return
	}

	if _, err := syncGitHubConfig(r.Context(), src); err != nil {
		log.Printf("configWebhook.syncGitHubConfig: %s", describeGitHubError(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// validWebhookSignature verifies GitHub's HMAC over the raw body in constant
// time.
func validWebhookSignature(header string, secret string, payload []byte) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}

	headerMac, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)

	return hmac.Equal(headerMac, mac.Sum(nil))
}

// pushTouchesBranch keeps a push to an unwatched branch from applying that
// branch's config. An empty watched branch means "the default branch", which
// the payload reports per push.
func pushTouchesBranch(payload []byte, branch string) bool {
	var push struct {
		Ref        string `json:"ref"`
		Repository struct {
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(payload, &push); err != nil {
		// Not a push payload we understand; let the sync decide.
		return true
	}

	if push.Ref == "" {
		return true
	}

	pushed := strings.TrimPrefix(push.Ref, "refs/heads/")
	if branch == "" {
		branch = push.Repository.DefaultBranch
	}
	if branch == "" {
		return true
	}

	return pushed == branch
}
