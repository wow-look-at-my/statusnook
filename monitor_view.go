package main

import (
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getMonitor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	dateParam := r.URL.Query().Get("date")
	if dateParam == "" {
		dateParam = time.Now().UTC().Format("2006-01-02")
	}

	date, err := time.Parse("2006-01-02", dateParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getMonitor.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	const logLimit = 100

	monitor, err := getMonitorByID(tx, id)
	if err != nil {
		log.Printf("getMonitor.getMonitorByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorLogs, err := listMonitorLogs(tx, id, logLimit, 0, 0, date)
	if err != nil {
		log.Printf("getMonitor.listMonitorLogs: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastChecked, err := getMonitorLogLastChecked(tx, monitor.ID)
	if err != nil {
		log.Printf("getMonitor.getMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getMonitor.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if _, ok := r.URL.Query()["ready"]; ok {
		if len(monitorLogs) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	tmpl, err := parseTmpl("get_monitor.html")
	if err != nil {
		log.Printf("getMonitor.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	refreshDelay := 5

	nextRefreshMsg := fmt.Sprintf(
		"Checking for updates in %ds",
		refreshDelay,
	)

	timeIDs := make(map[int]string, logLimit)
	for i, log := range monitorLogs {
		if i > 0 {
			lastLog := monitorLogs[i-1]
			if log.StartedAt.Hour() != lastLog.StartedAt.Hour() || log.StartedAt.Minute() != lastLog.StartedAt.Minute() {
				timeIDs[log.ID] = log.StartedAt.Format("15:04")
			}
		} else {
			timeIDs[log.ID] = log.StartedAt.Format("15:04")
		}
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

	if dateParam != "" {
		w.Header().Set("HX-Push-Url", r.URL.Path+"?date="+dateParam)
	}

	lastLogID := 0
	if len(monitorLogs) > 0 {
		lastLogID = monitorLogs[len(monitorLogs)-1].ID
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitor            Monitor
			Logs               []MonitorLogView
			NextRefreshMsg     string
			LastCheckedSuccess bool
			LastLogID          int
			TimeIDs            map[int]string
			RefreshDelay       int
			Ctx                pageCtx
			Date               string
		}{
			Monitor:            monitor,
			Logs:               formattedMonitorLogs,
			NextRefreshMsg:     nextRefreshMsg,
			LastCheckedSuccess: lastChecked.ResponseCode.Int32 != 0 && lastChecked.ResponseCode.Int32 < 400,
			LastLogID:          lastLogID,
			TimeIDs:            timeIDs,
			RefreshDelay:       refreshDelay,
			Date:               dateParam,
			Ctx:                getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getMonitor.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}
