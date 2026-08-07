package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

func postEditNotification(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	idParam := chi.URLParam(r, "id")
	notificationID, err := strconv.Atoi(idParam)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postEditNotification.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	channel, err := getNotificationChannelByID(tx, notificationID)
	if err != nil {
		log.Printf("postEditNotification.getNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	displayName := r.PostFormValue("display-name")
	if displayName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if channel.Type == "smtp" {
		host := r.PostFormValue("host")
		if host == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		port := r.PostFormValue("port")
		if port == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		portNum, err := strconv.Atoi(port)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		username := r.PostFormValue("username")
		if username == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		password := r.PostFormValue("password")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		from := r.PostFormValue("from")
		if password == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, err = mail.ParseAddress(from)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		headers := map[string]string{}
		if r.PostFormValue("header-key") != "" && r.PostFormValue("header-value") != "" {
			keys := r.PostForm["header-key"]
			values := r.PostForm["header-value"]
			for i := 0; i < len(keys) && i < len(values); i++ {
				headers[keys[i]] = values[i]
			}
		}

		misc := map[string]string{}
		if strings.EqualFold(host, "smtp.postmarkapp.com") {
			txStream := r.PostFormValue("pm-transactional")
			bStream := r.PostFormValue("pm-broadcast")

			if txStream == "" || bStream == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			misc["pm-transactional"] = txStream
			misc["pm-broadcast"] = bStream
		}

		details := SMTPNotificationDetails{
			Host:     host,
			Port:     portNum,
			Username: username,
			Password: password,
			Headers:  headers,
			From:     from,
			Misc:     misc,
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSMTP: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if channel.Type == "slack" {
		webhookURL, err := url.ParseRequestURI(r.PostFormValue("webhook-url"))
		if err != nil {
			// ParseRequestURI returns a nil URL alongside the error, and the
			// code below calls String() on it.
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		details := SlackNotificationDetails{
			WebhookURL: webhookURL.String(),
		}

		serializedDetails, err := json.Marshal(details)
		if err != nil {
			log.Printf("postEditNotification.MarshalSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		err = editNotificationChannel(
			tx,
			NotificationChannel{
				ID:      channel.ID,
				Name:    displayName,
				Type:    channel.Type,
				Details: serializedDetails,
			},
		)
		if err != nil {
			log.Printf("postEditNotification.editNotificationChannelSlack: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postEditNotification.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getViewNotification(w http.ResponseWriter, r *http.Request) {
	getEditNotification(w, r)
}

func deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("deleteNotificationChannel.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = deleteNotificationChannelByID(tx, id)
	if err != nil {
		log.Printf("deleteNotificationChannel.deleteNotificationChannelByID: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("deleteNotificationChannel.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Add("HX-Location", "/admin/notifications")
}

func getCreateMailGroup(w http.ResponseWriter, r *http.Request) {

	tmpl, err := parseTmpl("getCreateMailGroup", getCreateMailGroupMarkup)
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

	tmpl, err := parseTmpl("getEditMailGroup", getEditMailGroupMarkup)
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

type MailGroup struct {
	ID          int
	Slug        string
	Name        string
	Description string
}

type MailGroupIDs struct {
	ID   int
	Slug string
}

type MailGroupMember struct {
	ID           int
	EmailAddress string
}

func postCreateMailGroup(w http.ResponseWriter, r *http.Request) {
	if metaConfigFileEnabled.Load() {
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

func deleteMailGroup(w http.ResponseWriter, r *http.Request) {
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
