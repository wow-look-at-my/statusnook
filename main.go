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
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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

	r := newRouter()

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
