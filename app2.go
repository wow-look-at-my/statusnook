package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"
)

func updateCheck(w http.ResponseWriter, r *http.Request) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/goksan/statusnook/releases/latest", nil,
	)
	if err != nil {
		log.Printf("updateCheck.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("updateCheck.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("updateCheck.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// A 403 rate-limit body unmarshals cleanly into an empty release, whose
	// blank tag semver.Compare ranks below any real version -- so a failed
	// check rendered as "Statusnook is up to date".
	if resp.StatusCode != http.StatusOK {
		log.Printf("updateCheck.StatusCode %d: %s", resp.StatusCode, string(body))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type GitHubReleaseAsset struct {
		Name string `json:"name"`
	}

	type GitHubRelease struct {
		TagName     string               `json:"tag_name"`
		Assets      []GitHubReleaseAsset `json:"assets"`
		Body        string               `json:"body"`
		PublishedAt time.Time            `json:"published_at"`
	}

	latestRelease := GitHubRelease{}

	err = json.Unmarshal(body, &latestRelease)
	if err != nil {
		log.Printf("updateCheck.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	updateAvailable := semver.Compare(latestRelease.TagName, VERSION) > 0
	latestVersion := latestRelease.TagName

	tmpl, err := parseTmpl("updateCheck", updateCheckMarkup)
	if err != nil {
		log.Printf("updateCheck.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			UpdateAvailable bool
			LatestVersion   string
			PublishedAt     string
			UpdateBody      string
			Docker          bool
			Ctx             pageCtx
		}{

			UpdateAvailable: updateAvailable,
			LatestVersion:   latestVersion,
			PublishedAt:     latestRelease.PublishedAt.Format("2006/01/02"),
			UpdateBody:      latestRelease.Body,
			Docker:          *dockerFlag,
			Ctx:             getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("updateCheck.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func afterUpdate(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("afterUpdate", afterUpdateMarkup)
	if err != nil {
		log.Printf("afterUpdate.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Version string
			Ctx     pageCtx
		}{
			Version: VERSION,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("afterUpdate.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func postUpdate(w http.ResponseWriter, r *http.Request) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		"https://api.github.com/repos/goksan/statusnook/releases/latest",
		nil,
	)
	if err != nil {
		log.Printf("postUpdate.NewRequest: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("postUpdate.Do: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("postUpdate.ReadAll: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("postUpdate.StatusCode %d: %s", resp.StatusCode, string(body))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type GitHubReleaseAsset struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}

	type GitHubRelease struct {
		TagName string               `json:"tag_name"`
		Assets  []GitHubReleaseAsset `json:"assets"`
	}

	latestRelease := GitHubRelease{}

	err = json.Unmarshal(body, &latestRelease)
	if err != nil {
		log.Printf("postUpdate.Unmarshal: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	updateAvailable := semver.Compare(latestRelease.TagName, VERSION) > 0
	latestVersion := latestRelease.TagName

	if !updateAvailable {
		return
	}

	downloadURL := ""

	for _, asset := range latestRelease.Assets {
		if strings.Contains(asset.Name, runtime.GOOS+"_"+runtime.GOARCH) {
			downloadURL = asset.URL
		}
	}

	if downloadURL == "" {
		log.Printf("postUpdate.downloadURL: no download URL")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	downloadReq, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		log.Printf("postUpdate.NewRequestDownload: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	downloadReq.Header.Add("Accept", "application/octet-stream")

	// Its own client: httpClient's 10s Timeout is a whole-request deadline
	// that covers the body read, so it aborted the copy partway through a
	// multi-megabyte binary on any link slower than ~2 MB/s.
	downloadClient := http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}

	resp, err = downloadClient.Do(downloadReq)
	if err != nil {
		log.Printf("postUpdate.DoDownload: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// The status was never checked, so a 403 rate-limit body was written over
	// the binary, chmod 0700, and the process then SIGINT'd itself into a
	// restart loop on a JSON file.
	if resp.StatusCode != http.StatusOK {
		log.Printf("postUpdate.DownloadStatusCode: %d", resp.StatusCode)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	exePath, err := os.Executable()
	if err != nil {
		log.Printf("postUpdate.Executable: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Download beside the binary, then rename over it. os.Remove first left
	// nothing to fall back to the moment anything after it failed, and rename
	// within a directory is atomic.
	tmpPath := exePath + ".new"

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		log.Printf("postUpdate.Create: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		file.Close()
		os.Remove(tmpPath)
		log.Printf("postUpdate.Copy: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := file.Close(); err != nil {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Close: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := os.Rename(tmpPath, exePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Rename: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("postUpdate", postUpdateMarkup)
	if err != nil {
		log.Printf("postUpdate.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	svg, err := staticFS.ReadFile("static/images/statusnook.svg")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx             pageCtx
			Svg             template.HTML
			Version         string
			UpdateAvailable bool
			LatestVersion   string
		}{

			Ctx:             getPageCtx(r),
			Svg:             template.HTML(svg),
			UpdateAvailable: updateAvailable,
			LatestVersion:   latestVersion,
		},
	)
	if err != nil {
		log.Printf("postUpdate.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	syscall.Kill(syscall.Getpid(), syscall.SIGINT)
}

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
		if githubConfigSHA != "" {
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

	tmpl, err := parseTmpl("getSettings", getSettingsMarkup)
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
			Ctx                 pageCtx
		}{
			CurrentVersion:      VERSION,
			Domain:              metaDomain.Load(),
			Users:               users,
			Invitations:         formattedInvitations,
			Refresh:             refresh,
			ConfigFileEnabled:   configFileEnabled,
			ConfigFile:          configFile,
			GitHubManagedConfig: githubManagedConfig,
			GitHubConfigSHA:     githubConfigSHA,
			GitHubCommitLink: githubRepoURL + "/blob/" + githubConfigBranch + "/" +
				githubConfigPath,
			GitHubConfigErrors: githubConfigErrors,
			Ctx:                getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getSettings.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
