package main

import "testing"

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
		if got := safeAfterPath(after); got != "/" {
			t.Errorf("safeAfterPath(%q) = %q, want %q", after, got, "/")
		}
	}

	keeps := map[string]string{
		"":                       "/",
		"/":                      "/",
		"/admin/monitors":        "/admin/monitors",
		"/admin/monitors?tab=up": "/admin/monitors?tab=up",
	}
	for after, want := range keeps {
		if got := safeAfterPath(after); got != want {
			t.Errorf("safeAfterPath(%q) = %q, want %q", after, got, want)
		}
	}
}
