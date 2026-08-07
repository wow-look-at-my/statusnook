package main

import (
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// With TLS off nothing has to be proved about the domain: it is only the
// address the instance prints into emails and cross-auth redirects, so it is
// stored as given.
func TestSettingsStoreTheDomainWhenTLSIsOff(t *testing.T) {
	app := withTestApp(t)
	require.Equal(t, "false", metaSSL.Load())

	resp := app.post("/admin/settings", url.Values{"domain": {"status.example.com"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "status.example.com", metaDomain.Load())

	// Uppercase is folded, so the stored value matches what a certificate and
	// a Host header would carry.
	resp = app.post("/admin/settings", url.Values{"domain": {"STATUS.EXAMPLE.COM"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Equal(t, "status.example.com", metaDomain.Load())

	// An empty form changes nothing rather than clearing both fields.
	before := metaDomain.Load()
	require.Less(t, app.post("/admin/settings", url.Values{}).status, 400)
	require.Equal(t, before, metaDomain.Load())

	metaConfigFileEnabled.Store(true)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/settings", url.Values{"name": {"Renamed"}}).status)
}

// Cancelling records a problem against the pending domain rather than dropping
// it: that is what stops monitorUnconfirmedDomainLoop being restarted for it
// and what the settings page renders as the reason nothing is happening.
func TestCancelPendingDomain(t *testing.T) {
	app := withTestApp(t)

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, updateMetaValue(tx, "unconfirmedDomain", "status.example.com"))
	require.NoError(t, tx.Commit())

	metaUnconfirmedDomain.Store("status.example.com")
	metaUnconfirmedDomainProblem.Store("")

	resp := app.post("/admin/settings/cancel-domain", nil)
	require.Less(t, resp.status, 400, resp.body)

	require.Contains(t, metaUnconfirmedDomainProblem.Load(), "cancelled")
	require.Contains(t, app.metaValue("unconfirmedDomainProblem"), "cancelled")
	require.Equal(t, "status.example.com", metaUnconfirmedDomain.Load())

	body := app.get("/admin/settings").body
	require.Contains(t, body, "cancelled")

	t.Cleanup(func() {
		metaUnconfirmedDomain.Store("")
		metaUnconfirmedDomainProblem.Store("")
	})
}

// The -generate-self-signed-cert flag writes the pair the TLS listener falls
// back to when ACME has not issued anything yet.
func TestGenerateSelfSignedCertificateWritesAUsablePair(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { require.NoError(t, os.Chdir(wd)) })

	GenerateSelfSignedCertificate()

	cert, err := os.ReadFile(SELF_SIGNED_CERT_NAME)
	require.NoError(t, err)
	require.Contains(t, string(cert), "BEGIN CERTIFICATE")

	key, err := os.ReadFile(SELF_SIGNED_KEY_NAME)
	require.NoError(t, err)
	require.Contains(t, string(key), "PRIVATE KEY")

	info, err := os.Stat(SELF_SIGNED_KEY_NAME)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm()&0077, "the private key must not be world or group readable")
}

// With TLS on the domain has to be one a certificate could be issued for, so
// the form rejects the three shapes people actually type before it goes
// anywhere near DNS.
func TestSettingsValidatesTheDomainWhenTLSIsOn(t *testing.T) {
	app := withTestApp(t)

	metaSSL.Store("true")
	t.Cleanup(func() { metaSSL.Store("false") })
	metaDomain.Store("")
	t.Cleanup(func() { metaDomain.Store("") })

	for _, bad := range []string{
		"https://status.example.com", "192.0.2.1", "2001:db8::1", "not_a_domain",
	} {
		require.Equal(t, http.StatusBadRequest,
			app.post("/admin/settings", url.Values{"domain": {bad}}).status,
			"domain %q was accepted", bad)
	}

	// The pending domain is recorded before any of that, so the settings page
	// has something to show while the check is outstanding.
	require.Equal(t, "not_a_domain", metaUnconfirmedDomain.Load())
	require.Equal(t, "not_a_domain", app.metaValue("unconfirmedDomain"))

	// Once a domain is live the pending one is left alone -- the instance is
	// already reachable at the old name until the new one is proved.
	metaDomain.Store("status.example.com")
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/settings", url.Values{"domain": {"still_wrong"}}).status)
	require.Equal(t, "not_a_domain", metaUnconfirmedDomain.Load())
}

// The pending-domain write is the one database write on the TLS path that
// happens before anything is proved, so its failures have to surface.
func TestSettingsDomainSurfacesAFailureAtEveryStatement(t *testing.T) {
	app := withTestApp(t)

	metaSSL.Store("true")
	t.Cleanup(func() { metaSSL.Store("false") })
	metaDomain.Store("")
	t.Cleanup(func() { metaDomain.Store("") })

	withFaultyDB(app)

	for depth := int64(1); depth <= 20; depth++ {
		failAtStatement(depth)
		resp := app.post("/admin/settings", url.Values{"domain": {"not_a_domain"}})
		fired := faultFired()
		failAtStatement(0)

		if !fired {
			break
		}

		require.GreaterOrEqual(t, resp.status, 400,
			"settings answered %d with statement %d failed", resp.status, depth)
	}

	failAtStatement(0)
	require.Equal(t, http.StatusOK, app.get("/admin/settings").status)
}
