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
