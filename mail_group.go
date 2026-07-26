package main

import (
	"database/sql"
	"fmt"
	"github.com/go-chi/chi/v5"
	"log"
	"net/http"
	"net/mail"
	"strconv"
)

type MailGroup struct {
	ID          int
	Slug        string
	Name        string
	Description string
}

func listMailGroups(tx *sql.Tx) ([]MailGroup, error) {
	const query = `
		select id, slug, name, description from mail_group
	`

	mailGroups := []MailGroup{}

	rows, err := tx.Query(query)
	if err != nil {
		return mailGroups, fmt.Errorf("listMailGroups.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		mailGroup := MailGroup{}
		err = rows.Scan(&mailGroup.ID, &mailGroup.Slug, &mailGroup.Name, &mailGroup.Description)
		if err != nil {
			return mailGroups, fmt.Errorf("listMailGroups.Scan: %w", err)
		}

		mailGroups = append(mailGroups, mailGroup)
	}

	return mailGroups, nil
}

func updateMonitorMailGroups(tx *sql.Tx, monitorID int, mailGroupIDs []int) error {
	const deleteQuery = `
		delete from mail_group_monitor where monitor_id = ?
	`

	_, err := tx.Exec(deleteQuery, monitorID)
	if err != nil {
		return fmt.Errorf("updateMonitorMailGroups.ExecDelete: %w", err)
	}

	if len(mailGroupIDs) > 0 {
		const baseInsertQuery = `
			insert into mail_group_monitor(mail_group_id, monitor_id) values
		`

		insertQuery := baseInsertQuery

		params := []any{}

		for i, v := range mailGroupIDs {
			insertQuery += "(?, ?)"
			if i < len(mailGroupIDs)-1 {
				insertQuery += ","
			}
			params = append(params, v, monitorID)
		}

		_, err = tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMonitorMailGroups.ExecInsert: %w", err)
		}
	}

	return nil
}

type MailGroupIDs struct {
	ID   int
	Slug string
}

func listMailGroupIDsByMonitorID(tx *sql.Tx, monitorID int) ([]MailGroupIDs, error) {
	const query = `
		select mail_group_id, slug from mail_group_monitor 
		left join mail_group on mail_group.id = mail_group_id
		where monitor_id = ?
	`

	allIds := []MailGroupIDs{}

	rows, err := tx.Query(query, monitorID)
	if err != nil {
		return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ids MailGroupIDs
		err = rows.Scan(&ids.ID, &ids.Slug)
		if err != nil {
			return allIds, fmt.Errorf("listMailGroupIDsByMonitorID.Scan: %w", err)
		}

		allIds = append(allIds, ids)
	}

	return allIds, nil
}

type MailGroupMember struct {
	ID           int
	EmailAddress string
}

func listMailGroupMembersByID(tx *sql.Tx, id int) ([]MailGroupMember, error) {
	const query = `
		select id, email_address from mail_group_member where mail_group_id = ?
	`

	members := []MailGroupMember{}

	rows, err := tx.Query(query, id)
	if err != nil {
		return members, fmt.Errorf("listMailGroupMembersByID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		member := MailGroupMember{}
		err = rows.Scan(&member.ID, &member.EmailAddress)
		if err != nil {
			return members, fmt.Errorf("listMailGroupMembersByID.Scan: %w", err)
		}

		members = append(members, member)
	}

	return members, nil
}

func listMailGroupMembersEmailsByMonitorID(tx *sql.Tx, id int) ([]string, error) {
	const query = `
		select distinct email_address from mail_group_member
		left join mail_group on mail_group.id = mail_group_member.mail_group_id
		left join mail_group_monitor on mail_group_monitor.mail_group_id = mail_group.id
		where mail_group_monitor.monitor_id = ?
	`

	emails := []string{}

	rows, err := tx.Query(query, id)
	if err != nil {
		return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var email string
		err = rows.Scan(&email)
		if err != nil {
			return emails, fmt.Errorf("listMailGroupMembersEmailsByMonitorID.Scan: %w", err)
		}

		emails = append(emails, email)
	}

	return emails, nil
}

func getMailGroupByID(tx *sql.Tx, id int) (MailGroup, error) {
	const query = `
		select id, name, description from mail_group where id = ?
	`

	mailGroup := MailGroup{}

	err := tx.QueryRow(query, id).Scan(&mailGroup.ID, &mailGroup.Name, &mailGroup.Description)
	if err != nil {
		return mailGroup, fmt.Errorf("getMailGroupByID.QueryRow: %w", err)
	}

	return mailGroup, nil
}

func createMailGroup(tx *sql.Tx, slug string, name string, description string) (int, error) {
	const query = `
		insert into mail_group(slug, name, description) values(?, ?, ?) returning id
	`

	var id int

	err := tx.QueryRow(query, slug, name, description).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("createMailGroup.QueryRow: %w", err)
	}

	return id, nil
}

