package main

import (
	"log"
	"net/http"
	"net/mail"
	"strconv"

	"github.com/go-chi/chi/v5"
)

func postEditMailGroup(w http.ResponseWriter, r *http.Request) {
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
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	description := r.PostFormValue("description")

	if r.PostFormValue("members") != "" {
		for _, v := range r.Form["members"] {
			_, err := mail.ParseAddress(v)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMailGroup(tx, id, name, description)
	if err != nil {
		log.Printf("postEditMailGroup.updateMailGroup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMailGroupMembers(tx, id, r.Form["members"])
	if err != nil {
		log.Printf("postEditMailGroup.updateMailGroupMembers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}
