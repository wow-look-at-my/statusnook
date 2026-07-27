package main

import (
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"strconv"
	"strings"
)

func getCreateMailGroup(w http.ResponseWriter, r *http.Request) {
	tmpl, err := parseTmpl("get_create_mail_group.html")
	if err != nil {
		log.Printf("getCreateMailGroup.parseTmpl: %s", err)
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
		log.Printf("getCreateMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getEditMailGroup(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("getEditMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	mailGroup, err := getMailGroupByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.getMailGroupByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroupMembers, err := listMailGroupMembersByID(tx, id)
	if err != nil {
		log.Printf("getEditMailGroup.listMailGroupMembersByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("getEditMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	tmpl, err := parseTmpl("get_edit_mail_group.html")
	if err != nil {
		log.Printf("getEditMailGroup.parseTmpl: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(
		w,
		struct {
			MailGroup        MailGroup
			MailGroupMembers []MailGroupMember
			ReadOnly         bool
			Ctx              pageCtx
		}{
			MailGroup:        mailGroup,
			MailGroupMembers: mailGroupMembers,
			ReadOnly:         readOnly,
			Ctx:              getPageCtx(r),
		},
	)
	if err != nil {
		log.Printf("getEditMailGroup.Execute: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func getViewMailGroup(w http.ResponseWriter, r *http.Request) {
	getEditMailGroup(w, r)
}
