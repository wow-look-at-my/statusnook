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
	"fmt"
	"log"
	"math/big"
	mathRand "math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/mholt/acmez/acme"
	"github.com/miekg/dns"
)

const SELF_SIGNED_CERT_NAME = "self-signed-cert.pem"

var metaDomain atomicString
var metaUnconfirmedDomain atomicString
var metaUnconfirmedDomainProblem atomicString

var metaSSL atomicString

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
		// err is nil here, and %w with a nil operand yields an error whose
		// text is %!w(<nil>) and which errors.As can never match -- so the
		// caller's ACME-problem branch fell through to "unexpected error".
		return errors.New("attemptCertificateAcquisition: storage is not a certmagic.FileStorage")
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

	certFile, err := os.Create(SELF_SIGNED_CERT_NAME)
	if err != nil {
		log.Fatalf("Failed to open %s for writing: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		log.Fatalf("Failed to write data to %s: %v", SELF_SIGNED_CERT_NAME, err)
	}
	if err := certFile.Close(); err != nil {
		log.Fatalf("Error closing %s: %v", SELF_SIGNED_CERT_NAME, err)
	}

	keyOut, err := os.OpenFile(SELF_SIGNED_KEY_NAME, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
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

func postSettingsCancelDomain(w http.ResponseWriter, r *http.Request) {
	v := "You cancelled the domain verification process. Please enter a new domain."

	tx, err := rwDB.Begin()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Begin: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	err = updateMetaValue(tx, "unconfirmedDomainProblem", v)
	if err != nil {
		log.Printf("postSettingsCancelDomain.updateMetaValueProblem: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("postSettingsCancelDomain.Commit: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	metaUnconfirmedDomainProblem.Store(v)

	w.Header().Add("HX-Location", "/admin/settings")
}

func lookupDomain(domain string) (bool, error) {
	rootServers := []string{
		"a.root-servers.net",
		"b.root-servers.net",
		"c.root-servers.net",
		"d.root-servers.net",
		"e.root-servers.net",
		"f.root-servers.net",
		"g.root-servers.net",
		"h.root-servers.net",
		"i.root-servers.net",
		"j.root-servers.net",
		"k.root-servers.net",
		"l.root-servers.net",
		"m.root-servers.net",
	}

	rootNS := rootServers[mathRand.Intn(len(rootServers))]
	c := &dns.Client{}
	m := &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err := c.Exchange(m, rootNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.rootNS %s %s: %w", domain, rootNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	authorityNS, ok := randomNS(r.Ns)
	if !ok {
		return false, nil
	}
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, authorityNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.authorityNS %s %s: %w", domain, authorityNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	domainNS, ok := randomNS(r.Ns)
	if !ok {
		return false, nil
	}
	m = &dns.Msg{}
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.SetEdns0(4096, false)
	r, _, err = c.Exchange(m, domainNS+":53")
	if err != nil {
		return false, fmt.Errorf("lookupDomain.domainNS %s %s: %w", domain, domainNS, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return false, nil
	}

	return len(r.Answer) > 0, nil
}

var acmeProblemTypeMessages = map[string]string{
	acme.ProblemTypeDNS:                "Let's Encrypt can't find your domain's DNS record, verify it exists and then retry",
	acme.ProblemTypeConnection:         "Let's Encrypt could not reach your server, ensure your server is publicly accessible on ports 80 and 443, then try again",
	acme.ProblemTypeRejectedIdentifier: "Let's Encrypt will not issue certificates for this domain",
}
