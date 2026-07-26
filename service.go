package main

import (
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type service struct {
	ID         int
	Slug       string
	Name       string
	HelperText string
}

func listServices(tx *sql.Tx) ([]service, error) {
	const query = `
		select 
			id, slug, name, helper_text
		from
			service
	`

	services := []service{}

	rows, err := tx.Query(query)
	if err != nil {
		return services, fmt.Errorf("listServices.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		svc := service{}
		err = rows.Scan(
			&svc.ID,
			&svc.Slug,
			&svc.Name,
			&svc.HelperText,
		)
		if err != nil {
			return services, fmt.Errorf("listServices.Scan: %w", err)
		}

		services = append(services, svc)
	}

	return services, nil
}

func services(w http.ResponseWriter, r *http.Request) {
	tx, err := db.Begin()
	if err != nil {
		log.Printf("services.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("services.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("services.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}Services{{end}}
		{{define "body"}}
			<div class="admin-nav-header">
				<div>
					<h2>Services</h2>
				</div>

				{{if not .Ctx.ConfigFile}}
					<div>
						<a href="/admin/services/create" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
								<path d="M10.75 4.75a.75.75 0 00-1.5 0v4.5h-4.5a.75.75 0 000 1.5h4.5v4.5a.75.75 0 001.5 0v-4.5h4.5a.75.75 0 000-1.5h-4.5v-4.5z" />
							</svg>
						</a>
					</div>
				{{end}}
			</div>

			{{if eq (len .Services) 0}}
				<div class="entity-empty-state">
					<div class="icon">
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
							<path d="M12 4.467c0-.405.262-.75.559-1.027.276-.257.441-.584.441-.94 0-.828-.895-1.5-2-1.5s-2 .672-2 1.5c0 .362.171.694.456.953.29.265.544.6.544.994a.968.968 0 01-1.024.974 39.655 39.655 0 01-3.014-.306.75.75 0 00-.847.847c.14.993.242 1.999.306 3.014A.968.968 0 014.447 10c-.393 0-.729-.253-.994-.544C3.194 9.17 2.862 9 2.5 9 1.672 9 1 9.895 1 11s.672 2 1.5 2c.356 0 .683-.165.94-.441.276-.297.622-.559 1.027-.559a.997.997 0 011.004 1.03 39.747 39.747 0 01-.319 3.734.75.75 0 00.64.842c1.05.146 2.111.252 3.184.318A.97.97 0 0010 16.948c0-.394-.254-.73-.545-.995C9.171 15.693 9 15.362 9 15c0-.828.895-1.5 2-1.5s2 .672 2 1.5c0 .356-.165.683-.441.94-.297.276-.559.622-.559 1.027a.998.998 0 001.03 1.005c1.337-.05 2.659-.162 3.961-.337a.75.75 0 00.644-.644c.175-1.302.288-2.624.337-3.961A.998.998 0 0016.967 12c-.405 0-.75.262-1.027.559-.257.276-.584.441-.94.441-.828 0-1.5-.895-1.5-2s.672-2 1.5-2c.362 0 .694.17.953.455.265.291.601.545.995.545a.97.97 0 00.976-1.024 41.159 41.159 0 00-.318-3.184.75.75 0 00-.842-.64c-1.228.164-2.473.271-3.734.319A.997.997 0 0112 4.467z" />
						</svg>
					</div>
					<span>Add your first service</span>
					{{if not .Ctx.ConfigFile}}
						<a class="action" href="/admin/services/create" hx-boost="true">Add service</a>
					{{else}}
						<a class="action" href="/admin/settings#config-form" hx-boost="true">Go to settings</a>
					{{end}}
				</div>
			{{else}}
				<div class="services-container">
					{{range $service := .Services}}
						<div>
							<div>
								<span>{{$service.Name}}</span>
								<span>{{$service.HelperText}}</span>
							</div>
							{{if not $.Ctx.ConfigFile}}
								<div class="menu">
									<button class="menu-button">
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="w-5 h-5">
											<path d="M3 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM8.5 10a1.5 1.5 0 113 0 1.5 1.5 0 01-3 0zM15.5 8.5a1.5 1.5 0 100 3 1.5 1.5 0 000-3z" />
										</svg>
									</button>

									<dialog>
										<a href="/admin/services/{{$service.ID}}/edit" hx-boost="true">Edit</a>
										<button onclick="document.getElementById('dialog-{{$service.ID}}').showModal();">Delete</button>
									</dialog>
								</div>
								<dialog class="modal" id="dialog-{{$service.ID}}">
									<span>Delete {{$service.Name}}</span>
									<form hx-delete="/admin/services/{{$service.ID}}" hx-swap="none">
										<div>
											<button onclick="document.getElementById('dialog-{{$service.ID}}').close(); return false;">Cancel</button>
											<button><span></span>Delete</button>
										</div>
									</form>
								</dialog>
							{{end}}
						</div>
					{{end}}
				</div>
			{{end}}
		{{end}}
	`

	tmpl, err := parseTmpl("services", markup)
	if err != nil {
		log.Printf("services.parseTmpl: %s", err)
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
		log.Printf("services.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getCreateService(w http.ResponseWriter, r *http.Request) {
	const markup = `
		{{define "title"}}Create service{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/services" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						 </a>
				  
						<h2>Create service</h2>
					</div>
				</div>

				<form hx-post hx-swap="none">
					<label>
						Name
						<input name="name" required />
					</label>

					<label>
						Helper text
						<input name="helper" />
					</label>

					<div>
						<button type="submit">Create</button>
					</div>
				</form>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getCreateService", markup)
	if err != nil {
		log.Printf("getCreateService.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Ctx pageCtx
		}{
			Ctx: getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getCreateService.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func createService(tx *sql.Tx, slug string, name string, helperText string) error {
	const query = `
		insert into service(slug, name, helper_text) values(?, ?, ?)
	`

	_, err := tx.Exec(query, slug, name, helperText)
	if err != nil {
		return fmt.Errorf("createService.Exec: %w", err)
	}

	return nil
}

func postCreateService(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	helperText := r.PostFormValue("helper")

	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postCreateService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	services, err := listServices(tx)
	if err != nil {
		log.Printf("postCreateService.listServices: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	serviceSlugs := map[string]bool{}
	for _, v := range services {
		serviceSlugs[v.Slug] = true
	}

	err = createService(tx, generateSlug(name, serviceSlugs), name, helperText)
	if err != nil {
		log.Printf("postCreateService.createService: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postCreateService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}

func deleteServiceByID(tx *sql.Tx, id int) error {
	const query = `
		delete from service where id = $1
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteServiceByID.Exec: %w", err)
	}

	return nil
}

func deleteService(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteServiceByID(tx, id)
	if err != nil {
		log.Printf("deleteService.deleteServiceByID: %s", err)
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}

func getServiceByID(tx *sql.Tx, id int) (service, error) {
	const query = `
		select id, name, helper_text from service where id = $1
	`

	service := service{}

	err := tx.QueryRow(query, id).Scan(
		&service.ID,
		&service.Name,
		&service.HelperText,
	)
	if err != nil {
		return service, fmt.Errorf("getServiceByID.Scan: %w", err)
	}

	return service, nil
}

func getEditService(w http.ResponseWriter, r *http.Request) {
	readOnly := strings.HasSuffix(r.URL.Path, "view")
	if !readOnly && metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("getEditService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	svc, err := getServiceByID(tx, id)
	if err != nil {
		log.Printf("getEditService.getServiceByID: %s", err)
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	const markup = `
		{{define "title"}}Edit service{{end}}
		{{define "body"}}
			<div class="create-service-container">
				<div class="admin-nav-header">
					<div>
						<a href="/admin/services" hx-boost="true">
							<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor">
								<path fill-rule="evenodd" d="M11.78 5.22a.75.75 0 0 1 0 1.06L8.06 10l3.72 3.72a.75.75 0 1 1-1.06 1.06l-4.25-4.25a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 0Z" clip-rule="evenodd" />
							</svg>
						</a>
					
						<h2>Edit service</h2>
					</div>
				</div>

				<form hx-post hx-swap="none" autocomplete="off">
					<label>
						Title
						<input name="name" value="{{.Service.Name}}" required />
					</label>

					<label>
						Helper text
						<input name="helper" value="{{.Service.HelperText}}">
					</label>

					<div>
						<button type="submit">Edit</button>
					</div>
				</form>
			</div>
		{{end}}
	`

	tmpl, err := parseTmpl("getEditService", markup)
	if err != nil {
		log.Printf("getEditService.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			Service service
			Ctx     pageCtx
		}{
			Service: svc,
			Ctx:     getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditService.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func editService(tx *sql.Tx, id int, name string, helperText string) error {
	const query = `
		update service set name = ?, helper_text = ? where id = ?
	`

	_, err := tx.Exec(query, name, helperText, id)
	if err != nil {
		return fmt.Errorf("editService.Exec: %w", err)
	}

	return nil
}

func updateServiceSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update service set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateServiceSlug.Exec: %w", err)
	}

	return id, nil
}

func postEditService(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	name := r.PostFormValue("name")
	helperText := r.PostFormValue("helper")

	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditService.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = editService(tx, id, name, helperText)
	if err != nil {
		log.Printf("postEditService.createService: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditService.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/services")
}
