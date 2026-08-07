package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A rename is the only way to change a resource's key without deleting it and
// its history, and it is the one part of a config that reads live state. Every
// way it can be wrong has its own message, because the operator sees nothing
// else.
func TestApplyConfigRenameFailureModes(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	joined := func(config string) string {
		return strings.Join(applyTestConfig(t, tx, config), "\n")
	}

	// A key with no entity type at all.
	require.Contains(t, joined(`
general-settings:
  name: Test Status
rename:
  website: site
services:
  site:
    name: Website
`), "invalid")

	// Renaming something to the name it already has.
	require.Contains(t, joined(`
general-settings:
  name: Test Status
rename:
  services.website: website
services:
  website:
    name: Website
`), "is invalid")

	// A target that is not a legal key.
	require.Contains(t, joined(`
general-settings:
  name: Test Status
rename:
  services.website: Not_A_Slug
services:
  Not_A_Slug:
    name: Website
`), "lower-case letters")

	// Renaming onto a key another resource already holds.
	require.Empty(t, applyTestConfig(t, tx, `
general-settings:
  name: Test Status
services:
  website:
    name: Website
  api:
    name: API
`))
	require.Contains(t, joined(`
general-settings:
  name: Test Status
rename:
  services.website: api
services:
  api:
    name: API
`), "would not be unique")

	// A rename whose target the file never goes on to declare: the resource
	// would be renamed and then deleted for not being named.
	require.Contains(t, joined(`
general-settings:
  name: Test Status
rename:
  services.api: gateway
services:
  api:
    name: API
`), "to perform a rename")

	// An entity type that does not exist.
	require.NotEmpty(t, applyTestConfig(t, tx, `
general-settings:
  name: Test Status
rename:
  widgets.one: two
`))
}
