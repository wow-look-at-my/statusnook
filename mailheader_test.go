package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A monitor name or alert title carrying CRLF used to become extra headers in
// every recipient's copy -- smtp.SendMail checks the envelope, never the body.
func TestHeaderValueStripsCRLF(t *testing.T) {
	injected := "Outage\r\nBcc: attacker@example.com"

	got := headerValue(injected)
	require.False(t, strings.ContainsAny(got, "\r\n"))

	assert.True(t, strings.HasPrefix(got, "Outage"))

	got = headerValue("Plain title")
	assert.Equal(t, "Plain title", got)

}
