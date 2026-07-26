package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEscapeContentsPath(t *testing.T) {
	cases := map[string]string{
		"statusnook.yaml":            "statusnook.yaml",
		"/statusnook.yaml":           "statusnook.yaml",
		"conf/statusnook.yaml":       "conf/statusnook.yaml",
		"my dir/statusnook.yaml":     "my%20dir/statusnook.yaml",
		"weird#name/config.yaml":     "weird%23name/config.yaml",
		"a/b/c/statusnook.yaml":      "a/b/c/statusnook.yaml",
		"question?/statusnook.yaml":  "question%3F/statusnook.yaml",
		"percent%/statusnook.yaml":   "percent%25/statusnook.yaml",
		"trailing/slash/config.yaml": "trailing/slash/config.yaml",
	}

	for in, want := range cases {
		assert.Equal(t, want, escapeContentsPath(in), "input %q", in)
	}
}

// fakeGitHub serves the contents endpoint for one file.
type fakeGitHub struct {
	t *testing.T

	content   string
	sha       string
	status    int
	body      string
	lastPath  string
	lastQuery string
	lastAuth  string
	lastAPIV  string
}

func (f *fakeGitHub) start() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.RawQuery
		f.lastAuth = r.Header.Get("Authorization")
		f.lastAPIV = r.Header.Get("X-GitHub-Api-Version")

		if f.status != 0 && f.status != http.StatusOK {
			w.WriteHeader(f.status)
			w.Write([]byte(f.body))
			return
		}

		// Repository lookups have no /contents/ segment.
		if !strings.Contains(r.URL.Path, "/contents/") {
			w.Write([]byte(`{"full_name": "acme/config"}`))
			return
		}

		encoded := base64.StdEncoding.EncodeToString([]byte(f.content))
		// GitHub wraps the payload; make sure we cope.
		wrapped := ""
		for len(encoded) > 60 {
			wrapped += encoded[:60] + "\n"
			encoded = encoded[60:]
		}
		wrapped += encoded

		json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"size":     len(f.content),
			"sha":      f.sha,
			"content":  wrapped,
		})
	}))
	f.t.Cleanup(server.Close)

	return server
}

func (f *fakeGitHub) source(server *httptest.Server) gitHubConfigSource {
	return gitHubConfigSource{
		Repo:   "acme/config",
		Branch: "main",
		Path:   "conf/statusnook.yaml",
		Token:  "github_pat_secret",
		APIURL: server.URL,
	}
}

func TestFetchGitHubConfig(t *testing.T) {
	fake := &fakeGitHub{t: t, content: "general-settings:\n  name: Example\n", sha: "abc123"}
	server := fake.start()
	src := fake.source(server)

	file, err := fetchGitHubConfig(context.Background(), src)
	require.NoError(t, err)

	assert.Equal(t, "general-settings:\n  name: Example\n", string(file.Content))
	assert.Equal(t, "abc123", file.SHA)
	assert.Equal(t, "/repos/acme/config/contents/conf/statusnook.yaml", fake.lastPath)
	assert.Equal(t, "ref=main", fake.lastQuery)
	assert.Equal(t, "Bearer github_pat_secret", fake.lastAuth)
	assert.Equal(t, gitHubAPIVersion, fake.lastAPIV)
}

func TestFetchGitHubConfigOmitsRefWhenBranchEmpty(t *testing.T) {
	fake := &fakeGitHub{t: t, content: "x", sha: "s"}
	server := fake.start()
	src := fake.source(server)
	src.Branch = ""

	_, err := fetchGitHubConfig(context.Background(), src)
	require.NoError(t, err)
	assert.Empty(t, fake.lastQuery)
}

func TestFetchGitHubConfigErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		fake := &fakeGitHub{
			t:      t,
			status: http.StatusNotFound,
			body:   `{"message": "Not Found"}`,
		}
		server := fake.start()

		_, err := fetchGitHubConfig(context.Background(), fake.source(server))
		require.Error(t, err)

		var apiErr *gitHubAPIError
		require.ErrorAs(t, err, &apiErr)
		assert.True(t, apiErr.notFound())

		// The token must never travel with an error message.
		assert.NotContains(t, err.Error(), "github_pat_secret")
		assert.NotContains(t, describeGitHubError(err), "github_pat_secret")
		assert.Contains(t, describeGitHubError(err), "STATUSNOOK_GITHUB_REPO")
	})

	t.Run("unauthorized", func(t *testing.T) {
		fake := &fakeGitHub{
			t:      t,
			status: http.StatusUnauthorized,
			body:   `{"message": "Bad credentials"}`,
		}
		server := fake.start()

		_, err := fetchGitHubConfig(context.Background(), fake.source(server))
		require.Error(t, err)

		var apiErr *gitHubAPIError
		require.ErrorAs(t, err, &apiErr)
		assert.True(t, apiErr.unauthorized())
		assert.Contains(t, describeGitHubError(err), "STATUSNOOK_GITHUB_TOKEN")
	})

	t.Run("incomplete source", func(t *testing.T) {
		_, err := fetchGitHubConfig(context.Background(), gitHubConfigSource{Repo: "acme/config"})
		assert.Error(t, err)
	})

	t.Run("directory instead of file", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"type": "dir"}`))
		}))
		defer server.Close()

		src := gitHubConfigSource{
			Repo: "acme/config", Path: "conf", Token: "t", APIURL: server.URL,
		}
		_, err := fetchGitHubConfig(context.Background(), src)
		assert.Error(t, err)
	})

	t.Run("too large", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "encoding": "base64", "size": maxConfigSize + 1, "sha": "s",
			})
		}))
		defer server.Close()

		src := gitHubConfigSource{
			Repo: "acme/config", Path: "c.yaml", Token: "t", APIURL: server.URL,
		}
		_, err := fetchGitHubConfig(context.Background(), src)
		assert.Error(t, err)
	})
}

func TestGitHubSourceConfigured(t *testing.T) {
	assert.False(t, gitHubConfigSource{}.configured())
	assert.False(t, gitHubConfigSource{Repo: "a/b", Path: "c"}.configured())
	assert.True(t, gitHubConfigSource{Repo: "a/b", Path: "c", Token: "t"}.configured())

	assert.Equal(t, "https://api.github.com", gitHubConfigSource{}.apiURL())
	assert.Equal(
		t, "https://ghes.example.com/api/v3",
		gitHubConfigSource{APIURL: "https://ghes.example.com/api/v3/"}.apiURL(),
	)
}
