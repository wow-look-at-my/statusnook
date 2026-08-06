package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// applyConfig writes as it validates, deletes included, and never
// short-circuits when it has messages to report. Any caller that commits
// anyway applies a partially-broken config -- which is what configWebhook did
// until it started rolling back. This test pins the property the callers rely
// on: a config with an invalid monitor still mutates the transaction, so the
// transaction must be discarded rather than committed.
func TestApplyConfigMutatesEvenWhenItReportsErrors(t *testing.T) {
	withTestDB(t)

	// A service that a later config will not mention, so applyConfig deletes
	// it while also reporting an unrelated error.
	_, err := rwDB.Exec(
		`insert into service(slug, name, helper_text) values('doomed', 'Doomed', '')`,
	)
	require.NoError(t, err)

	// The secret key applyConfig reads before doing anything else.
	tx, err := rwDB.Begin()
	require.NoError(t, err)
	require.NoError(t, updateMetaValue(tx, "secretKey",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="))
	require.NoError(t, tx.Commit())

	tx, err = rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	// 'doomed' is absent, and the monitor's frequency is not one of the
	// allowed values.
	msgs, err := applyConfig(tx, []byte(`
general-settings:
  name: Test
monitors:
  broken:
    name: Broken
    url: https://example.com
    method: GET
    frequency: 15
    timeout: 5
    attempts: 1
`))
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "an invalid frequency should be reported")

	// The delete already happened inside the transaction, alongside the error.
	var remaining int
	require.NoError(t,
		tx.QueryRow("select count(*) from service where slug = 'doomed'").Scan(&remaining))
	require.Equal(t, 0, remaining,
		"applyConfig no longer deletes before validating -- if this is now 1, "+
			"the callers' rollback-on-messages requirement may have changed")

	// And rolling back is what undoes it.
	require.NoError(t, tx.Rollback())

	require.NoError(t,
		rwDB.QueryRow("select count(*) from service where slug = 'doomed'").Scan(&remaining))
	require.Equal(t, 1, remaining, "rollback did not restore the deleted service")
}
