package main

import (
	"strings"
	"testing"
)

// A monitor name or alert title carrying CRLF used to become extra headers in
// every recipient's copy -- smtp.SendMail checks the envelope, never the body.
func TestHeaderValueStripsCRLF(t *testing.T) {
	injected := "Outage\r\nBcc: attacker@example.com"

	got := headerValue(injected)
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("headerValue(%q) = %q, still contains CR or LF", injected, got)
	}
	if !strings.HasPrefix(got, "Outage") {
		t.Errorf("headerValue(%q) = %q, lost the original text", injected, got)
	}

	if got := headerValue("Plain title"); got != "Plain title" {
		t.Errorf("headerValue rewrote a clean value: %q", got)
	}
}
