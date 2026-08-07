package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

type StatusnookConfigService struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

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

	if err := rows.Err(); err != nil {
		return services, fmt.Errorf("listServices.RowsErr: %w", err)
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

	tmpl, err := parseTmpl("services", servicesMarkup)
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

	tmpl, err := parseTmpl("getCreateService", getCreateServiceMarkup)
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
	if metaConfigFileEnabled.Load() {
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
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

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
		// Without the return this committed an empty transaction and
		// redirected as though the delete had worked.
		log.Printf("deleteService.deleteServiceByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
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
	if !readOnly && metaConfigFileEnabled.Load() {
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

	tmpl, err := parseTmpl("getEditService", getEditServiceMarkup)
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
	if metaConfigFileEnabled.Load() {
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
