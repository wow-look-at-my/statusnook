package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeConfigFile(t *testing.T, config string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))

	return path
}

// -validate-config is the only feedback a GitHub-pushed config gets before it
// reaches the instance, so it has to agree with what applyConfig will say.
func TestValidateConfigAcceptsAGoodFile(t *testing.T) {
	require.NoError(t, validateConfig(writeConfigFile(t, fullConfig)))
}

func TestValidateConfigReportsWhatApplyConfigWould(t *testing.T) {
	err := validateConfig(writeConfigFile(t, `
general-settings:
  name: Test Status
monitors:
  api:
    name: API
    url: not a url
    method: GET
    frequency: 60
    timeout: 5
    attempts: 9
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "url is invalid")
	require.Contains(t, err.Error(), "attempts must be one of 1, 2, 3")
}

func TestValidateConfigReportsAParseFailure(t *testing.T) {
	err := validateConfig(writeConfigFile(t, "services: [oh: no"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "yaml")

	// An unknown field is a typo, and silently ignoring it would delete
	// whatever the misspelled key was meant to configure.
	err = validateConfig(writeConfigFile(t, `
general-settings:
  name: Test Status
services:
  web:
    name: Web
    helper-text: not a field
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "helper-text")
}

func TestValidateConfigReportsAMissingFile(t *testing.T) {
	require.Error(t, validateConfig(filepath.Join(t.TempDir(), "nope.yaml")))
}

// A rename's source lives in the instance, not the file, so the validator
// seeds one -- otherwise every rename would report as broken offline.
func TestValidateConfigAcceptsARenameItCannotSee(t *testing.T) {
	require.NoError(t, validateConfig(writeConfigFile(t, `
general-settings:
  name: Test Status
rename:
  services.old-web: web
  monitors.old-api: api
  mail-groups.old-core: core
  notification-channels.old-chat: chat
mail-groups:
  core:
    name: Core
    members:
      - core@example.com
notification-channels:
  chat:
    type: slack
    name: Chat
    webhook-url: https://hooks.example.com/x
monitors:
  api:
    name: API
    url: https://example.com/health
    method: GET
    frequency: 60
    timeout: 5
    attempts: 2
services:
  web:
    name: Web
`)))
}
