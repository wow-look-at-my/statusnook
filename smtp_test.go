package main

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSMTP is the smallest server that can accept one message.
type fakeSMTP struct {
	mu       sync.Mutex
	conv     []string
	body     string
	listener net.Listener
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &fakeSMTP{listener: listener}
	t.Cleanup(func() { listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		write := func(line string) {
			conn.Write([]byte(line + "\r\n"))
		}

		write("220 fake ESMTP")

		inData := false
		var body strings.Builder

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if inData {
				if line == "." {
					inData = false
					server.mu.Lock()
					server.body = body.String()
					server.mu.Unlock()
					write("250 queued")
					continue
				}
				body.WriteString(line + "\n")
				continue
			}

			server.mu.Lock()
			server.conv = append(server.conv, line)
			server.mu.Unlock()

			switch {
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				write("250-fake")
				write("250 SIZE 1000000")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				write("250 ok")
			case line == "DATA":
				inData = true
				write("354 send it")
			case line == "QUIT":
				write("221 bye")
				return
			default:
				write("500 unknown")
			}
		}
	}()

	return server
}

func (f *fakeSMTP) addr() string {
	return f.listener.Addr().String()
}

func TestSendMailDelivers(t *testing.T) {
	server := startFakeSMTP(t)

	err := sendMail(
		server.addr(), nil, "status@example.com",
		[]string{"admin@example.com"},
		[]byte("Subject: Monitor down\r\n\r\nThe monitor is down.\r\n"),
	)
	require.NoError(t, err)

	server.mu.Lock()
	defer server.mu.Unlock()

	conversation := strings.Join(server.conv, "\n")
	assert.Contains(t, conversation, "MAIL FROM:<status@example.com>")
	assert.Contains(t, conversation, "RCPT TO:<admin@example.com>")
	assert.Contains(t, conversation, "DATA")
	assert.Contains(t, server.body, "Subject: Monitor down")
	assert.Contains(t, server.body, "The monitor is down.")
}

// TestSendMailTimesOut is the reason this function exists: net/smtp.SendMail
// dials without a timeout, so a host that accepts and then says nothing used to
// block the notification loop forever.
func TestSendMailTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Never send a greeting.
		<-time.After(5 * time.Second)
		conn.Close()
	}()

	prevDial, prevTotal := smtpDialTimeout, smtpTotalTimeout
	t.Cleanup(func() { smtpDialTimeout, smtpTotalTimeout = prevDial, prevTotal })
	smtpDialTimeout, smtpTotalTimeout = time.Second, 500*time.Millisecond

	start := time.Now()
	err = sendMail(
		listener.Addr().String(), nil, "a@example.com", []string{"b@example.com"},
		[]byte("hi"),
	)

	assert.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second, "sendMail must not hang")
	<-accepted
}

func TestSendMailRejectsBadAddress(t *testing.T) {
	err := sendMail("no-port", nil, "a@example.com", []string{"b@example.com"}, []byte("hi"))
	assert.Error(t, err)
}
