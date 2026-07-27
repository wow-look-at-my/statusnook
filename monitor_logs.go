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

	tmpl, err := parseTmpl("get_monitor_all_logs.html")
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

	tmpl, err := parseTmpl("get_monitor_poll.html")
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
