package main

import (
	"encoding/json"
	"golang.org/x/mod/semver"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func update(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("update.html")
	if err != nil {
		log.Printf("update.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			CurrentVersion string
			Ctx            pageCtx
		}{
			CurrentVersion: VERSION,
			Ctx:            getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("update.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func updateCheck(w http.ResponseWriter, r *http.Request) {
	httpClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		releaseAPIURL, nil,
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

	tmpl, err := parseTmpl("update_check.html")
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
			Docker:          env.SelfUpdateDisabled,
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
	tmpl, err := parseTmpl("after_update.html")
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
	if env.SelfUpdateDisabled {
		// Under Docker the image owns the binary: replacing it would be
		// undone by the next container start, and the running process kills
		// itself to restart, so the container would come back on the old
		// binary at best.
		w.WriteHeader(http.StatusBadRequest)
		w.Write(alertOOB(
			"Self-update is disabled. Pull a newer image and recreate the container",
		))
		return
	}

	httpClient := http.Client{
		Timeout: 5 * time.Minute,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		releaseAPIURL,
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

	resp, err = httpClient.Do(downloadReq)
	if err != nil {
		log.Printf("postUpdate.DoDownload: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Writing a 404 page over the binary and then restarting used to be a
		// possible outcome here.
		log.Printf("postUpdate.StatusCodeDownload: %d", resp.StatusCode)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		log.Printf("postUpdate.Executable: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Download beside the current binary, then rename over it: a failed or
	// truncated download leaves the working binary in place.
	tmpPath := executable + ".new"

	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		log.Printf("postUpdate.Create: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	written, err := io.Copy(file, resp.Body)
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

	if written < 1<<20 {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Size: downloaded %d bytes, too small to be a build", written)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err := os.Rename(tmpPath, executable); err != nil {
		os.Remove(tmpPath)
		log.Printf("postUpdate.Rename: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("post_update.html")
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
