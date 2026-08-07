package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

func configWebhook(w http.ResponseWriter, r *http.Request) {
	// Public route, and the whole body is buffered before the signature is
	// checked. GitHub caps webhook payloads at 25 MB; without a cap here an
	// unauthenticated client streams until the 30s read timeout, repeatedly.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	tx, err := db.Begin()
	if err != nil {
		log.Printf("configWebhook.BeginRead: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// No row at all means the settings page never turned GitHub sync on, which
	// is the same answer as having turned it off -- and this is a public route,
	// so an instance that never configured it answered every delivery with a
	// 500 and a log line.
	githubManagedConfig, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
		githubAPIBaseURL+"/repos/"+path.Join(repoPath, "contents", configPath)+"?ref="+branch,
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
		// resp, not r: the request body was drained long ago, so reading it
		// here logged an empty string and threw away GitHub's reason.
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("configWebhook.ReadAllNon200: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if string(respBody) != "" {
			log.Printf("configWebhook.StatusCode %d: %s", resp.StatusCode, string(respBody))
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
			if unwrappedErr == nil || !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
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

			// applyConfig writes as it validates, deletes included, so by the
			// time one bad monitor produces a message the services, channels
			// and monitors the file no longer mentions are already gone. Throw
			// the whole apply away and record only the errors, the way the
			// admin editor path does. githubConfigSHA deliberately stays
			// unchanged so a corrected push re-applies instead of being
			// skipped as already-seen.
			if err := tx.Rollback(); err != nil {
				log.Printf("configWebhook.RollbackInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			errTx, err := rwDB.Begin()
			if err != nil {
				log.Printf("configWebhook.BeginInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			defer errTx.Rollback()

			if err := updateMetaValue(errTx, "githubConfigErrors", configErrors); err != nil {
				log.Printf("configWebhook.updateMetaValueGitHubConfigErrorsInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if err := updateMetaValue(errTx, "configFile", string(content)); err != nil {
				log.Printf("configWebhook.updateMetaValueConfigFileInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			if err := errTx.Commit(); err != nil {
				log.Printf("configWebhook.CommitInvalid: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			// 422 rather than 200: GitHub records the response in the webhook
			// delivery log, which is the only place a pusher looks.
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write(msgsBytes)
			return
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
		// log.Fatalf here let any DB error on a webhook GitHub delivers kill
		// the process outright, abandoning the open write transaction.
		log.Printf("configWebhook.getMetaValueName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("configWebhook.CommitWrite: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)
}

func postConfig(w http.ResponseWriter, r *http.Request) {
	config := r.PostFormValue("config")

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postConfig.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	githubManagedConfigStr, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postConfig.getMetaValueGitHubManagedConfig: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	githubManagedConfig := false
	if githubManagedConfigStr != "" {
		githubManagedConfig, err = strconv.ParseBool(githubManagedConfigStr)
		if err != nil {
			log.Printf("postConfig.ParseBoolGitHubManagedConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	if githubManagedConfig {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msgs, err := applyConfig(tx, []byte(config))
	if err != nil {
		unwrappedErr := errors.Unwrap(err)

		if unwrappedErr == nil || !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
			log.Printf("postConfig.applyConfig: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		formattedErr := strings.TrimPrefix(unwrappedErr.Error(), "yaml: ")

		errMsg := fmt.Sprintf(
			`<div class="save-overlay save-overlay--error">%s</div>`,
			html.EscapeString(formattedErr),
		)
		w.WriteHeader(http.StatusBadRequest)
		w.Write(
			[]byte(fmt.Sprintf(
				`<div 
					id="save-overlay-errors"
					class="save-overlay-errors"
					hx-swap-oob="true"
				>
					%s
				</div>`,
				errMsg,
			)),
		)
		return
	}

	if len(msgs) > 0 {
		errors := ""
		for _, v := range msgs {
			errors += fmt.Sprintf(
				`<div class="save-overlay save-overlay--error">%s</div>`,
				html.EscapeString(v),
			)
		}

		w.WriteHeader(http.StatusBadRequest)
		w.Write(
			[]byte(fmt.Sprintf(
				`<div id="save-overlay-errors" class="save-overlay-errors" hx-swap-oob="true">%s</div>`,
				errors,
			)),
		)
		return
	}

	err = updateMetaValue(tx, "githubConfigErrors", "")
	if err != nil {
		log.Printf("postConfig.updateMetaValueGitHubConfigErrors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("postConfig.getMetaValueSetupName: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postConfig.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName.Store(name)

	w.Write(
		[]byte(
			fmt.Sprintf(`
			<div id="save-overlay-errors" class="save-overlay-errors" hx-swap-oob="true"></div>
			<div id="update-config" hx-swap-oob="true">
				<script>
					document.querySelector("#save-overlay").style.display = "none";
					window.configFile = "%s";
				</script>
			</div>
			`,
				template.JSEscapeString(config),
			),
		),
	)
}

func postSecret(w http.ResponseWriter, r *http.Request) {
	action := r.PostFormValue("action")
	if action != "encrypt" && action != "decrypt" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	input := r.PostFormValue("input")
	if input == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSecret.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	key, err := getMetaValue(tx, "secretKey")
	if err != nil {
		log.Printf("postSecret.getMetaValue: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSecret.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	keyBytes, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		log.Printf("postSecret.DecodeString: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		log.Printf("postSecret.NewCipher: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		log.Printf("postSecret.NewGCM: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if action == "encrypt" {
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			log.Printf("postSecret.ReadFull: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ciphertext := aesGCM.Seal(nil, nonce, []byte(input), nil)

		b64Ciphertext := base64.StdEncoding.EncodeToString(ciphertext) + "." +
			base64.StdEncoding.EncodeToString(nonce)

		w.Write(
			[]byte(fmt.Sprintf(
				`<input id="output" placeholder="Output" value="%s" hx-swap-oob="true" disabled>`,
				"secret_"+html.EscapeString(b64Ciphertext),
			)),
		)
	} else if action == "decrypt" {
		nonceSplit := strings.Split(input, ".")
		if len(nonceSplit) != 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ciphertext, err := base64.StdEncoding.DecodeString(
			strings.TrimPrefix(nonceSplit[0], "secret_"),
		)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		nonce, err := base64.StdEncoding.DecodeString(nonceSplit[1])
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Write(
			[]byte(fmt.Sprintf(
				`<input id="output" placeholder="Output" value="%s" hx-swap-oob="true" disabled>`,
				html.EscapeString(string(plaintext)),
			)),
		)
	}
}