func updateMailGroup(tx *sql.Tx, id int, name string, description string) error {
	const query = `
		update mail_group set name = ?, description = ? where id = ?
	`

	_, err := tx.Exec(query, name, description, id)
	if err != nil {
		return fmt.Errorf("updateMailGroup.Exec: %w", err)
	}

	return nil
}

func updateMailGroupSlug(tx *sql.Tx, old string, new string) (int, error) {
	const query = `
		update mail_group set slug = ? where slug = ? returning id
	`

	var id int

	err := tx.QueryRow(query, new, old).Scan(&id)
	if err != nil {
		return id, fmt.Errorf("updateMailGroupSlug.QueryRow: %w", err)
	}

	return id, nil
}

func updateMailGroupMembers(tx *sql.Tx, id int, members []string) error {
	const deleteQuery = `
		delete from mail_group_member where mail_group_id = ?
	`

	_, err := tx.Exec(deleteQuery, id)
	if err != nil {
		return fmt.Errorf("updateMailGroupMembers.ExecDelete: %w", err)
	}

	if len(members) > 0 {
		const baseInsertQuery = `
			insert into mail_group_member(email_address, mail_group_id)
			values
		`

		insertQuery := baseInsertQuery

		for i := range members {
			insertQuery += "(?, ?)"

			if i != len(members)-1 {
				insertQuery += ","
			}
		}

		params := []any{}
		for _, v := range members {
			params = append(params, v, id)
		}

		insertQuery += " on conflict (mail_group_id, email_address) do nothing"

		_, err := tx.Exec(insertQuery, params...)
		if err != nil {
			return fmt.Errorf("updateMailGroupMembers.ExecInsert: %w", err)
		}
	}

	return nil
}

func postCreateMailGroup(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled {
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
		log.Printf("postCreateMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	mailGroups, err := listMailGroups(tx)
	if err != nil {
		log.Printf("postCreateMailGroup.listMailGroups: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	mailGroupSlugs := map[string]bool{}
	for _, v := range mailGroups {
		mailGroupSlugs[v.Slug] = true
	}

	id, err := createMailGroup(tx, generateSlug(name, mailGroupSlugs), name, description)
	if err != nil {
		log.Printf("postCreateMailGroup.createMailGroup: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = updateMailGroupMembers(tx, id, r.Form["members"])
	if err != nil {
		log.Printf("postCreateMailGroup.updateMailGroupMembers: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postCreateMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func deleteMailGroupByID(tx *sql.Tx, id int) error {
	const query = `
		delete from mail_group where id = ?
	`

	_, err := tx.Exec(query, id)
	if err != nil {
		return fmt.Errorf("deleteMailGroupByID.Exec: %w", err)
	}

	return nil
}

func deleteMailGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("deleteMailGroup.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteMailGroupByID(tx, id)
	if err != nil {
		log.Printf("deleteMailGroup.deleteMailGroupByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteMailGroup.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func postEditMailGroup(w http.ResponseWriter, r *http.Request) {
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
