package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"time"
)

// net/smtp's SendMail dials with no timeout at all, so one unreachable or
// blackholed mail host wedges whatever called it: the notification loop stops
// delivering alerts for good, a monitor check never finishes, a subscribe
// request hangs until the client gives up.
// Variables, not constants, so tests can shorten them.
var (
	smtpDialTimeout  = 15 * time.Second
	smtpTotalTimeout = 60 * time.Second
)

// implicitTLSPort is the SMTPS port, where TLS starts before the greeting
// instead of being negotiated with STARTTLS.
const implicitTLSPort = "465"

// sendMail delivers one message, with timeouts, over STARTTLS (or implicit TLS
// on port 465). It follows the same sequence as net/smtp.SendMail otherwise.
func sendMail(addr string, auth smtp.Auth, from string, to []string, msg []byte) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("sendMail.SplitHostPort: %w", err)
	}

	conn, err := net.DialTimeout("tcp", addr, smtpDialTimeout)
	if err != nil {
		return fmt.Errorf("sendMail.Dial: %w", err)
	}
	defer conn.Close()

	// One deadline for the whole conversation: a server that accepts the
	// connection and then stalls is as bad as one that never answers.
	if err := conn.SetDeadline(time.Now().Add(smtpTotalTimeout)); err != nil {
		return fmt.Errorf("sendMail.SetDeadline: %w", err)
	}

	if port == implicitTLSPort {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("sendMail.Handshake: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("sendMail.NewClient: %w", err)
	}
	defer client.Close()

	if port != implicitTLSPort {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return fmt.Errorf("sendMail.StartTLS: %w", err)
			}
		}
	}

	if auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return errors.New("sendMail: server does not support AUTH")
		}
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("sendMail.Auth: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("sendMail.Mail: %w", err)
	}

	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("sendMail.Rcpt: %w", err)
		}
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("sendMail.Data: %w", err)
	}

	if _, err := writer.Write(msg); err != nil {
		return fmt.Errorf("sendMail.Write: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("sendMail.CloseData: %w", err)
	}

	return client.Quit()
}
