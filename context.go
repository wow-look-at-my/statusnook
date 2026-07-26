package main

import (
	"context"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

//go:embed static/*
var staticFS embed.FS

//go:embed migrations/*
var migrationsFS embed.FS

var appWg sync.WaitGroup
var db *sql.DB
var appCtx context.Context
var cancelAppCtx context.CancelFunc
var rwDB *sql.DB
var metaSetup string
var metaName string
var metaDomain string
var metaUnconfirmedDomain string
var metaUnconfirmedDomainProblem string

var metaSSL string

var metaConfigFileEnabled bool

type statusCtxKey struct{}

type pageCtx struct {
	Status                   string
	Auth                     authCtx
	Index                    bool
	Name                     string
	HXRequest                bool
	HXBoosted                bool
	AdminArea                bool
	Nav                      string
	UnconfirmedDomainProblem string
	UnconfirmedDomain        string
	HideUnconfirmedDomain    bool
	ShouldAttemptRedirect    bool
	Domain                   string
	ConfigFile               bool
}

func getPageCtx(r *http.Request) pageCtx {
	status := ""
	if val, ok := r.Context().Value(statusCtxKey{}).(string); ok {
		status = val
	}

	authCtx := getAuthCtx(r)

	adminURLPrefix := ""
	adminArea := false
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		adminURLPrefix = strings.Split(r.URL.Path, "/")[2]
		adminArea = true
	}

	parsedURL, _ := url.ParseRequestURI("https://" + r.Host)

	return pageCtx{
		Status:                   status,
		Auth:                     authCtx,
		Index:                    r.URL.Path == "/" || r.URL.Path == "/history",
		Name:                     metaName,
		HXRequest:                r.Header.Get("HX-Request") == "true",
		HXBoosted:                r.Header.Get("HX-Boosted") == "true",
		AdminArea:                adminArea,
		Nav:                      adminURLPrefix,
		UnconfirmedDomainProblem: metaUnconfirmedDomainProblem,
		UnconfirmedDomain:        metaUnconfirmedDomain,
		HideUnconfirmedDomain:    r.URL.Path == "/admin/settings",
		ShouldAttemptRedirect: metaSSL == "true" && authCtx.ID != 0 &&
			metaDomain != "" && parsedURL.Hostname() != metaDomain,
		Domain:     metaDomain,
		ConfigFile: metaConfigFileEnabled,
	}
}

func csrfMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			h.ServeHTTP(w, r)
			return
		}

		csrfToken := r.Header.Get("csrf-token")
		authCtx := getAuthCtx(r)

		if csrfToken != authCtx.CSRFToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		h.ServeHTTP(w, r)
	})
}

func statusMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.Begin()
		if err != nil {
			log.Printf("statusMiddleware.Begin: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		severity, err := getSeverity(tx)
		if err != nil {
			tx.Rollback()
			log.Printf("statusMiddleware.getSeverity: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if err = tx.Commit(); err != nil {
			log.Printf("statusMiddleware.Commit: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		ctx := context.WithValue(r.Context(), statusCtxKey{}, severity)

		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

type authCtxKey struct{}

type authCtx struct {
	ID        int
	CSRFToken string
}

func getAuthCtx(r *http.Request) authCtx {
	ctx := authCtx{}
	if val, ok := r.Context().Value(authCtxKey{}).(authCtx); ok {
		ctx = val
	}

	return ctx
}
