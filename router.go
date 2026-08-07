package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Every route and middleware the app serves. Split out of main so a test can
// drive the real router against a test database instead of a live listener.
func newRouter() *chi.Mux {
	r := chi.NewRouter()
	if BUILD == "dev" {
		r.Use(middleware.Logger)
	}

	// No CSP: the pages carry inline <script> blocks that would need nonces
	// first. The rest cost nothing. X-Frame-Options matters more than usual
	// here -- the CSRF token is injected by the page's own JavaScript, so a
	// framed admin's click carries a valid token.
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.ServeHTTP(w, r)
		})
	})

	if BUILD == "release" && metaSSL.Load() == "true" {
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if certmagic.DefaultACME.HandleHTTPChallenge(w, r) {
					return
				}

				if r.TLS == nil {
					toURL := "https://"

					requestHost, _, err := net.SplitHostPort(r.Host)
					if err != nil {
						requestHost = r.Host
					}

					toURL += requestHost
					toURL += r.URL.RequestURI()

					w.Header().Set("Connection", "close")

					http.Redirect(w, r, toURL, http.StatusFound)

					return
				}
				h.ServeHTTP(w, r)
			})
		})
	}
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// /healthz is exempt so a probe still answers on an instance
			// nobody has finished setting up.
			if metaSetup.Load() != "done" &&
				!strings.HasPrefix(r.URL.Path, "/setup") &&
				!strings.HasPrefix(r.URL.Path, "/static") &&
				r.URL.Path != "/healthz" {
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Neither static assets nor the health probe have a session to
			// look up, and this middleware opens a transaction for every
			// request that reaches it -- every image and font included.
			if strings.HasPrefix(r.URL.Path, "/static") || r.URL.Path == "/healthz" {
				h.ServeHTTP(w, r)
				return
			}

			tx, err := db.Begin()
			if err != nil {
				log.Printf("adminMiddleware.Begin: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			sessionToken, err := r.Cookie("session")
			if err != nil {
				tx.Rollback()
				h.ServeHTTP(w, r)
				return
			}

			id, csrfToken, err := validateSession(tx, sessionToken.Value)
			if err != nil {
				tx.Rollback()
				if !errors.Is(err, sql.ErrNoRows) {
					log.Printf("adminMiddleware.validateSession: %s", err)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				h.ServeHTTP(w, r)
				return
			}

			if err = tx.Commit(); err != nil {
				log.Printf("adminMiddleware.Commit: %s", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			ctx := context.WithValue(
				r.Context(),
				authCtxKey{},
				authCtx{
					ID:        id,
					CSRFToken: csrfToken,
				},
			)

			h.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	fs := http.FileServer(http.FS(staticFS))
	r.Get("/static/*", neuter(fs).ServeHTTP)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			log.Printf("healthz.Ping: %s", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("database unreachable"))
			return
		}
		w.Write([]byte("ok"))
	})
	r.Route("/", func(r chi.Router) {
		r.Use(statusMiddleware)
		r.Get("/", index)
		r.Get("/resolve", getResolve)
		r.Get("/cross-auth", getCrossAuth)
		r.Get("/history", history)
		r.Get("/unsubscribe", getUnsubscribe)
		r.Post("/unsubscribe", postUnsubscribe)
		r.Post("/resubscribe", postResubscribe)
		r.Get("/invitation/{token}", getInvitation)
		r.Post("/invitation/{token}", postInvitation)
		r.Post("/github-config-webhook", configWebhook)
	})
	r.Route("/admin", func(r chi.Router) {
		r.Use(csrfMiddleware)
		r.Use(statusMiddleware)
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := getAuthCtx(r)

				if ctx.ID == 0 {
					http.Redirect(w, r, "/login", http.StatusFound)
					return
				}

				h.ServeHTTP(w, r)
			})
		})
		r.Get("/", adminIndex)
		r.Post("/resolve", postResolve)
		r.Route("/alerts", func(r chi.Router) {
			r.Get("/", alerts)
			r.Route("/notifications", func(r chi.Router) {
				r.Get("/", getAlertNotifications)
				r.Post("/", postAlertNotifications)
			})
			r.Get("/{id}", getAlert)
			r.Delete("/{id}", deleteAlert)
			r.Get("/create", getCreateAlert)
			r.Post("/create", postCreateAlert)
			r.Get("/{id}/edit", getEditAlert)
			r.Post("/{id}/edit", postEditAlert)
			r.Get("/{id}/messages", getAddAlertMessage)
			r.Post("/{id}/messages", postAddAlertMessage)
			r.Post("/{id}/resolve", postResolveAlert)
			r.Post("/{id}/unresolve", postUnresolveAlert)
			r.Delete("/{id}/messages/{messageID}", deleteAlertMessage)
			r.Get("/{id}/messages/{messageID}", getEditAlertMessage)
			r.Post("/{id}/messages/{messageID}", postEditAlertMessage)
		})
		r.Route("/monitors", func(r chi.Router) {
			r.Get("/", monitors)
			r.Get("/{id}", getMonitor)
			r.Get("/{id}/all", getMonitorAllLogs)
			r.Get("/{id}/poll", getMonitorPoll)
			r.Delete("/{id}", deleteMonitor)
			r.Get("/create", getCreateMonitor)
			r.Post("/create", postCreateMonitor)
			r.Get("/{id}/edit", getEditMonitor)
			r.Post("/{id}/edit", postEditMonitor)
			r.Get("/{id}/view", getDetailsMonitor)
		})
		r.Route("/services", func(r chi.Router) {
			r.Get("/", services)
			r.Get("/create", getCreateService)
			r.Post("/create", postCreateService)
			r.Delete("/{id}", deleteService)
			r.Get("/{id}/edit", getEditService)
			r.Post("/{id}/edit", postEditService)
		})
		r.Route("/notifications", func(r chi.Router) {
			r.Get("/", notifications)
			r.Get("/create", getCreateNotification)
			r.Post("/create", postCreateNotification)
			r.Delete("/{id}", deleteNotificationChannel)
			r.Get("/{id}/edit", getEditNotification)
			r.Post("/{id}/edit", postEditNotification)
			r.Get("/{id}/view", getViewNotification)
			r.Route("/mail-groups", func(r chi.Router) {
				r.Get("/create", getCreateMailGroup)
				r.Post("/create", postCreateMailGroup)
				r.Get("/{id}/edit", getEditMailGroup)
				r.Post("/{id}/edit", postEditMailGroup)
				r.Get("/{id}/view", getViewMailGroup)
				r.Delete("/{id}", deleteMailGroup)
			})
		})
		r.Route("/update", func(r chi.Router) {
			r.Get("/", update)
			r.Get("/check", updateCheck)
			r.Get("/after-update", afterUpdate)
			r.Post("/", postUpdate)
		})
		r.Route("/settings", func(r chi.Router) {
			r.Get("/", getSettings)
			r.Post("/", postSettings)
			r.Post("/cancel-domain", postSettingsCancelDomain)
			r.Get("/users/{id}/edit", getEditUser)
			r.Post("/users/{id}/edit", postEditUser)
			r.Delete("/users/{id}", deleteUser)
			r.Post("/users/invite", postInviteUser)
			r.Delete("/users/invite/{id}", postDeleteInvite)
			r.Post("/config", postConfig)
			r.Route("/config-settings", func(r chi.Router) {
				r.Get("/", getConfigSettings)
				r.Post("/", postConfigSettings)
				r.Post("/generate-webhook-secret", postGenerateWebhookSecret)
			})
			r.Post("/secrets", postSecret)
		})
	})
	r.Route("/login", func(r chi.Router) {
		r.Get("/", getLogin)
		r.Post("/", postLogin)
	})
	r.Route("/logout", func(r chi.Router) {
		r.Use(csrfMiddleware)
		r.Post("/", logout)
	})
	r.Route("/setup", func(r chi.Router) {
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Every failure below writes a status. Returning bare sent a 200
				// with an empty body, so a database fault mid-setup rendered as a
				// blank page and the wizard looked like it had simply stopped.
				tx, err := db.Begin()
				if err != nil {
					log.Printf("Setup.Begin: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				defer tx.Rollback()

				v, err := getMetaValue(tx, "setup")
				if err != nil {
					log.Printf("Setup.getMetaValue: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("Setup.Commit: %s", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				if v == "done" {
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}

				if r.URL.Path == "/setup/statusnook" {
					h.ServeHTTP(w, r)
					return
				}

				if v == "domain" && r.URL.Path == "/setup/skip-domain" {
					h.ServeHTTP(w, r)
					return
				}

				endpoints := map[string]string{
					"domain":  "/setup/domain",
					"account": "/setup/account",
					"name":    "/setup/name",
					"done":    "/",
				}

				url, ok := endpoints[v]
				if !ok {
					log.Printf("Setup.endpoints: no endpoint for setup state %q", v)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				if r.URL.Path == "/setup" || r.URL.Path != url {
					if r.Method == http.MethodGet {
						http.Redirect(w, r, url, http.StatusFound)
					} else {
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}

				h.ServeHTTP(w, r)
			})
		})
		r.Post("/statusnook", postSetupStatusnook)
		r.Options("/statusnook", postSetupStatusnook)
		r.Get("/domain", getSetupDomain)
		r.Post("/domain", postSetupDomain)
		r.Post("/skip-domain", postSetupDomainSkip)
		r.Get("/account", getSetupAccount)
		r.Post("/account", postSetupAccount)
		r.Get("/name", getSetupName)
		r.Post("/name", postSetupName)
	})
	r.Get("/callback/slack", slackOAuth2Callback)
	r.Post("/subscribe/email", postSubscribeEmail)
	r.Get("/subscribe/email/confirm", getSubscribeEmailConfirm)
	r.Post("/subscribe/email/confirm", postSubscribeEmailConfirm)

	return r
}
