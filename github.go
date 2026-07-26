package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxConfigSize bounds every config document Statusnook will read, whether it
// arrives from GitHub or a form post. The GitHub contents API itself refuses to
// inline anything above 1 MiB.
const maxConfigSize = 1 << 20

// gitHubAPIVersion pins the REST API media type. Without it GitHub is free to
// change response shapes under us.
const gitHubAPIVersion = "2022-11-28"

var gitHubClient = &http.Client{Timeout: 30 * time.Second}

// gitHubConfigSource is everything needed to pull one config file out of one
// repository.
type gitHubConfigSource struct {
	Repo   string // owner/name
	Branch string // empty means the repository's default branch
	Path   string
	Token  string
	APIURL string

	// FromEnv marks a source configured through the environment, which the
	// admin UI must not be able to change.
	FromEnv bool
}

func (s gitHubConfigSource) configured() bool {
	return s.Repo != "" && s.Path != "" && s.Token != ""
}

// apiURL returns the API root, defaulting to github.com.
func (s gitHubConfigSource) apiURL() string {
	if s.APIURL == "" {
		return "https://api.github.com"
	}

	return strings.TrimSuffix(s.APIURL, "/")
}

// envGitHubConfigSource builds the source described by the environment.
func envGitHubConfigSource() gitHubConfigSource {
	return gitHubConfigSource{
		Repo:    env.GitHub.Repo,
		Branch:  env.GitHub.Branch,
		Path:    env.GitHub.Path,
		Token:   env.GitHub.Token,
		APIURL:  env.GitHub.APIURL,
		FromEnv: true,
	}
}

// gitHubConfigSourceFromDB reads the source the admin UI stored. The
// environment always wins when it is configured.
func gitHubConfigSourceFromDB(tx *sql.Tx) (gitHubConfigSource, error) {
	if env.GitHub.Managed() {
		return envGitHubConfigSource(), nil
	}

	src := gitHubConfigSource{}

	get := func(name string) (string, error) {
		v, err := getMetaValue(tx, name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("gitHubConfigSourceFromDB.%s: %w", name, err)
		}

		return v, nil
	}

	repoURL, err := get("githubRepoURL")
	if err != nil {
		return src, err
	}
	if repoURL != "" {
		repo, err := normalizeRepo(repoURL)
		if err != nil {
			return src, fmt.Errorf("gitHubConfigSourceFromDB.normalizeRepo: %w", err)
		}
		src.Repo = repo
	}

	if src.Branch, err = get("githubConfigBranch"); err != nil {
		return src, err
	}
	if src.Path, err = get("githubConfigPath"); err != nil {
		return src, err
	}
	if src.Token, err = get("githubConfigToken"); err != nil {
		return src, err
	}

	return src, nil
}

type gitHubAPIError struct {
	StatusCode int
	Message    string
}

func (e *gitHubAPIError) Error() string {
	return fmt.Sprintf("github api: %d %s", e.StatusCode, e.Message)
}

// notFound reports whether the repository, branch or path does not exist, or
// the token cannot see it. GitHub deliberately answers 404 for both cases on
// private repositories.
func (e *gitHubAPIError) notFound() bool {
	return e.StatusCode == http.StatusNotFound
}

func (e *gitHubAPIError) unauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

// gitHubRequest issues an authenticated GET and decodes a JSON response. The
// token is never included in returned errors.
func gitHubRequest(ctx context.Context, src gitHubConfigSource, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("gitHubRequest.NewRequest: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)
	if src.Token != "" {
		req.Header.Set("Authorization", "Bearer "+src.Token)
	}

	resp, err := gitHubClient.Do(req)
	if err != nil {
		// url.Error embeds the request URL, never the credentials.
		return fmt.Errorf("gitHubRequest.Do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

		message := strings.TrimSpace(string(body))
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			message = apiErr.Message
		}

		return &gitHubAPIError{StatusCode: resp.StatusCode, Message: message}
	}

	if out == nil {
		return nil
	}

	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8*maxConfigSize))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("gitHubRequest.Decode: %w", err)
	}

	return nil
}

