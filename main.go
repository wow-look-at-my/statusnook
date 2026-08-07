package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/caddyserver/certmagic"
)

func main() {
	portFlag := flag.Int("port", 80, "")
	selfSignedFlag := flag.Bool("generate-self-signed-cert", false, "")

	flag.Parse()

	if *validateConfigFlag != "" {
		if err := validateConfig(*validateConfigFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("config ok")
		return
	}

	if *selfSignedFlag {
		GenerateSelfSignedCertificate()
		return
	}

	db = initDB(false)
	// Unbounded by default, so a burst opened an unbounded number of SQLite
	// connections, each re-running the DSN pragmas. Writes are already
	// serialized on rwDB below.
	db.SetMaxOpenConns(max(4, runtime.NumCPU()*4))
	db.SetMaxIdleConns(4)

	rwDB = initDB(true)
	rwDB.SetMaxOpenConns(1)

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("main.Begin: %s", err)
		return
	}
	defer tx.Rollback()

	sslDefault := "true"
	if BUILD == "dev" || *portFlag != 80 {
		sslDefault = "false"
	}

	if err := loadMetaState(tx, sslDefault); err != nil {
		log.Fatalf("main.loadMetaState: %s", err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatalf("main.Commit: %s", err)
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
	go retentionLoop(appCtx, &appWg)

	var httpServer *http.Server
	var httpsServer *http.Server

	if BUILD == "dev" {
		httpLn, err := net.Listen("tcp", fmt.Sprintf(":%d", 8000))
		if err != nil {
			log.Fatalf("main.ListenHTTPS: %s", err)
		}

		// Same timeouts as the release path below; the dev server had none.
		httpServer = &http.Server{
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      2 * time.Minute,
			IdleTimeout:       5 * time.Minute,
			Handler:           r,
			BaseContext:       func(listener net.Listener) context.Context { return appCtx },
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

		if metaSSL.Load() == "true" {
			certmagic.Default.Storage = &certmagic.FileStorage{Path: "certmagic"}
			certmagic.DefaultACME.Agreed = true
			certmagic.DefaultACME.CA = CA
			certmagic.DefaultACME.Email = " "

			domains := []string{}
			if metaDomain.Load() != "" {
				domains = append(domains, metaDomain.Load())
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
						SELF_SIGNED_CERT_NAME,
						SELF_SIGNED_KEY_NAME,
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

			if metaUnconfirmedDomain.Load() != "" && metaUnconfirmedDomainProblem.Load() == "" {
				appWg.Add(1)
				go monitorUnconfirmedDomainLoop(appCtx, &appWg)
			}
		}
	}

	<-shutdownCh

	// Shutdown BEFORE waiting on the loops: it stops accepting new requests
	// and drains the ones in flight, where the old order kept the listeners
	// open while the background loops were already winding down.
	//
	// Bounded, because Shutdown with a background context waits forever on a
	// single hung request and Docker or systemd then SIGKILLs instead.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("main.ShutdownHTTP: %s", err)
	}

	if httpsServer != nil {
		if err := httpsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("main.ShutdownHTTPS: %s", err)
		}
	}

	cancelAppCtx()
	appWg.Wait()

	err = db.Close()
	if err != nil {
		log.Printf("main.DBClose: %s", err)
	}

	err = rwDB.Close()
	if err != nil {
		log.Printf("main.rwDBClose: %s", err)
	}
}
