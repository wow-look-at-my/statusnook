package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidWebhookSignature(t *testing.T) {
	payload := []byte(`{"ref": "refs/heads/main"}`)
	secret := "s3cret"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	valid := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	assert.True(t, validWebhookSignature(valid, secret, payload))
	assert.False(t, validWebhookSignature(valid, "other-secret", payload))
	assert.False(t, validWebhookSignature(valid, secret, []byte("tampered")))
	assert.False(t, validWebhookSignature("", secret, payload))
	assert.False(t, validWebhookSignature("sha1=abcdef", secret, payload))
	assert.False(t, validWebhookSignature("sha256=not-hex", secret, payload))
	assert.False(t, validWebhookSignature(strings.TrimPrefix(valid, "sha256="), secret, payload))
}

func TestPushTouchesBranch(t *testing.T) {
	push := func(ref string, defaultBranch string) []byte {
		body, err := json.Marshal(map[string]any{
			"ref":        ref,
			"repository": map[string]any{"default_branch": defaultBranch},
		})
		require.NoError(t, err)

		return body
	}

	assert.True(t, pushTouchesBranch(push("refs/heads/main", "main"), "main"))
	assert.False(t, pushTouchesBranch(push("refs/heads/feature", "main"), "main"))

	// An empty watched branch means the repository's default branch.
	assert.True(t, pushTouchesBranch(push("refs/heads/main", "main"), ""))
	assert.False(t, pushTouchesBranch(push("refs/heads/feature", "main"), ""))

	// Tag pushes carry refs/tags/... and must not match a branch.
	assert.False(t, pushTouchesBranch(push("refs/tags/v1", "main"), "main"))

	// Unparseable or ref-less payloads fall through to the sync, which is
	// SHA-guarded anyway.
	assert.True(t, pushTouchesBranch([]byte("not json"), "main"))
	assert.True(t, pushTouchesBranch(push("", "main"), "main"))
}

// useTestDBs points the package-level database handles at a temporary file
// database, so handlers can be exercised end to end.
func useTestDBs(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("STATUSNOOK_DATA_DIR", dir)

	prevEnv, prevDB, prevRWDB := env, db, rwDB
	prevName, prevConfigFile := metaName, metaConfigFileEnabled

	env = envConfig{DataDir: dir}

	dsn := "file:" + filepath.Join(dir, "app.db") +
		"?_foreign_keys=on&_journal_mode=wal&_busy_timeout=5000"

	var err error
	db, err = openTestDB(dsn)
	require.NoError(t, err)

	rwDB, err = openTestDB(dsn + "&_txlock=immediate")
	require.NoError(t, err)
	rwDB.SetMaxOpenConns(1)

	_, err = rwDB.Exec(sqlSchema)
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Close()
		rwDB.Close()
		env, db, rwDB = prevEnv, prevDB, prevRWDB
		metaName, metaConfigFileEnabled = prevName, prevConfigFile
	})
}

// setTestSecretKey installs the key applyConfig uses to decrypt secrets.
func setTestSecretKey(t *testing.T) {
	t.Helper()

	tx, err := rwDB.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	key := make([]byte, 32)
	require.NoError(t, updateMetaValue(
		tx, "secretKey", base64.StdEncoding.EncodeToString(key),
	))
	require.NoError(t, tx.Commit())
}

const testConfig = `general-settings:
  name: Synced Name
services:
  website:
    name: Website
    description: example.com
monitors:
  homepage:
    name: Homepage
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
`

// TestSyncGitHubConfigAppliesRevision covers the whole private-repository path:
// fetch, apply, remember the blob SHA, and skip work when nothing changed.
func TestSyncGitHubConfigAppliesRevision(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	fake := &fakeGitHub{t: t, content: testConfig, sha: "sha-one"}
	server := fake.start()
	src := fake.source(server)

	changed, err := syncGitHubConfig(context.Background(), src)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "Synced Name", metaName)

	monitors := listMonitorSlugs(t)
	assert.Equal(t, []string{"homepage"}, monitors)

	// Same SHA: nothing to do.
	changed, err = syncGitHubConfig(context.Background(), src)
	require.NoError(t, err)
	assert.False(t, changed)

	// New revision: applied, and the removed monitor goes away.
	fake.content = strings.Replace(testConfig, "Synced Name", "Renamed", 1)
	fake.content = strings.Replace(fake.content, "homepage:", "docs:", 1)
	fake.sha = "sha-two"

	changed, err = syncGitHubConfig(context.Background(), src)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "Renamed", metaName)
	assert.Equal(t, []string{"docs"}, listMonitorSlugs(t))
}

