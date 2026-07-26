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
	const markup = `
		{{define "title"}}Update{{end}}
		{{define "body"}}
			<div class="admin-nav-header">
				<div>
					<h2>Update</h2>
				</div>
			</div>

			<div class="update-container">
				<div>
					<span>Current version</span>
					<span>{{.CurrentVersion}}</span>
				</div>
				<hr>
				<div class="new-update-title" hx-get="/admin/update/check" hx-trigger="load" hx-swap="outerHTML">
					<div>
						<div class="icon icon--rotate">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
								<path fill-rule="evenodd" d="M13.836 2.477a.75.75 0 0 1 .75.75v3.182a.75.75 0 0 1-.75.75h-3.182a.75.75 0 0 1 0-1.5h1.37l-.84-.841a4.5 4.5 0 0 0-7.08.932.75.75 0 0 1-1.3-.75 6 6 0 0 1 9.44-1.242l.842.84V3.227a.75.75 0 0 1 .75-.75Zm-.911 7.5A.75.75 0 0 1 13.199 11a6 6 0 0 1-9.44 1.241l-.84-.84v1.371a.75.75 0 0 1-1.5 0V9.591a.75.75 0 0 1 .75-.75H5.35a.75.75 0 0 1 0 1.5H3.98l.841.841a4.5 4.5 0 0 0 7.08-.932.75.75 0 0 1 1.025-.273Z" clip-rule="evenodd" />
							</svg>									
						</div>
						<span>Checking for updates...</span>
					</div>
				</div>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("update", markup)
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

	const markup = `
		<div>
			{{if .UpdateAvailable}}
				<div class="new-update-title">
					<div>
						<div class="icon">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
								<path d="M8.75 2.75a.75.75 0 0 0-1.5 0v5.69L5.03 6.22a.75.75 0 0 0-1.06 1.06l3.5 3.5a.75.75 0 0 0 1.06 0l3.5-3.5a.75.75 0 0 0-1.06-1.06L8.75 8.44V2.75Z" />
								<path d="M3.5 9.75a.75.75 0 0 0-1.5 0v1.5A2.75 2.75 0 0 0 4.75 14h6.5A2.75 2.75 0 0 0 14 11.25v-1.5a.75.75 0 0 0-1.5 0v1.5c0 .69-.56 1.25-1.25 1.25h-6.5c-.69 0-1.25-.56-1.25-1.25v-1.5Z" />
							</svg>					  
						</div>
						<span>Updates are available</span>
					</div>
					{{if not .Docker}}
						<button onclick="document.getElementById('update-dialog').showModal();">
							<span>Update</span>
						</button>
					{{end}}
				</div>
				<div class="new-update-details">
					<div>
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
							<path fill-rule="evenodd" d="M4.5 2A2.5 2.5 0 0 0 2 4.5v2.879a2.5 2.5 0 0 0 .732 1.767l4.5 4.5a2.5 2.5 0 0 0 3.536 0l2.878-2.878a2.5 2.5 0 0 0 0-3.536l-4.5-4.5A2.5 2.5 0 0 0 7.38 2H4.5ZM5 6a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" clip-rule="evenodd" />
						</svg>
						<span>{{.LatestVersion}}</span>
					</div>
					<div>
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
							<path d="M5.75 7.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5ZM5 10.25a.75.75 0 1 1 1.5 0 .75.75 0 0 1-1.5 0ZM10.25 7.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5ZM7.25 8.25a.75.75 0 1 1 1.5 0 .75.75 0 0 1-1.5 0ZM8 9.5A.75.75 0 1 0 8 11a.75.75 0 0 0 0-1.5Z" />
							<path fill-rule="evenodd" d="M4.75 1a.75.75 0 0 0-.75.75V3a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V5a2 2 0 0 0-2-2V1.75a.75.75 0 0 0-1.5 0V3h-5V1.75A.75.75 0 0 0 4.75 1ZM3.5 7a1 1 0 0 1 1-1h7a1 1 0 0 1 1 1v4.5a1 1 0 0 1-1 1h-7a1 1 0 0 1-1-1V7Z" clip-rule="evenodd" />
						</svg>
						<span>{{.PublishedAt}}</span>
					</div>
				</div>
			{{else}}
				<div class="new-update-title">
					<div>
						<div class="icon">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="w-4 h-4">
								<path fill-rule="evenodd" d="M12.416 3.376a.75.75 0 0 1 .208 1.04l-5 7.5a.75.75 0 0 1-1.154.114l-3-3a.75.75 0 0 1 1.06-1.06l2.353 2.353 4.493-6.74a.75.75 0 0 1 1.04-.207Z" clip-rule="evenodd" />
							</svg>							  
						</div>
						<span>Statusnook is up to date</span>
					</div>
				</div>
			{{end}}
		</div>
		{{if .UpdateAvailable}}
			<p class="new-update-notes">{{.UpdateBody}}</p>
		{{end}}
		<dialog class="modal" id="update-dialog">
			<span>Update to {{.LatestVersion}}</span>
			<form hx-post hx-swap="outerHTML" hx-target="#update-dialog">
				<div>
					<button onclick="document.getElementById('update-dialog').close(); return false;">Cancel</button>
					<button>Start update</button>
				</div>
			</form>
		</dialog>
	`

	tmpl, err := parseTmpl("updateCheck", markup)
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
	const markup = `	
		<div id="update-overlay" hx-swap="none">
			<img src="/static/images/statusnook.svg" width="226" height="59">
			<span>Successfully installed {{.Version}}</span>
			<button onclick="location.reload();"><span>Continue</span></button>
		</div>
	`

	tmpl, err := parseTmpl("afterUpdate", markup)
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

	err = os.Remove("statusnook")
	if err != nil {
		log.Printf("postUpdate.Remove: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	file, err := os.Create("statusnook")
	if err != nil {
		log.Printf("postUpdate.Create: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = file.Chmod(0700)
	if err != nil {
		log.Printf("postUpdate.Chmod: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		log.Printf("postUpdate.Copy: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	file.Close()

	const markup = `
		<div 
			id="update-overlay"
			hx-get="/admin/update/after-update"
			hx-trigger="every 2s"
			hx-swap="outerHTML"
		>
			{{.Svg}}
			<span class="loader"></span>
		</div>
	`

	tmpl, err := parseTmpl("postUpdate", markup)
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
