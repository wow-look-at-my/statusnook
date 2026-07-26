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

	const markup = `
		{{define "title"}}Create alert{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/alerts" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						 </a>
				  
						<h2>Create alert</h2>
					</div>
				</div>

				<form hx-post hx-swap="none" autocomplete="off">
					<label>
						Title
						<input name="title" required />
					</label>

					<label>
						Messages
						<textarea name="message" required></textarea>
					</label>

					<div id="services" {{if and .Ctx.HXRequest (not .Ctx.HXBoosted)}}hx-swap-oob="true"{{end}}>
						<label>
							Affected services
						</label>

						{{if not (len .Services)}}
							<div
								class="entity-empty-state entity-empty-state--secondary" 
								style="margin-top: 1.0rem;"
							>
								<div class="icon">
									<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
										<path d="M12 4.467c0-.405.262-.75.559-1.027.276-.257.441-.584.441-.94 0-.828-.895-1.5-2-1.5s-2 .672-2 1.5c0 .362.171.694.456.953.29.265.544.6.544.994a.968.968 0 0 1-1.024.974 39.655 39.655 0 0 1-3.014-.306.75.75 0 0 0-.847.847c.14.993.242 1.999.306 3.014A.968.968 0 0 1 4.447 10c-.393 0-.729-.253-.994-.544C3.194 9.17 2.862 9 2.5 9 1.672 9 1 9.895 1 11s.672 2 1.5 2c.356 0 .683-.165.94-.441.276-.297.622-.559 1.027-.559a.997.997 0 0 1 1.004 1.03 39.747 39.747 0 0 1-.319 3.734.75.75 0 0 0 .64.842c1.05.146 2.111.252 3.184.318A.97.97 0 0 0 10 16.948c0-.394-.254-.73-.545-.995C9.171 15.693 9 15.362 9 15c0-.828.895-1.5 2-1.5s2 .672 2 1.5c0 .356-.165.683-.441.94-.297.276-.559.622-.559 1.027a.998.998 0 0 0 1.03 1.005c1.337-.05 2.659-.162 3.961-.337a.75.75 0 0 0 .644-.644c.175-1.302.288-2.624.337-3.961A.998.998 0 0 0 16.967 12c-.405 0-.75.262-1.027.559-.257.276-.584.441-.94.441-.828 0-1.5-.895-1.5-2s.672-2 1.5-2c.362 0 .694.17.953.455.265.291.601.545.995.545a.97.97 0 0 0 .976-1.024 41.159 41.159 0 0 0-.318-3.184.75.75 0 0 0-.842-.64c-1.228.164-2.473.271-3.734.319A.997.997 0 0 1 12 4.467Z" />
									</svg>
								</div>
								<span>No services found</span>
								<div class="actions">
									<a class="action" href="/admin/services/create" target="_blank">
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
											<path d="M6.22 8.72a.75.75 0 0 0 1.06 1.06l5.22-5.22v1.69a.75.75 0 0 0 1.5 0v-3.5a.75.75 0 0 0-.75-.75h-3.5a.75.75 0 0 0 0 1.5h1.69L6.22 8.72Z" />
											<path d="M3.5 6.75c0-.69.56-1.25 1.25-1.25H7A.75.75 0 0 0 7 4H4.75A2.75 2.75 0 0 0 2 6.75v4.5A2.75 2.75 0 0 0 4.75 14h4.5A2.75 2.75 0 0 0 12 11.25V9a.75.75 0 0 0-1.5 0v2.25c0 .69-.56 1.25-1.25 1.25h-4.5c-.69 0-1.25-.56-1.25-1.25v-4.5Z" />
										</svg>
										Add service
									</a>
									<button type="button" class="empty-state-refresh" hx-get="">
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor">
											<path fill-rule="evenodd" d="M13.836 2.477a.75.75 0 0 1 .75.75v3.182a.75.75 0 0 1-.75.75h-3.182a.75.75 0 0 1 0-1.5h1.37l-.84-.841a4.5 4.5 0 0 0-7.08.932.75.75 0 0 1-1.3-.75 6 6 0 0 1 9.44-1.242l.842.84V3.227a.75.75 0 0 1 .75-.75Zm-.911 7.5A.75.75 0 0 1 13.199 11a6 6 0 0 1-9.44 1.241l-.84-.84v1.371a.75.75 0 0 1-1.5 0V9.591a.75.75 0 0 1 .75-.75H5.35a.75.75 0 0 1 0 1.5H3.98l.841.841a4.5 4.5 0 0 0 7.08-.932.75.75 0 0 1 1.025-.273Z" clip-rule="evenodd" />
										</svg>
										Refresh
									</button>
								</div>
							</div>
						{{end}}

						<div class="checkbox-group">
							{{range $service := .Services}}
								<label>
									<input
										name="services"
										type="checkbox"
										value="{{$service.ID}}"
										onchange="updateService();"
										required
									/>
									{{$service.Name}}
								</label>
							{{end}}
						</div>
					</div>

					<label>
						Type
					</label>
					<div class="checkbox-group">
						<label>
							<input 
								name="type" 
								type="radio"
								value="incident"
								onchange="updateType(this.value);"
							 	required
							/>
							Incident
						</label>
						<label>
							<input 
								name="type"
								type="radio"
								value="maintenance"
								onchange="updateType(this.value);"
								required
							/>
							Maintenance / notice
						</label>
					</div>

					<fieldset class="radio-group" style="display: none;" disabled>
						<legend>Severity</legend>
						<input name="severity" type="radio" value="red" required style="background-color: #E57F73;"/>
						<input name="severity" type="radio" value="amber" required style="background-color: #E5B773;"/>
					</fieldset>

					<div>
						<button type="submit">Create</button>
					</div>
				</form>
			</div>
			<script>
				function updateService() {
					const inputs = [...document.querySelectorAll("form [name='services']")];
					const anyChecked = inputs.some(e => e.checked); 

					[...document.querySelectorAll("form [name='services']")].forEach((e) => {
						e.required = !anyChecked;
					});
				}

				function updateType(type) {
					if (type === "incident") {
						document.querySelector(".radio-group").style.display = "block";
						document.querySelector(".radio-group").disabled = false;
					} else {
						document.querySelector(".radio-group").style.display = "none";
						document.querySelector(".radio-group").disabled = true;
					}
				}
			</script>
		{{end}}
	`

	tmpl, err := parseTmpl("getCreateAlert", markup)
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
