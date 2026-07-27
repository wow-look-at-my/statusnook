package main

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metaValue reads a meta row straight out of the database, to check what a
// handler persisted rather than what it left in the in-memory mirror.
func metaValue(t *testing.T, key string) string {
	t.Helper()

	value := ""
	require.NoError(t, db.QueryRow("select value from meta where key = ?", key).Scan(&value))

	return value
}

func TestSettingsName(t *testing.T) {
	ts := newTestServer(t)

	renamed := ts.post("/admin/settings", url.Values{"name": {"Renamed Nook"}})
	require.Equal(t, http.StatusOK, renamed.code, renamed.body)
	assert.Contains(t, renamed.body, "Renamed Nook")
	assert.Equal(t, "Renamed Nook", metaName)
	assert.Equal(t, "Renamed Nook", metaValue(t, "name"))

	// An empty form changes nothing rather than blanking the name.
	assert.Equal(t, http.StatusOK, ts.post("/admin/settings", nil).code)
	assert.Equal(t, "Renamed Nook", metaName)
}

// TestSettingsRefusesEnvOwnedValues covers the read-only settings: whatever the
// environment sets is re-applied on every start, so a change here would revert.
func TestSettingsRefusesEnvOwnedValues(t *testing.T) {
	ts := newTestServer(t)

	env.Name = "From The Environment"
	env.Domain = "env.example.com"
	t.Cleanup(func() { env.Name, env.Domain = "", "" })

	refusedName := ts.post("/admin/settings", url.Values{"name": {"Manual"}})
	assert.Equal(t, http.StatusBadRequest, refusedName.code)
	assert.Contains(t, refusedName.body, "STATUSNOOK_NAME")
	assert.Equal(t, "Test Nook", metaName)

	refusedDomain := ts.post("/admin/settings", url.Values{"domain": {"manual.example.com"}})
	assert.Equal(t, http.StatusBadRequest, refusedDomain.code)
	assert.Contains(t, refusedDomain.body, "STATUSNOOK_DOMAIN")
}

// TestSettingsRefusesNameWhenConfigManaged covers the config file owning the
// name instead of the UI.
func TestSettingsRefusesNameWhenConfigManaged(t *testing.T) {
	ts := newTestServer(t)

	prev := metaConfigFileEnabled
	metaConfigFileEnabled = true
	t.Cleanup(func() { metaConfigFileEnabled = prev })

	assert.Equal(t, http.StatusBadRequest,
		ts.post("/admin/settings", url.Values{"name": {"Manual"}}).code)
	assert.Equal(t, "Test Nook", metaName)
}

// TestSettingsDomainWithoutManagedTLS covers the path a reverse-proxied
// instance takes: the domain is only used for links, so it is stored directly
// with no certificate work.
func TestSettingsDomainWithoutManagedTLS(t *testing.T) {
	ts := newTestServer(t)

	prev := metaDomain
	t.Cleanup(func() { metaDomain = prev })

	saved := ts.post("/admin/settings", url.Values{"domain": {"Status.Example.COM"}})
	require.Equal(t, http.StatusOK, saved.code, saved.body)
	assert.Equal(t, "/admin/settings", saved.hdr.Get("HX-Location"))
	assert.Equal(t, "status.example.com", metaDomain, "the domain is lower-cased")
	assert.Equal(t, "status.example.com", metaValue(t, "domain"))
}

// TestSettingsDomainValidation covers the managed-TLS path up to the point it
// would talk to DNS: a rejected domain must not be stored.
func TestSettingsDomainValidation(t *testing.T) {
	ts := newTestServer(t)

	prevSSL, prevDomain, prevUnconfirmed := metaSSL, metaDomain, metaUnconfirmedDomain
	metaSSL, metaDomain, metaUnconfirmedDomain = "true", "", ""
	t.Cleanup(func() {
		metaSSL, metaDomain, metaUnconfirmedDomain = prevSSL, prevDomain, prevUnconfirmed
	})

	for _, tc := range []struct{ domain, message string }{
		{"https://status.example.com", "URL"},
		{"192.0.2.1", "IP address"},
		{"2001:db8::1", "IP address"},
		{"localhost", "Invalid domain"},
		{"status.example.c", "Invalid domain"},
	} {
		resp := ts.post("/admin/settings", url.Values{"domain": {tc.domain}})
		assert.Equal(t, http.StatusBadRequest, resp.code, tc.domain)
		assert.Contains(t, resp.body, tc.message, tc.domain)
	}

	assert.Empty(t, metaUnconfirmedDomain, "a rejected domain must not be stored")
	assert.Empty(t, metaValue(t, "unconfirmedDomain"))
}

// TestSettingsCancelDomain covers abandoning the verification of a domain that
// never resolved.
func TestSettingsCancelDomain(t *testing.T) {
	ts := newTestServer(t)

	prev := metaUnconfirmedDomainProblem
	t.Cleanup(func() { metaUnconfirmedDomainProblem = prev })

	cancelled := ts.post("/admin/settings/cancel-domain", nil)
	require.Equal(t, http.StatusOK, cancelled.code, cancelled.body)
	assert.Equal(t, "/admin/settings", cancelled.hdr.Get("HX-Location"))
	assert.Contains(t, metaUnconfirmedDomainProblem, "cancelled")
	assert.Equal(t, metaUnconfirmedDomainProblem, metaValue(t, "unconfirmedDomainProblem"))
}

// TestSettingsPageShowsEnvOwnership covers the settings page rendering with the
// pieces the environment owns marked read-only.
func TestSettingsPageShowsEnvOwnership(t *testing.T) {
	ts := newTestServer(t)

	env.Name = "From The Environment"
	env.Domain = "env.example.com"
	env.GitHub = gitHubEnvConfig{Repo: "acme/config", Path: "statusnook.yaml"}
	t.Cleanup(func() {
		env.Name, env.Domain, env.GitHub = "", "", gitHubEnvConfig{}
	})

	page := ts.get("/admin/settings")
	require.Equal(t, http.StatusOK, page.code)

	config := ts.get("/admin/settings/config-settings")
	require.Equal(t, http.StatusOK, config.code)
	assert.Contains(t, config.body, "acme/config")
	assert.Contains(t, config.body, "STATUSNOOK_GITHUB_REPO")
}
