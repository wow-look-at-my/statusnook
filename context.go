package main

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"embed"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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

		if authCtx.CSRFToken == "" || !hmac.Equal([]byte(csrfToken), []byte(authCtx.CSRFToken)) {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		h.ServeHTTP(w, r)
	})
}

// maxRequestBody bounds any request body. The config editor posts the whole
// config document, which is the largest legitimate body.
const maxRequestBody = 2 * maxConfigSize

// limitRequestBody stops an unauthenticated client from making the process
// read an unbounded body into memory. The webhook applies its own, smaller
// limit before it verifies the signature.
func limitRequestBody(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}

		h.ServeHTTP(w, r)
	})
}

// securityHeaders sets the headers every response should carry. There is no
// Content-Security-Policy: the UI relies on inline scripts and styles
// throughout, so a policy permissive enough to work would not add protection.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")

		// Framing is only denied where clicks are dangerous. Status pages get
		// embedded in other people's dashboards on purpose.
		if isAuthenticatedArea(r.URL.Path) {
			header.Set("X-Frame-Options", "SAMEORIGIN")
		}

		if requestIsHTTPS(r) {
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}

		h.ServeHTTP(w, r)
	})
}

// isAuthenticatedArea reports whether a path belongs to the admin surface.
func isAuthenticatedArea(path string) bool {
	for _, prefix := range []string{"/admin", "/login", "/logout", "/setup"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}

	return false
}

// requestIsHTTPS reports whether the browser reached Statusnook over TLS,
// consulting X-Forwarded-Proto only when a proxy is trusted. It decides whether
// session cookies may carry the Secure attribute: setting Secure on a plain
// HTTP deployment (a NAS reached at http://nas.local:8000) makes the browser
// drop the cookie, which locks the operator out of their own admin area.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	if !env.TrustProxy {
		return false
	}

	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.Index(proto, ","); i != -1 {
		proto = proto[:i]
	}

	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// sessionCookie builds the session cookie for a request.
func sessionCookie(r *http.Request, token string) *http.Cookie {
	return &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		Expires:  time.Now().UTC().Add(sessionLifetime),
		Secure:   requestIsHTTPS(r),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
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
