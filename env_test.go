package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRepo(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "owner/name", want: "owner/name"},
		{in: "  owner/name  ", want: "owner/name"},
		{in: "owner/name/", want: "owner/name"},
		{in: "https://github.com/owner/name", want: "owner/name"},
		{in: "https://github.com/owner/name/", want: "owner/name"},
		{in: "https://github.com/owner/name.git", want: "owner/name"},
		{in: "git@github.com:owner/name", wantErr: true},
		{in: "owner", wantErr: true},
		{in: "owner/name/extra", wantErr: true},
		{in: "https://github.com/owner/name/tree/main", wantErr: true},
		{in: "owner/na me", wantErr: true},
	}

	for _, c := range cases {
		got, err := normalizeRepo(c.in)
		if c.wantErr {
			assert.Error(t, err, "input %q", c.in)
			continue
		}
		require.NoError(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
	}
}

func TestEnvSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("  github_pat_secret\n"), 0o600))

	t.Setenv("STATUSNOOK_GITHUB_TOKEN_FILE", path)

	got, err := envSecret("GITHUB_TOKEN")
	require.NoError(t, err)
	assert.Equal(t, "github_pat_secret", got)

	// Both forms at once is a configuration mistake, not a precedence puzzle.
	t.Setenv("STATUSNOOK_GITHUB_TOKEN", "inline")
	_, err = envSecret("GITHUB_TOKEN")
	assert.Error(t, err)
}

func TestEnvSecretMissingFile(t *testing.T) {
	t.Setenv("STATUSNOOK_GITHUB_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))

	_, err := envSecret("GITHUB_TOKEN")
	assert.Error(t, err)
}

func TestEnvDuration(t *testing.T) {
	cases := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{value: "", want: time.Minute},
		{value: "90", want: 90 * time.Second},
		{value: "5m", want: 5 * time.Minute},
		{value: "0", want: 0},
		{value: "-1", wantErr: true},
		{value: "-5m", wantErr: true},
		{value: "soon", wantErr: true},
	}

	for _, c := range cases {
		t.Setenv("STATUSNOOK_GITHUB_POLL_INTERVAL", c.value)

		got, err := envDuration("GITHUB_POLL_INTERVAL", time.Minute)
		if c.wantErr {
			assert.Error(t, err, "value %q", c.value)
			continue
		}
		require.NoError(t, err, "value %q", c.value)
		assert.Equal(t, c.want, got, "value %q", c.value)
	}
}

func TestLoadEnvRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
	}{
		{
			name: "repo without token",
			vars: map[string]string{"STATUSNOOK_GITHUB_REPO": "owner/name"},
		},
		{
			name: "token without repo",
			vars: map[string]string{"STATUSNOOK_GITHUB_TOKEN": "t"},
		},
		{
			name: "username without password",
			vars: map[string]string{"STATUSNOOK_ADMIN_USERNAME": "admin"},
		},
		{
			name: "short password",
			vars: map[string]string{
				"STATUSNOOK_ADMIN_USERNAME": "admin",
				"STATUSNOOK_ADMIN_PASSWORD": "short",
			},
		},
		{
			name: "tls auto without domain",
			vars: map[string]string{"STATUSNOOK_TLS": "auto"},
		},
		{
			name: "unknown tls mode",
			vars: map[string]string{"STATUSNOOK_TLS": "maybe"},
		},
		{
			name: "invalid domain",
			vars: map[string]string{"STATUSNOOK_DOMAIN": "not a host"},
		},
		{
			name: "poll interval too small",
			vars: map[string]string{"STATUSNOOK_GITHUB_POLL_INTERVAL": "5s"},
		},
		{
			name: "non boolean trust proxy",
			vars: map[string]string{"STATUSNOOK_TRUST_PROXY": "sure"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.vars {
				t.Setenv(k, v)
			}

			_, err := loadEnv()
			assert.Error(t, err)
		})
	}
}

func TestLoadEnvManagedGitHub(t *testing.T) {
	t.Setenv("STATUSNOOK_GITHUB_REPO", "https://github.com/acme/status-config")
	t.Setenv("STATUSNOOK_GITHUB_TOKEN", "github_pat_x")
	t.Setenv("STATUSNOOK_GITHUB_CONFIG_PATH", "conf/statusnook.yaml")
	t.Setenv("STATUSNOOK_GITHUB_POLL_INTERVAL", "30s")
	t.Setenv("STATUSNOOK_ADMIN_USERNAME", "admin")
	t.Setenv("STATUSNOOK_ADMIN_PASSWORD", "long-enough")
	t.Setenv("STATUSNOOK_DOMAIN", "Status.Example.com")
	t.Setenv("STATUSNOOK_TLS", "auto")
	t.Setenv("STATUSNOOK_TRUST_PROXY", "true")

	cfg, err := loadEnv()
	require.NoError(t, err)

	assert.True(t, cfg.GitHub.Managed())
	assert.Equal(t, "acme/status-config", cfg.GitHub.Repo)
	assert.Equal(t, "conf/statusnook.yaml", cfg.GitHub.Path)
	assert.Equal(t, 30*time.Second, cfg.GitHub.PollInterval)
	assert.Equal(t, "status.example.com", cfg.Domain)
	assert.Equal(t, "auto", cfg.TLS)
	assert.True(t, cfg.TrustProxy)
	assert.Equal(t, "https://api.github.com", cfg.GitHub.APIURL)
}

func TestLoadEnvDefaults(t *testing.T) {
	cfg, err := loadEnv()
	require.NoError(t, err)

	assert.False(t, cfg.GitHub.Managed())
	assert.Equal(t, "statusnook-data", cfg.DataDir)
	assert.Equal(t, "statusnook.yaml", cfg.GitHub.Path)
	assert.Equal(t, time.Minute, cfg.GitHub.PollInterval)
	assert.False(t, cfg.TrustProxy)
}
