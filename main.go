package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/caddyserver/certmagic"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	portFlag := flag.Int("port", 80, "")
	selfSignedFlag := flag.Bool("generate-self-signed-cert", false, "")

	flag.Parse()

	envCfg, err := loadEnv()
	if err != nil {
		log.Fatalf("configuration error: %s", err)
	}
	env = envCfg

	// -port wins over STATUSNOOK_PORT when both are given.
	envPort, err := envInt("PORT", 0)
	if err != nil {
		log.Fatalf("configuration error: %s", err)
	}
	if envPort != 0 {
		if envPort < 1 || envPort > 65535 {
			log.Fatalf("configuration error: STATUSNOOK_PORT must be 1-65535, got %d", envPort)
		}
		if !isFlagSet("port") {
			*portFlag = envPort
		}
	}

	if err := os.MkdirAll(dataDir(), 0o700); err != nil {
		log.Fatalf("main.MkdirAll: %s", err)
	}
	migrateLegacyTLSPaths()

	if *selfSignedFlag {
		GenerateSelfSignedCertificate()
		return
	}

	db = initDB(false)

	rwDB = initDB(true)
	rwDB.SetMaxOpenConns(1)

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("main.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	setup, err := getMetaValue(tx, "setup")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueSetup: %s", err)
		return
	}
	metaSetup = setup

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueName: %s", err)
		return
	}
	metaName = name

	domain, err := getMetaValue(tx, "domain")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueDomain: %s", err)
		return
	}
	metaDomain = domain

	unconfirmedDomain, err := getMetaValue(tx, "unconfirmedDomain")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueUnconfirmedDomain: %s", err)
		return
	}
	metaUnconfirmedDomain = unconfirmedDomain

	unconfirmedDomainProblem, err := getMetaValue(tx, "unconfirmedDomainProblem")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueUnconfirmedDomainProblem: %s", err)
		return
	}
	metaUnconfirmedDomainProblem = unconfirmedDomainProblem

	configFileEnabled, err := getMetaValue(tx, "configFileEnabled")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueConfigFileEnabled: %s", err)
		return
	}
	if configFileEnabled == "true" {
		metaConfigFileEnabled = true
	}

	ssl, err := getMetaValue(tx, "ssl")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Fatalf("main.getMetaValueSSL: %s", err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		metaSSL = "true"
		if BUILD == "dev" || *portFlag != 80 {
			metaSSL = "false"
		}

		err = updateMetaValue(tx, "ssl", metaSSL)
		if err != nil {
			log.Printf("main.updateMetaValueSSL: %s", err)
			return
		}
	} else {
		metaSSL = ssl
	}

	_, err = getMetaValue(tx, "secretKey")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Fatalf("main.getMetaValueSecretKey: %s", err)
			return
		}

		keyBytes := make([]byte, 32)
		_, err = rand.Read(keyBytes)
		if err != nil {
			log.Printf("main.Read: %s", err)
			return
		}

		keyB64 := base64.StdEncoding.EncodeToString(keyBytes)

		err = updateMetaValue(tx, "secretKey", keyB64)
		if err != nil {
			log.Printf("main.updateMetaValueSecretKey: %s", err)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Fatalf("main.Commit: %s", err)
		return
	}

	// The environment is applied through the write pool: it changes settings.
	bootstrapTx, err := rwDB.Begin()
	if err != nil {
		log.Fatalf("main.BeginBootstrap: %s", err)
	}
	if err := applyEnvBootstrap(bootstrapTx); err != nil {
		bootstrapTx.Rollback()
		log.Fatalf("main.applyEnvBootstrap: %s", err)
	}
	if err := bootstrapTx.Commit(); err != nil {
		log.Fatalf("main.CommitBootstrap: %s", err)
	}

	r := chi.NewRouter()
	if BUILD == "dev" {
		r.Use(middleware.Logger)
	}
	r.Use(securityHeaders)
	r.Use(limitRequestBody)
	if BUILD == "release" && metaSSL == "true" {
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
			if metaSetup != "done" &&
				r.URL.Path != "/healthz" &&
				!strings.HasPrefix(r.URL.Path, "/setup") &&
				!strings.HasPrefix(r.URL.Path, "/static") {
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	r.Use(func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	r.Get("/healthz", health)
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
				tx, err := db.Begin()
				if err != nil {
					log.Printf("Setup.Begin: %s", err)
					return
				}
				defer tx.Rollback()

				v, err := getMetaValue(tx, "setup")
				if err != nil {
					log.Printf("Setup.getMetaValue: %s", err)
					return
				}

				err = tx.Commit()
				if err != nil {
					log.Printf("Setup.Commit: %s", err)
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
					log.Printf("Setup.endpoints: no endpoint")
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

	appCtx, cancelAppCtx = context.WithCancel(context.Background())

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	appWg.Add(1)
	go monitorLoop(appCtx, &appWg)

	appWg.Add(1)
	go notificationLoop(appCtx, &appWg)

	appWg.Add(1)
	go sessionCleanupLoop(appCtx, &appWg)

	if env.GitHub.Managed() {
		// Pull the config before serving so monitors start from the
		// repository's current state rather than the last synced copy.
		src := envGitHubConfigSource()
		if _, err := syncGitHubConfig(appCtx, src); err != nil {
			log.Printf(
				"initial github config sync failed, will retry: %s",
				describeGitHubError(err),
			)
		} else {
			log.Printf(
				"github config: watching %s (%s) at %s",
				src.Repo, branchLabel(src.Branch), src.Path,
			)
		}

		appWg.Add(1)
		go gitHubConfigSyncLoop(appCtx, &appWg)
	}

	var httpServer *http.Server
	var httpsServer *http.Server

	if BUILD == "dev" {
		httpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", 8000))
		if err != nil {
			log.Fatalf("main.ListenHTTPS: %s", err)
		}

		httpServer = &http.Server{
			Handler:     r,
			BaseContext: func(listener net.Listener) context.Context { return appCtx },
		}

		go httpServer.Serve(httpLn)
	} else {
		host := ""
		if !*dockerFlag && *portFlag != 80 {
			host = "127.0.0.1"
		}

		httpLn, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, *portFlag))
		if err != nil {
			log.Fatalf("main.ListenHTTP: %s", err)
		}

		httpServer = &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      2 * time.Minute,
			IdleTimeout:       5 * time.Minute,
			Handler:           r,
			BaseContext:       func(listener net.Listener) context.Context { return appCtx },
		}

		go httpServer.Serve(httpLn)

		if metaSSL == "true" {
			certmagic.Default.Storage = &certmagic.FileStorage{Path: certmagicDir()}
			certmagic.DefaultACME.Agreed = true
			certmagic.DefaultACME.CA = CA
			certmagic.DefaultACME.Email = " "

			domains := []string{}
			if domain != "" {
				domains = append(domains, metaDomain)
			}

			tlsConfig, err := certmagic.TLS(domains)
			if err != nil {
				log.Fatalf("main.TLS: %s", err)
			}
			tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)
			getCertificateCertMagic := tlsConfig.GetCertificate
			tlsConfig.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				certificate, err := getCertificateCertMagic(clientHello)
				if err != nil {
					certificate, err := tls.LoadX509KeyPair(
						selfSignedCertPath(),
						selfSignedKeyPath(),
					)
					if err != nil {
						log.Printf("main.LoadX509KeyPair: %s", err)
						return &certificate, err
					}

					return &certificate, nil
				}

				return certificate, nil
			}

			httpsLn, err := tls.Listen("tcp", fmt.Sprintf(":%d", 443), tlsConfig)
			if err != nil {
				log.Fatalf("main.ListenHTTPS: %s", err)
			}

			httpsServer = &http.Server{
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      2 * time.Minute,
				IdleTimeout:       5 * time.Minute,
				Handler:           r,
				BaseContext:       func(listener net.Listener) context.Context { return appCtx },
			}

			go httpsServer.Serve(httpsLn)

			if unconfirmedDomain != "" && unconfirmedDomainProblem == "" {
				appWg.Add(1)
				go monitorUnconfirmedDomainLoop(appCtx, &appWg)
			}
		}
	}

	<-shutdownCh
	cancelAppCtx()
	appWg.Wait()

	if err := httpServer.Shutdown(context.Background()); err != nil {
		panic(err)
	}

	if httpsServer != nil {
		if err := httpsServer.Shutdown(context.Background()); err != nil {
			panic(err)
		}
	}

	err = db.Close()
	if err != nil {
		log.Printf("main.DBClose: %s", err)
	}

	err = rwDB.Close()
	if err != nil {
		log.Printf("main.rwDBClose: %s", err)
	}
}
