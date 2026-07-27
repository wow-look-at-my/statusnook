package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
)

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

		w.WriteHeader(http.StatusBadRequest)
		w.Write(saveErrorsOOB([]string{formattedErr}))
		return
	}

	if len(msgs) > 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(saveErrorsOOB(msgs))
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
		log.Fatalf("postConfig.getMetaValueSetupName: %s", err)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postConfig.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaName = name

	w.Write(renderFragment(
		"fragment_config_saved.html",
		struct{ Config string }{config},
	))
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

		w.Write(renderFragment(
			"fragment_secret_output.html",
			struct{ Value string }{"secret_" + b64Ciphertext},
		))
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

		w.Write(renderFragment(
			"fragment_secret_output.html",
			struct{ Value string }{string(plaintext)},
		))
	}
}
