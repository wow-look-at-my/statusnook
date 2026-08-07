package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSafeAfterPath(t *testing.T) {
	// Everything in the first group left the origin before safeAfterPath
	// existed: the value is concatenated onto "https://" + metaDomain, and an
	// "@" ends the authority's userinfo.
	escapes := []string{
		"@evil.example.com/phish",
		"//evil.example.com/phish",
		"https://evil.example.com",
		"http://evil.example.com",
		"@evil.example.com",
		"///evil.example.com",
	}
	for _, after := range escapes {
		got := safeAfterPath(after)
		assert.Equal(t, "/", got)

	}

	keeps := map[string]string{
		"":                       "/",
		"/":                      "/",
		"/admin/monitors":        "/admin/monitors",
		"/admin/monitors?tab=up": "/admin/monitors?tab=up",
	}
	for after, want := range keeps {
		got := safeAfterPath(after)
		assert.Equal(t, want, got)

	}
}