// checkGitHubRepo confirms the token can read the repository.
func checkGitHubRepo(ctx context.Context, src gitHubConfigSource) error {
	return gitHubRequest(ctx, src, src.apiURL()+"/repos/"+src.Repo, nil)
}

type gitHubConfigFile struct {
	Content []byte
	SHA     string
}

// fetchGitHubConfig downloads the config file. The returned SHA is the blob
// SHA, which changes only when the file's contents change.
func fetchGitHubConfig(ctx context.Context, src gitHubConfigSource) (gitHubConfigFile, error) {
	file := gitHubConfigFile{}

	if !src.configured() {
		return file, errors.New("fetchGitHubConfig: incomplete github configuration")
	}

	endpoint := src.apiURL() + "/repos/" + src.Repo + "/contents/" +
		escapeContentsPath(src.Path)
	if src.Branch != "" {
		endpoint += "?ref=" + url.QueryEscape(src.Branch)
	}

	var contents struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int    `json:"size"`
		SHA      string `json:"sha"`
	}

	if err := gitHubRequest(ctx, src, endpoint, &contents); err != nil {
		return file, err
	}

	if contents.Type != "file" {
		return file, fmt.Errorf("fetchGitHubConfig: %q is a %s, not a file", src.Path, contents.Type)
	}

	if contents.Size > maxConfigSize {
		return file, fmt.Errorf(
			"fetchGitHubConfig: %q is %d bytes, limit is %d",
			src.Path, contents.Size, maxConfigSize,
		)
	}

	if contents.Encoding != "base64" {
		return file, fmt.Errorf("fetchGitHubConfig: unexpected encoding %q", contents.Encoding)
	}

	// GitHub wraps base64 payloads at 60 columns.
	decoded, err := base64.StdEncoding.DecodeString(
		strings.NewReplacer("\n", "", "\r", "").Replace(contents.Content),
	)
	if err != nil {
		return file, fmt.Errorf("fetchGitHubConfig.DecodeString: %w", err)
	}

	file.Content = decoded
	file.SHA = contents.SHA

	return file, nil
}

// escapeContentsPath percent-escapes each path segment so a path containing
// spaces or "#" cannot break out of the contents endpoint.
func escapeContentsPath(p string) string {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	return strings.Join(segments, "/")
}

// syncMu serialises config syncs so a webhook delivery and the poll loop can
// never apply two revisions concurrently.
var syncMu sync.Mutex

// syncGitHubConfig fetches the config file and applies it when its blob SHA
// differs from the last applied one. It reports whether anything was applied.
func syncGitHubConfig(ctx context.Context, src gitHubConfigSource) (bool, error) {
	syncMu.Lock()
	defer syncMu.Unlock()

	file, err := fetchGitHubConfig(ctx, src)
	if err != nil {
		return false, err
	}

	tx, err := rwDB.Begin()
	if err != nil {
		return false, fmt.Errorf("syncGitHubConfig.Begin: %w", err)
	}
	defer tx.Rollback()

	lastSHA, err := getMetaValue(tx, "githubConfigSHA")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("syncGitHubConfig.getMetaValueGitHubConfigSHA: %w", err)
	}

	if lastSHA == file.SHA {
		return false, nil
	}

	configErrors := ""
	msgs, err := applyConfig(tx, file.Content)
	if err != nil {
		unwrappedErr := errors.Unwrap(err)
		if unwrappedErr == nil || !strings.HasPrefix(unwrappedErr.Error(), "yaml:") {
			return false, fmt.Errorf("syncGitHubConfig.applyConfig: %w", err)
		}

		msgs = append(msgs, strings.TrimPrefix(unwrappedErr.Error(), "yaml: "))
	}

	if len(msgs) > 0 {
		msgsBytes, err := json.Marshal(msgs)
		if err != nil {
			return false, fmt.Errorf("syncGitHubConfig.Marshal: %w", err)
		}
		configErrors = string(msgsBytes)
	}

	if err := updateMetaValue(tx, "githubConfigErrors", configErrors); err != nil {
		return false, fmt.Errorf("syncGitHubConfig.updateMetaValueGitHubConfigErrors: %w", err)
	}

	if err := updateMetaValue(tx, "configFile", string(file.Content)); err != nil {
		return false, fmt.Errorf("syncGitHubConfig.updateMetaValueConfigFile: %w", err)
	}

	if err := updateMetaValue(tx, "githubConfigSHA", file.SHA); err != nil {
		return false, fmt.Errorf("syncGitHubConfig.updateMetaValueGitHubConfigSHA: %w", err)
	}

	name, err := getMetaValue(tx, "name")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("syncGitHubConfig.getMetaValueName: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("syncGitHubConfig.Commit: %w", err)
	}

	metaName = name

	if len(msgs) > 0 {
		log.Printf("github config applied with %d problem(s):", len(msgs))
		for _, msg := range msgs {
			log.Printf("  config: %s", msg)
		}
	}

	return true, nil
}

