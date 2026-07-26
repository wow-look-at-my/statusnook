package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/acme"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Everything Statusnook persists lives under the data directory, so a single
// Docker volume covers the database and the TLS material.

// certmagicDir is where managed certificates are cached.
func certmagicDir() string {
	return filepath.Join(dataDir(), "certmagic")
}

func selfSignedCertPath() string {
	return filepath.Join(dataDir(), SELF_SIGNED_CERT_NAME)
}

func selfSignedKeyPath() string {
	return filepath.Join(dataDir(), SELF_SIGNED_KEY_NAME)
}

// migrateLegacyTLSPaths moves TLS material written next to the binary by older
// versions into the data directory, once.
func migrateLegacyTLSPaths() {
	moves := map[string]string{
		"certmagic":           certmagicDir(),
		SELF_SIGNED_CERT_NAME: selfSignedCertPath(),
		SELF_SIGNED_KEY_NAME:  selfSignedKeyPath(),
	}

	for legacy, target := range moves {
		if legacy == target {
			continue
		}
		if _, err := os.Stat(legacy); err != nil {
			continue
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := os.Rename(legacy, target); err != nil {
			log.Printf("migrateLegacyTLSPaths %s: %s", legacy, err)
			continue
		}
		log.Printf("moved %s to %s", legacy, target)
	}
}

func attemptCertificateAcquisition(ctx context.Context, domain string) error {
	var testCache *certmagic.Cache
	testCache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(cert certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(testCache, certmagic.Config{}), nil
		},
	})
	testCache.Stop()

	testMagic := certmagic.New(testCache, certmagic.Config{})

	testACME := certmagic.NewACMEIssuer(testMagic, certmagic.ACMEIssuer{
		CA:     certmagic.LetsEncryptStagingCA,
		Email:  " ",
		Agreed: true,
	})

	testMagic.Issuers = []certmagic.Issuer{testACME}

	err := testMagic.ObtainCertSync(ctx, domain)
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.ObtainCertSync: %w", err)
	}

	fileStorage, ok := certmagic.Default.Storage.(*certmagic.FileStorage)
	if !ok {
		return fmt.Errorf("attemptCertificateAcquisition.FileStorageAssert: %w", err)
	}

	err = os.RemoveAll(
		fileStorage.Filename(certmagic.StorageKeys.CertsPrefix(testACME.IssuerKey())),
	)
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.RemoveAll: %w", err)
	}

	err = certmagic.ManageSync(ctx, []string{domain})
	if err != nil {
		return fmt.Errorf("attemptCertificateAcquisition.ManageSync: %w", err)
	}

	return nil
}

func monitorUnconfirmedDomainLoop(ctx context.Context, wg *sync.WaitGroup) {
	tick := time.Tick(time.Minute * 1)

	for {
		if metaUnconfirmedDomain == "" || metaUnconfirmedDomainProblem != "" {
			wg.Done()
			return
		}

		select {
		case <-tick:
			func() {
				found, err := lookupDomain(metaUnconfirmedDomain)
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.lookupDomain: %s", err)
					return
				}

				if !found {
					return
				}

				err = attemptCertificateAcquisition(ctx, metaUnconfirmedDomain)
				if err != nil {
					unconfirmedDomainProblemMsg := "An unexpected error occurred"

					var acmeProblem acme.Problem
					if errors.As(err, &acmeProblem) {
						var ok bool
						unconfirmedDomainProblemMsg, ok = acmeProblemTypeMessages[acmeProblem.Type]
						if !ok {
							unconfirmedDomainProblemMsg = "An unhandled error occurred " +
								acmeProblem.Type
						}
					} else {
						log.Printf("monitorUnconfirmedDomainLoop.attemptCertificateAcquisition: %s", err)
					}

					tx, err := rwDB.Begin()
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.BeginUnconfirmedDomainProblem: %s", err)
						return
					}
					defer tx.Rollback()

					metaUnconfirmedDomainProblem = unconfirmedDomainProblemMsg
					err = updateMetaValue(tx, "unconfirmedDomainProblem", metaUnconfirmedDomainProblem)
					if err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.UpdateUnconfirmedDomainProblem: %s", err)
						return
					}

					if err := tx.Commit(); err != nil {
						log.Printf("monitorUnconfirmedDomainLoop.CommitUnconfirmedDomainProblem: %s", err)
						return
					}

					return
				}

				tx, err := rwDB.Begin()
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Begin: %s", err)
					return
				}
				defer tx.Rollback()

				metaDomain = metaUnconfirmedDomain
				err = updateMetaValue(tx, "domain", metaUnconfirmedDomain)
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueDomain: %s", err)
					return
				}

				metaUnconfirmedDomain = ""
				err = updateMetaValue(tx, "unconfirmedDomain", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomain: %s", err)
					return
				}

				metaUnconfirmedDomainProblem = ""
				err = updateMetaValue(tx, "unconfirmedDomainProblem", "")
				if err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.updateMetaValueUnconfirmedDomainProblem: %s", err)
					return
				}

				if err := tx.Commit(); err != nil {
					log.Printf("monitorUnconfirmedDomainLoop.Commit: %s", err)
					return
				}
			}()
		case <-ctx.Done():
			wg.Done()
			return
		}
	}
}

func GenerateSelfSignedCertificate() {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("Failed to generate key: %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Fatalf("Failed to generate serial number: %v", err)
	}

	notBefore := time.Now().UTC()
	notAfter := notBefore.Add(time.Hour * 720)

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Statusnook Installer"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		log.Fatalf("Failed to create certificate: %v", err)
	}

	fingerprint := sha256.Sum256(derBytes)
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	certFile, err := os.Create(selfSignedCertPath())
	if err != nil {
		log.Fatalf("Failed to open %s for writing: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		log.Fatalf("Failed to write data to %s: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := certFile.Close(); err != nil {
		log.Fatalf("Error closing %s: %v", SELF_SIGNED_CERT_NAME, err)
	}

	keyOut, err := os.OpenFile(selfSignedKeyPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Failed to open %s for writing: %v", SELF_SIGNED_KEY_NAME, err)
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		log.Fatalf("Unable to marshal private key: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		log.Fatalf("Failed to write data to %s: %v", SELF_SIGNED_KEY_NAME, err)
	}
	if err := keyOut.Close(); err != nil {
		log.Fatalf("Error closing %s: %v", SELF_SIGNED_KEY_NAME, err)
	}

	formattedFingerprint := ""
	for i := 0; i < len(fingerprintHex); i += 2 {
		formattedFingerprint +=
			strings.ToUpper(string(fingerprintHex[i])+string(fingerprintHex[i+1])) + ":"
	}
	formattedFingerprint = formattedFingerprint[:len(formattedFingerprint)-1]
	fmt.Println(formattedFingerprint)
}

var dockerFlag = flag.Bool("docker", false, "")
