package main

import (
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"time"
)

func getEditAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("getEditAlert.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alert, err := getAlertByID(tx, id)
	if err != nil {
		log.Printf("getEditAlert.getAlertByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("getEditAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_edit_alert.html")
	if err != nil {
		log.Printf("getEditAlert.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	checkedServices := map[int]bool{}
	for _, service := range services {
		checkedServices[service.ID] = false
	}
	for _, service := range alert.Services {
		checkedServices[service.ID] = true
	}

	err = tmpl.Execute(w, struct {
		Alert           AlertDetail
		Services        []service
		CheckedServices map[int]bool
		Ctx             pageCtx
	}{
		Alert:           alert,
		Services:        services,
		CheckedServices: checkedServices,
		Ctx:             getPageCtx(r),
	})
	if err != nil {
		log.Printf("getEditAlert.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editAlert(
	tx *sql.Tx,
	id int,
	title string,
	services []int,
	alertType string,
	severity string,
) error {
	const alertQuery = `
		update alert set title = ?, type = ?, severity = ? where id = ?
	`

	_, err := tx.Exec(alertQuery, title, alertType, severity, id)
	if err != nil {
		return fmt.Errorf("editAlert.Exec: %w", err)
	}

	const serviceDeleteQuery = `
		delete from alert_service where alert_id = ?
	`

	_, err = tx.Exec(serviceDeleteQuery, id)
	if err != nil {
		return fmt.Errorf("editAlert.Exec2: %w", err)
	}

	const baseServiceInsertQuery = `
		insert into alert_service(alert_id, service_id) values
	`

	serviceInsertQuery := baseServiceInsertQuery

	params := []any{}

	for i, serviceID := range services {
		serviceInsertQuery += "(?, ?)"
		if i < len(services)-1 {
			serviceInsertQuery += ", "
		}
		params = append(params, id, serviceID)
	}

	_, err = tx.Exec(serviceInsertQuery, params...)
	if err != nil {
		return fmt.Errorf("editAlert.Exec3: %w", err)
	}

	return nil
}

func postEditAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	title := r.PostFormValue("title")
	if title == "" {
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
		log.Printf("postEditAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editAlert(tx, id, title, services, alertType, severity)
	if err != nil {
		log.Printf("postEditAlert.editAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postEditAlert.getOngoingAlerts: %s", err)
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
		log.Printf("postEditAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if err = tx.Commit(); err != nil {
		log.Printf("postEditAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts")
}

func resolveAlert(tx *sql.Tx, id int) error {
	const query = `
		update alert set ended_at = ? where id = ?
	`

	_, err := tx.Exec(query, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("resolveAlert.Exec: %w", err)
	}

	return nil
}

func getSeverity(tx *sql.Tx) (string, error) {
	const query = `
		select severity from severity limit 1
	`

	var severity string

	err := tx.QueryRow(query).Scan(&severity)
	if err != nil {
		return severity, fmt.Errorf("getSeverity.QueryRow: %w", err)
	}

	return severity, nil
}

func updateSeverity(tx *sql.Tx, severity string) error {
	const query = `
		update severity set severity = ?
	`

	_, err := tx.Exec(query, severity)
	if err != nil {
		return fmt.Errorf("updateSeverity.Exec: %w", err)
	}

	return nil
}

func postResolveAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postResolveAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = resolveAlert(tx, id)
	if err != nil {
		log.Printf("postResolveAlert.resolveAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postResolveAlert.getOngoingAlerts: %s", err)
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
		log.Printf("postResolveAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postResolveAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}

func unresolveAlert(tx *sql.Tx, id int) error {
	const query = `
		update alert set ended_at = null where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("unresolveAlert.Exec: %w", err)
	}

	return nil
}

func postUnresolveAlert(w http.ResponseWriter, r *http.Request) {
	idParam := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postUnresolveAlert.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = unresolveAlert(tx, id)
	if err != nil {
		log.Printf("postUnresolveAlert.resolveAlert: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	alerts, err := getOngoingAlerts(tx)
	if err != nil {
		log.Printf("postUnresolveAlert.getOngoingAlerts: %s", err)
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
		log.Printf("postUnresolveAlert.updateSeverity: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postUnresolveAlert.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/alerts/"+idParam)
}