// currentGitHubConfigSource resolves the active source, preferring the
// environment over anything stored by the admin UI.
func currentGitHubConfigSource() (gitHubConfigSource, error) {
	if env.GitHub.Managed() {
		return envGitHubConfigSource(), nil
	}

	tx, err := db.Begin()
	if err != nil {
		return gitHubConfigSource{}, fmt.Errorf("currentGitHubConfigSource.Begin: %w", err)
	}
	defer tx.Rollback()

	managed, err := getMetaValue(tx, "githubManagedConfig")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return gitHubConfigSource{}, fmt.Errorf(
			"currentGitHubConfigSource.getMetaValueGitHubManagedConfig: %w", err,
		)
	}
	if managed != "true" {
		return gitHubConfigSource{}, nil
	}

	return gitHubConfigSourceFromDB(tx)
}

// gitHubConfigSyncLoop polls the configured repository. Polling is what makes
// a private repository usable from a network GitHub cannot reach: no inbound
// webhook, no port forwarding.
func gitHubConfigSyncLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	interval := env.GitHub.PollInterval
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// A failing sync backs off up to 16 intervals so a wrong token does not
	// hammer the API, while a transient failure still recovers quickly.
	failures := 0

	for {
		src, err := currentGitHubConfigSource()
		if err != nil {
			log.Printf("gitHubConfigSyncLoop.currentGitHubConfigSource: %s", err)
		} else if src.configured() {
			changed, err := syncGitHubConfig(ctx, src)
			switch {
			case err != nil && ctx.Err() != nil:
				// shutting down
			case err != nil:
				failures++
				log.Printf("github config sync failed: %s", describeGitHubError(err))
			default:
				if failures > 0 {
					log.Printf("github config sync recovered")
				}
				failures = 0
				if changed {
					log.Printf("github config sync applied a new revision")
				}
			}
		}

		wait := interval << min(failures, 4)
		ticker.Reset(wait)

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// describeGitHubError turns an API failure into something an operator can act
// on without reading the source.
func describeGitHubError(err error) string {
	var apiErr *gitHubAPIError
	if !errors.As(err, &apiErr) {
		return err.Error()
	}

	switch {
	case apiErr.notFound():
		return "repository, branch or config path not found - check " +
			"STATUSNOOK_GITHUB_REPO, STATUSNOOK_GITHUB_BRANCH, " +
			"STATUSNOOK_GITHUB_CONFIG_PATH, and that the token grants read " +
			"access to the repository (" + apiErr.Message + ")"
	case apiErr.unauthorized():
		return "token rejected - check STATUSNOOK_GITHUB_TOKEN has contents:read " +
			"on the repository (" + apiErr.Message + ")"
	default:
		return apiErr.Error()
	}
}
