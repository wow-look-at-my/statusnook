package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

type Monitor struct {
	ID             int
	Slug           string
	Name           string
	URL            string
	Method         string
	Frequency      int
	Timeout        int
	Attempts       int
	RequestHeaders map[string]string
	BodyFormat     sql.NullString
	Body           sql.NullString
}

func listMonitors(tx *sql.Tx) ([]Monitor, error) {
	const query = `
		select id, slug, name, url, method, frequency, timeout, attempts, request_headers, 
			body_format, body
		from monitor
	`

	monitorListings := []Monitor{}

	rows, err := tx.Query(query)
	if err != nil {
		return monitorListings, fmt.Errorf("listMonitors.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var serializedRequestHeaders sql.NullString

		monitor := Monitor{}
		err = rows.Scan(
			&monitor.ID,
			&monitor.Slug,
			&monitor.Name,
			&monitor.URL,
			&monitor.Method,
			&monitor.Frequency,
			&monitor.Timeout,
			&monitor.Attempts,
			&serializedRequestHeaders,
			&monitor.BodyFormat,
			&monitor.Body,
		)
		if err != nil {
			return monitorListings, fmt.Errorf("listMonitors.Scan: %w", err)
		}

		requestHeaders := map[string]string{}
		if serializedRequestHeaders.Valid {
			err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
			if err != nil {
				return monitorListings, fmt.Errorf("listMonitors.Unmarshal: %w", err)
			}
		}

		monitor.RequestHeaders = requestHeaders
		monitorListings = append(monitorListings, monitor)
	}

	return monitorListings, nil
}

func monitors(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("monitors.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	monitors, err := listMonitors(tx)
	if err != nil {
		log.Printf("monitors.listMonitors: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	lastCheckedLogs, err := listAllMonitorLogLastChecked(tx)
	if err != nil {
		log.Printf("monitors.listAllMonitorLogLastChecked: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	monitorHappy := make(map[int]bool, len(lastCheckedLogs))
	for _, v := range lastCheckedLogs {
		monitorHappy[v.ID] = v.ResponseCode.Int32 != 0 && v.ResponseCode.Int32 < 400
	}

	const markup = `
		{{define "title"}}Monitors{{end}}
		{{define "body"}}
			<div class="admin-nav-header">
				<div>
					<h2>Monitors</h2>
				</div>

				{{if not .Ctx.ConfigFile}}
					<div>
						<a href="/admin/monitors/create" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
								<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
							</svg>
						</a>
					</div>
				{{end}}
			</div>

			{{if eq (len .Monitors) 0}}
				<div class="entity-empty-state">
					<div class="icon">
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
							<path d="M10 12.5a2.5 2.5 0 100-5 2.5 2.5 0 000 5z" />
							<path fill-rule="evenodd" d="M.664 10.59a1.651 1.651 0 010-1.186A10.004 10.004 0 0110 3c4.257 0 7.893 2.66 9.336 6.41.147.381.146.804 0 1.186A10.004 10.004 0 0110 17c-4.257 0-7.893-2.66-9.336-6.41zM14 10a4 4 0 11-8 0 4 4 0 018 0z" clip-rule="evenodd" />
						</svg>
					</div>
					<span>Create your first monitor</span>
					{{if not .Ctx.ConfigFile}}
						<a class="action" href="/admin/monitors/create" hx-boost="true">Create monitor</a>
					{{else}}
						<a class="action" href="/admin/settings#config-form" hx-boost="true">Go to settings</a>
					{{end}}
				</div>
			{{else}}
				<div class="monitors-container">
					{{range $monitor := .Monitors}}
						<a hx-boost="true" href="/admin/monitors/{{$monitor.ID}}">
							<div>
								<span>{{$monitor.Name}}</span>
								<span>{{$monitor.URL}}</span>
							</div>
							{{if index $.MonitorHappy $monitor.ID}}
								<span class="badge">OK</span>
							{{else}}
								<span class="badge badge--error">Error</span>
							{{end}}
						</a>
					{{end}}
				</div>
			{{end}}
		{{end}}
	`

	tmpl, err := parseTmpl("monitors", markup)
	if err != nil {
		log.Printf("monitors.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Monitors     []Monitor
			MonitorHappy map[int]bool
			Ctx          pageCtx
		}{

			Monitors:     monitors,
			MonitorHappy: monitorHappy,
			Ctx:          getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("monitors.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getMonitorByID(tx *sql.Tx, id int) (Monitor, error) {
	const query = `
		select
			id,
			name,
			url,
			method,
			frequency,
			timeout,
			attempts,
			request_headers,
			body_format,
			body
		from
			monitor
		where
			id = ?
	`

	monitor := Monitor{}

	var serializedRequestHeaders sql.NullString

	err := tx.QueryRow(query, id).Scan(
		&monitor.ID,
		&monitor.Name,
		&monitor.URL,
		&monitor.Method,
		&monitor.Frequency,
		&monitor.Timeout,
		&monitor.Attempts,
		&serializedRequestHeaders,
		&monitor.BodyFormat,
		&monitor.Body,
	)
	if err != nil {
		return monitor, fmt.Errorf("getMonitorByID.QueryRow: %w", err)
	}

	requestHeaders := map[string]string{}

	if serializedRequestHeaders.Valid {
		err = json.Unmarshal([]byte(serializedRequestHeaders.String), &requestHeaders)
		if err != nil {
			return monitor, fmt.Errorf("getMonitorByID.Unmarshal: %w", err)
		}
	}

	monitor.RequestHeaders = requestHeaders

	return monitor, nil
}

type MonitorLog struct {
	ID           int
	StartedAt    time.Time
	EndedAt      time.Time
	ResponseCode sql.NullInt64
	ErrorMessage sql.NullString
	Attempts     int
	Result       string
	MonitorID    int
}

func listMonitorLogs(tx *sql.Tx, monitorID int, limit int, after int, before int, date time.Time) ([]MonitorLog, error) {
	query := `
		select
			id,
			started_at,
			ended_at,
			response_code,
			error_message,
			attempts,
			result,
			monitor_id
		from
			monitor_log
		where
			monitor_id = ?
	`

	if after != 0 {
		query += "and id < ?"
	}

	if before != 0 {
		query += " and id >= ?"
	}

	query += " and started_at >= ? and started_at < ?"

	query += "\norder by id desc"

	if limit > 0 {
		query += "\nlimit " + strconv.Itoa(limit)
	}

	monitorLogs := make([]MonitorLog, 0, limit)

	params := []any{monitorID}

	if after != 0 {
		params = append(params, after)
	}

	if before != 0 {
		params = append(params, before)
	}

	endOfDay := date.Add(time.Hour * 24)
	params = append(params, date, endOfDay)

	rows, err := tx.Query(query, params...)
	if err != nil {
		return monitorLogs, fmt.Errorf("listMonitorLogs.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		monitorLog := MonitorLog{}
		err = rows.Scan(
			&monitorLog.ID,
			&monitorLog.StartedAt,
			&monitorLog.EndedAt,
			&monitorLog.ResponseCode,
			&monitorLog.ErrorMessage,
			&monitorLog.Attempts,
			&monitorLog.Result,
			&monitorLog.MonitorID,
		)
		if err != nil {
			return monitorLogs, fmt.Errorf("listMonitorLogs.Scan: %w", err)
		}
		monitorLogs = append(monitorLogs, monitorLog)
	}

	return monitorLogs, nil
}

type MonitorLogView struct {
	ID           int
	StartedAt    string
	Latency      time.Duration
	ResponseCode sql.NullInt64
	ErrorMessage sql.NullString
	Attempts     int
	Result       string
	MonitorID    int
}
