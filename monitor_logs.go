package main

import (
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getMonitorAllLogs(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	afterParam := r.URL.Query().Get("after")
	if afterParam == "" {
		return
	}

	after, err := strconv.Atoi(afterParam)
	if err != nil {
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitorAllLogs.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	const logLimit = 2160

	monitorLogs, err := listMonitorLogs(tx, id, logLimit, after, 0, date)
	if err != nil {
		log.Printf("getMonitorAllLogs.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitorAllLogs.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastLogID := 0
	if len(monitorLogs) > 0 {
		lastLogID = monitorLogs[len(monitorLogs)-1].ID
	}

	const markup = `
		{{range $log := .Logs}}
			<div {{if index $.TimeIDs $log.ID}}id="{{index $.TimeIDs $log.ID}}"{{end}}>
				<span>{{$log.StartedAt}}</span>
				<span>{{$log.Latency}}</span>
				{{if eq $log.Result "error"}}
					<span class="badge{{if ge $log.ResponseCode.Int64 400}} badge--error{{end}}">
						{{$log.ResponseCode.Int64}}
					</span>
				{{end}}

				{{if eq $log.Result "success"}}
					<span class="badge">
						{{$log.ResponseCode.Int64}}
					</span>
				{{end}}

				{{if eq $log.Result "timeout"}}
					<span class="badge badge--error">
						TIMEOUT
					</span>
				{{end}}
			</div>
			{{if eq $log.ID $.LastLogID }}
				<div
					style="display: none;" 
					hx-get="/admin/monitors/{{$.MonitorID}}/all?after={{$.LastLogID}}&date={{$.Date}}" 
					hx-trigger="load delay:500ms"
					hx-target=".monitor-logs-container"
					hx-swap="beforeend"
				>
				</div>
			{{end}}
		{{end}}
		{{if not (len .Logs)}}
			<div id="monitor-time" class="monitor-time" hx-swap-oob="true">
				<span id="loader" class="loader" style="display: none;"></span>

				<form>
					<input class="time-input" type="time" name="time" />
					<button>
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
							<path fill-rule="evenodd" d="M9 3.5a5.5 5.5 0 100 11 5.5 5.5 0 000-11zM2 9a7 7 0 1112.452 4.391l3.328 3.329a.75.75 0 11-1.06 1.06l-3.329-3.328A7 7 0 012 9z" clip-rule="evenodd" />
						</svg>
					</button>
				</form>
			</div>
			<script>
				(() => {
					const timeInputForm = document.querySelector(
						".monitor-time form"
					);

					const timeInput = document.querySelector(
						".time-input"
					);

					timeInput.addEventListener("change", () => {
						timeInput.setCustomValidity("");
						timeInput.reportValidity();
					});
	
					timeInputForm.addEventListener("submit", (e) => {
						e.preventDefault();
						
						const result = document.getElementById(timeInput.value); 
						
						if (result) {
							window.location.hash = timeInput.value;
							result.scrollIntoView();
						} else {
							timeInput.setCustomValidity("No results");
							timeInput.reportValidity();
						}
					});
				})();
			</script>
		{{end}}
	`

	tmpl, err := parseTmpl("getMonitorAllLogs", markup)
	if err != nil {
		log.Printf("getMonitorAllLogs.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	formattedMonitorLogs := make([]MonitorLogView, 0, len(monitorLogs))
	for _, log := range monitorLogs {
		formattedMonitorLogs = append(
			formattedMonitorLogs,
			MonitorLogView{
				ID:        log.ID,
				StartedAt: log.StartedAt.Format("2006/01/02 15:04:05 MST"),
				Latency: log.EndedAt.Sub(log.StartedAt).
					Round(time.Millisecond * 1),
				ResponseCode: log.ResponseCode,
				ErrorMessage: log.ErrorMessage,
				Attempts:     log.Attempts,
				Result:       log.Result,
				MonitorID:    log.MonitorID,
			},
		)
	}

	timeIDs := make(map[int]string, logLimit)
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() ||
				log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Logs      []MonitorLogView
			LastLogID int
			MonitorID int
			TimeIDs   map[int]string
			Date      string
		}{
			Logs:      formattedMonitorLogs,
			LastLogID: lastLogID,
			MonitorID: id,
			TimeIDs:   timeIDs,
			Date:      dateParam,
		},
	)
	if err != nil {
		log.Printf("getMonitorAllLogs.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorPoll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	beforeParam := r.URL.Query().Get("before")
	if beforeParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	before, err := strconv.Atoi(beforeParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitorPoll.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitorLogs, err := listMonitorLogs(tx, id, 0, 0, before, date)
	if err != nil {
		log.Printf("getMonitorPoll.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastChecked, err := getMonitorLogLastChecked(tx, id)
	if err != nil {
		log.Printf("getMonitorPoll.getMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitorPoll.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	refreshDelay := 5

	nextRefreshMsg := fmt.Sprintf(
		"Checking for updates in %ds",
		refreshDelay,
	)

	const markup = `
		<div id="last-checked-status" class="badge{{if not .LastCheckedSuccess}} badge--error{{end}}" hx-swap-oob="true">
			{{if .LastCheckedSuccess}}
				<span>OK</span>
			{{else}}
				<span>Error</span>
			{{end}}
		</div>

		<span id="next-refresh" class="next-refresh" hx-swap-oob="true">
			{{.NextRefreshMsg}}
		</span>

		{{range $i, $log := .Logs}}
			<div 
				{{if index $.TimeIDs $log.ID}}id="{{index $.TimeIDs $log.ID}}"{{end}}
				{{if eq $i 0}}
					hx-get="/admin/monitors/{{$.MonitorID}}/poll?before={{$log.ID}}&date={{$.Date}}" 
					hx-trigger="load delay:{{$.RefreshDelay}}s"
					hx-target="this"
					hx-swap="outerHTML"
				{{end}}
			>
				<span>{{$log.StartedAt}}</span>
				<span>{{$log.Latency}}</span>
				{{if eq $log.Result "error"}}
					<span class="badge{{if ge $log.ResponseCode.Int64 400}} badge--error{{end}}">
						{{$log.ResponseCode.Int64}}
					</span>
				{{end}}

				{{if eq $log.Result "success"}}
					<span class="badge">
						{{$log.ResponseCode.Int64}}
					</span>
				{{end}}

				{{if eq $log.Result "timeout"}}
					<span class="badge badge--error">
						TIMEOUT
					</span>
				{{end}}
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getMonitorPoll", markup)
	if err != nil {
		log.Printf("getMonitorPoll.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	formattedMonitorLogs := make([]MonitorLogView, 0, len(monitorLogs))
	for _, log := range monitorLogs {
		formattedMonitorLogs = append(
			formattedMonitorLogs,
			MonitorLogView{
				ID:        log.ID,
				StartedAt: log.StartedAt.Format("2006/01/02 15:04:05 MST"),
				Latency: log.EndedAt.Sub(log.StartedAt).
					Round(time.Millisecond * 1),
				ResponseCode: log.ResponseCode,
				ErrorMessage: log.ErrorMessage,
				Attempts:     log.Attempts,
				Result:       log.Result,
				MonitorID:    log.MonitorID,
			},
		)
	}

	timeIDs := make(map[int]string, len(formattedMonitorLogs))
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() ||
				log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
	}

	err = tmpl.Execute(
		w,
		struct {
			Logs               []MonitorLogView
			LastLogID          int
			MonitorID          int
			TimeIDs            map[int]string
			RefreshDelay       int
			NextRefreshMsg     string
			LastCheckedSuccess bool
			Date               string
		}{
			Logs:               formattedMonitorLogs,
			MonitorID:          id,
			TimeIDs:            timeIDs,
			RefreshDelay:       5,
			NextRefreshMsg:     nextRefreshMsg,
			LastCheckedSuccess: lastChecked.ResponseCode.Int32 != 0 && lastChecked.ResponseCode.Int32 < 400,
			Date:               dateParam,
		},
	)
	if err != nil {
		log.Printf("getMonitorPoll.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
