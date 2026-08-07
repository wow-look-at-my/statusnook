package main

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Turning the config file on snapshots the live instance into a config, which
// is the file the editor then hands the operator. Anything generateConfig drops
// here is deleted the first time that file is applied back.
func TestEnablingTheConfigFileSnapshotsTheWholeInstance(t *testing.T) {
	app := withTestApp(t)

	app.createService("Ingest", "the pipeline")
	app.createMonitor("API", "https://example.com/health")
	app.createSlackChannel("Chat", "https://hooks.example.com/x")
	smtpID := app.createSMTPChannel("Mail")
	app.createMailGroup("Ops", "ops@example.com")

	resp := app.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(smtpID)},
		"slack-install-url":         {"https://slack.example.com/install"},
		"managed-subscriptions":     {"on"},
	})
	require.Less(t, resp.status, 400, resp.body)

	resp = app.post("/admin/settings/config-settings", url.Values{"config-file": {"on"}})
	require.Less(t, resp.status, 400, resp.body)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	config := app.metaValue("configFile")
	require.NotEmpty(t, config)

	for _, want := range []string{
		"Ingest", "API", "Chat", "Mail", "Ops",
		"hooks.example.com", "ops@example.com",
		"smtp.example.com", "slack.example.com/install",
		"managed-subscriptions",
	} {
		require.Contains(t, config, want, "the snapshot dropped %q", want)
	}

	// The snapshot has to be a config the instance would accept back, or the
	// operator's first push deletes whatever it left out.
	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	msgs, err := applyConfig(tx, []byte(config))
	require.NoError(t, err)
	require.Empty(t, msgs)
}

// The settings form checks the repository and the config path exist before it
// saves, so a typo is caught at the form rather than at the first push.
func TestConfigSettingsChecksTheRepositoryAndPath(t *testing.T) {
	app := withTestApp(t)
	gh := newFakeGitHub(t)

	valid := func() url.Values {
		return url.Values{
			"config-file":           {"on"},
			"github-managed":        {"on"},
			"github-repo-url":       {"https://github.com/example/status"},
			"github-branch":         {"master"},
			"github-config-path":    {"config.yaml"},
			"github-token":          {"ghtoken"},
			"github-webhook-secret": {"hooksecret"},
		}
	}

	// github-managed without config-file is a contradiction: the file is what
	// GitHub would be managing.
	noFile := valid()
	noFile.Del("config-file")
	require.Equal(t, http.StatusBadRequest,
		app.post("/admin/settings/config-settings", noFile).status)

	// Each GitHub field is required once sync is on.
	for _, field := range []string{
		"github-repo-url", "github-branch", "github-config-path",
		"github-token", "github-webhook-secret",
	} {
		form := valid()
		form.Del(field)

		require.Equal(t, http.StatusBadRequest,
			app.post("/admin/settings/config-settings", form).status,
			"a missing %s was accepted", field)
	}

	bare := valid()
	bare.Set("github-repo-url", "https://github.com")
	resp := app.post("/admin/settings/config-settings", bare)
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "Invalid GitHub repository URL")

	// The config path is gone from the repository.
	gh.serveMissing()
	resp = app.post("/admin/settings/config-settings", valid())
	require.Equal(t, http.StatusBadRequest, resp.status)
	require.Contains(t, resp.body, "configuration could not be found")

	gh.serve("general-settings:\n  name: Test Status\n", "sha-zero")
	resp = app.post("/admin/settings/config-settings", valid())
	require.Less(t, resp.status, 400, resp.body)
	t.Cleanup(func() { metaConfigFileEnabled.Store(false) })

	require.Equal(t, http.StatusOK, app.get("/admin/settings/config-settings").status)
	require.Equal(t, "true", app.metaValue("githubManagedConfig"))

	// Turning sync off clears the recorded sha, so re-enabling it re-applies
	// the file rather than skipping it as already-seen.
	off := valid()
	off.Del("github-managed")
	resp = app.post("/admin/settings/config-settings", off)
	require.Less(t, resp.status, 400, resp.body)
	require.Empty(t, app.metaValue("githubConfigSHA"))
	require.Equal(t, "false", app.metaValue("githubManagedConfig"))
}

// The form checks the repository and the config path by calling GitHub, so
// GitHub being unreachable has to stop the save rather than store settings
// nothing has verified.
func TestConfigSettingsFailsWhenGitHubIsUnreachable(t *testing.T) {
	app := withTestApp(t)

	previous := githubAPIBaseURL
	githubAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { githubAPIBaseURL = previous })

	resp := app.post("/admin/settings/config-settings", url.Values{
		"config-file":           {"on"},
		"github-managed":        {"on"},
		"github-repo-url":       {"https://github.com/example/status"},
		"github-branch":         {"master"},
		"github-config-path":    {"config.yaml"},
		"github-token":          {"ghtoken"},
		"github-webhook-secret": {"hooksecret"},
	})
	require.GreaterOrEqual(t, resp.status, 400, resp.body)
	require.Empty(t, app.metaValue("githubRepoURL"))
}
