package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// An SMTP send is how an outage actually reaches anyone, and none of it can be
// exercised without a server that speaks the protocol. net/smtp upgrades to TLS
// whenever the server advertises STARTTLS and verifies the certificate against
// the system pool, and statusnook's own auth refuses to send credentials over a
// connection that is not encrypted -- so the fake has to present a certificate
// the process trusts.
//
// crypto/x509 reads SSL_CERT_FILE for the system pool on Linux and caches the
// result on first use, which is why this is done in TestMain rather than in the
// test that needs it.
var fakeSMTPTLS tls.Certificate

func TestMain(m *testing.M) {
	cert, pemBytes, err := generateLocalhostCertificate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: generate certificate:", err)
		os.Exit(1)
	}
	fakeSMTPTLS = cert

	path := filepath.Join(os.TempDir(), "statusnook-test-ca.pem")
	if err := os.WriteFile(path, pemBytes, 0600); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: write certificate:", err)
		os.Exit(1)
	}
	os.Setenv("SSL_CERT_FILE", path)

	code := m.Run()
	os.Remove(path)
	os.Exit(code)
}

func generateLocalhostCertificate() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	pair, err := tls.X509KeyPair(certPEM, keyPEM)

	return pair, certPEM, err
}

// A received message: who it was for and the DATA payload.
type sentMail struct {
	from string
	to   []string
	body string
}

type fakeSMTP struct {
	t        *testing.T
	listener net.Listener
	port     int

	mu   sync.Mutex
	sent []sentMail
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newFakeSMTP.Listen: %s", err)
	}

	s := &fakeSMTP{t: t, listener: listener, port: listener.Addr().(*net.TCPAddr).Port}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()

	return s
}

func (s *fakeSMTP) messages() []sentMail {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]sentMail(nil), s.sent...)
}

// Enough of RFC 5321 for net/smtp's SendMail: greeting, EHLO with STARTTLS and
// AUTH advertised, the TLS upgrade, PLAIN auth, the envelope, and DATA.
func (s *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	write := func(format string, args ...any) bool {
		_, err := fmt.Fprintf(conn, format+"\r\n", args...)

		return err == nil
	}

	if !write("220 localhost ESMTP fake") {
		return
	}

	mail := sentMail{}
	upgraded := false

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		command := strings.ToUpper(strings.TrimSpace(line))

		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if upgraded {
				write("250-localhost")
				write("250 AUTH PLAIN LOGIN")
				continue
			}
			write("250-localhost")
			write("250 STARTTLS")
		case command == "STARTTLS":
			write("220 ready")
			tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{fakeSMTPTLS}})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
			upgraded = true
		case strings.HasPrefix(command, "AUTH"):
			write("235 accepted")
		case strings.HasPrefix(command, "MAIL FROM:"):
			mail.from = addressIn(line)
			write("250 ok")
		case strings.HasPrefix(command, "RCPT TO:"):
			mail.to = append(mail.to, addressIn(line))
			write("250 ok")
		case command == "DATA":
			write("354 send it")

			body := strings.Builder{}
			for {
				dataLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dataLine, "\r\n") == "." {
					break
				}
				body.WriteString(dataLine)
			}

			mail.body = body.String()

			s.mu.Lock()
			s.sent = append(s.sent, mail)
			s.mu.Unlock()

			mail = sentMail{}
			write("250 queued")
		case command == "QUIT":
			write("221 bye")

			return
		default:
			write("250 ok")
		}
	}
}

func addressIn(line string) string {
	start := strings.Index(line, "<")
	end := strings.Index(line, ">")
	if start < 0 || end < start {
		return strings.TrimSpace(line)
	}

	return line[start+1 : end]
}