// TestSyncGitHubConfigRecordsConfigErrors checks that a broken revision leaves
// the previous configuration running and records the problem.
func TestSyncGitHubConfigRecordsConfigErrors(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	fake := &fakeGitHub{t: t, content: testConfig, sha: "sha-one"}
	server := fake.start()
	src := fake.source(server)

	_, err := syncGitHubConfig(context.Background(), src)
	require.NoError(t, err)

	fake.content = "general-settings:\n  name: [unclosed\n"
	fake.sha = "sha-broken"

	_, err = syncGitHubConfig(context.Background(), src)
	require.NoError(t, err)

	assert.Equal(t, "Synced Name", metaName, "a broken revision must not clear the name")
	assert.NotEmpty(t, readMeta(t, "githubConfigErrors"))
	assert.Equal(t, []string{"homepage"}, listMonitorSlugs(t))
}

func TestConfigWebhookRejectsUnsigned(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	fake := &fakeGitHub{t: t, content: testConfig, sha: "sha-one"}
	server := fake.start()

	env.GitHub = gitHubEnvConfig{
		Repo:          "acme/config",
		Branch:        "main",
		Path:          "conf/statusnook.yaml",
		Token:         "github_pat_secret",
		APIURL:        server.URL,
		WebhookSecret: "hook-secret",
	}

	body := []byte(`{"ref": "refs/heads/main"}`)

	t.Run("missing signature", func(t *testing.T) {
		rec := httptest.NewRecorder()
		configWebhook(rec, httptest.NewRequest(http.MethodPost, "/github-config-webhook",
			strings.NewReader(string(body))))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong signature", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/github-config-webhook",
			strings.NewReader(string(body)))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString([]byte("nope")))
		configWebhook(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("no config applied", func(t *testing.T) {
		assert.Empty(t, readMeta(t, "githubConfigSHA"))
	})
}

func TestConfigWebhookAppliesSignedPush(t *testing.T) {
	useTestDBs(t)
	setTestSecretKey(t)

	fake := &fakeGitHub{t: t, content: testConfig, sha: "sha-one"}
	server := fake.start()

	env.GitHub = gitHubEnvConfig{
		Repo:          "acme/config",
		Branch:        "main",
		Path:          "conf/statusnook.yaml",
		Token:         "github_pat_secret",
		APIURL:        server.URL,
		WebhookSecret: "hook-secret",
	}

	post := func(body string, event string) *httptest.ResponseRecorder {
		mac := hmac.New(sha256.New, []byte("hook-secret"))
		mac.Write([]byte(body))

		req := httptest.NewRequest(http.MethodPost, "/github-config-webhook",
			strings.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-GitHub-Event", event)

		rec := httptest.NewRecorder()
		configWebhook(rec, req)

		return rec
	}

	rec := post(`{"ref": "refs/heads/main"}`, "push")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "sha-one", readMeta(t, "githubConfigSHA"))
	assert.Equal(t, "Synced Name", metaName)

	// A push to another branch must not apply that branch's config.
	fake.content = strings.Replace(testConfig, "Synced Name", "Other Branch", 1)
	fake.sha = "sha-other"

	rec = post(`{"ref": "refs/heads/feature"}`, "push")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "sha-one", readMeta(t, "githubConfigSHA"))
	assert.Equal(t, "Synced Name", metaName)

	// A ping is acknowledged without touching anything.
	rec = post(`{"zen": "Keep it logically awesome."}`, "ping")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "sha-one", readMeta(t, "githubConfigSHA"))
}

func TestConfigWebhookDisabledWithoutSecret(t *testing.T) {
	useTestDBs(t)

	env.GitHub = gitHubEnvConfig{
		Repo:  "acme/config",
		Path:  "conf/statusnook.yaml",
		Token: "github_pat_secret",
	}

	rec := httptest.NewRecorder()
	configWebhook(rec, httptest.NewRequest(http.MethodPost, "/github-config-webhook",
		strings.NewReader(`{"ref": "refs/heads/main"}`)))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestConfigWebhookRejectsOversizedBody(t *testing.T) {
	useTestDBs(t)

	env.GitHub = gitHubEnvConfig{
		Repo:          "acme/config",
		Path:          "conf/statusnook.yaml",
		Token:         "github_pat_secret",
		WebhookSecret: "hook-secret",
	}

	rec := httptest.NewRecorder()
	configWebhook(rec, httptest.NewRequest(http.MethodPost, "/github-config-webhook",
		strings.NewReader(strings.Repeat("a", maxWebhookPayload+10))))

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func listMonitorSlugs(t *testing.T) []string {
	t.Helper()

	rows, err := db.Query("select slug from monitor order by slug")
	require.NoError(t, err)
	defer rows.Close()

	slugs := []string{}
	for rows.Next() {
		var slug string
		require.NoError(t, rows.Scan(&slug))
		slugs = append(slugs, slug)
	}

	return slugs
}

func readMeta(t *testing.T, name string) string {
	t.Helper()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer tx.Rollback()

	value, err := getMetaValue(tx, name)
	if err != nil {
		return ""
	}

	return value
}
