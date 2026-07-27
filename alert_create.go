package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getCreateAlert(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("getCreateAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("getCreateAlert.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getCreateAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_create_alert.html")
	if err != nil {
		log.Printf("getCreateAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Services []service
			Ctx      pageCtx
		}{
			Services: services,
			Ctx:      getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createAlert(
	tx *sql.Tx,
	title string,
	services []int,
	alertType string,
	severity string,
) (int, error) {
	const alertQuery = `
		insert into alert(title, type, severity, created_at) values(?, ?, ?, ?) returning id
	`

	alertID := 0
	err := tx.QueryRow(alertQuery, title, alertType, severity, time.Now().UTC()).Scan(&alertID)
	if err != nil {
		return alertID, fmt.Errorf("createAlert.Scan: %w", err)
	}

	const baseServiceQuery = `
		insert into alert_service(alert_id, service_id) values
	`

	serviceQuery := baseServiceQuery

	params := []any{}

	for i, serviceID := range services {
		serviceQuery += "(?, ?)"
		if i < len(services)-1 {
			serviceQuery += ", "
		}
		params = append(params, alertID, serviceID)
	}

	_, err = tx.Exec(serviceQuery, params...)
	if err != nil {
		return alertID, fmt.Errorf("createAlert.Exec: %w", err)
	}

	return alertID, nil
}

func createAlertMessageNotifications(tx *sql.Tx, createdAt time.Time, alertMessageID int) error {
	const query = `
		insert into alert_notification(created_at, alert_subscription_id, alert_message_id)
		select ?, id, ? from alert_subscription where alert_subscription.active = true
	`

	_, err := tx.Exec(query, time.Now().UTC(), alertMessageID)
	if err != nil {
		return fmt.Errorf("createAlertMessageNotifications.Exec: %w", err)
	}

	return nil
}

func updateAlertSentAtByID(tx *sql.Tx, now time.Time, ids []int) error {
	const baseQuery = `
		update alert_notification set sent_at = ?
		where id in(
	`

	query := baseQuery

	params := []any{time.Now().UTC()}
	for i, destination := range ids {
		query += "?"
		if i < len(ids)-1 {
			query += ","
		}
		params = append(params, destination)
	}
	query += ")"

	_, err := tx.Exec(query, params...)
	if err != nil {
		return fmt.Errorf("updateAlertSentAtByID.Exec: %w", err)
	}

	return nil
}

func postCreateAlert(w http.ResponseWriter, r *http.Request) {
	title := r.PostFormValue("title")
	if title == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := r.PostFormValue("message")
	if message == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	services := []int{}
	for _, service := range r.PostForm["services"] {
		num, err := strconv.Atoi(service)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		services = append(services, num)
	}
	if len(services) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	alertType := r.PostFormValue("type")
	if alertType != "incident" && alertType != "maintenance" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	severity := r.PostFormValue("severity")
	if alertType == "incident" {
		if severity != "red" && severity != "amber" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	} else {
		alertType = "maintenance"
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	alertID, err := createAlert(tx, title, services, alertType, severity)
	if err != nil {
		log.Printf("postCreateAlert.createAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alertMessageID, err := createAlertMessage(tx, alertID, message)
	if err != nil {
		log.Printf("postCreateAlert.createAlertMessage: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postCreateAlert.getOngoingAlerts: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	newSeverity := "blue"
	for _, alert := range alerts {
		if alert.Severity == "amber" {
			newSeverity = "amber"
			continue
		}

		if alert.Severity == "red" {
			newSeverity = "red"
			break
		}
	}

	err = updateSeverity(tx, newSeverity)
	if err != nil {
		log.Printf("postCreateAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = createAlertMessageNotifications(tx, time.Now().UTC(), alertMessageID)
	if err != nil {
		log.Printf("postCreateAlert.createAlertMessageNotifications: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postCreateAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}
