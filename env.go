package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// env holds every setting Statusnook reads from the environment. It is
// populated once, before the database is opened, and never mutated after.
//
// see docs/deployment.md
var env envConfig

type envConfig struct {
	DataDir string

	// TrustProxy makes Statusnook believe X-Forwarded-Proto/X-Forwarded-For.
	// Only enable it when a reverse proxy in front of Statusnook sets them.
	TrustProxy bool

	// Name, when set, overrides the status page name on every boot.
	Name string

	// Domain is the public hostname used in notification links, and the
	// hostname certificates are issued for when TLS is "auto".
	Domain string

	// TLS is "auto" (managed Let's Encrypt certificates, needs ports 80 and
	// 443 reachable from the internet) or "off" (plain HTTP, for a reverse
	// proxy). Empty leaves whatever the database already says.
	TLS string

	// AdminUsername/AdminPassword provision the first admin account without
	// the setup wizard, so a container can come up ready to use.
	AdminUsername string
	AdminPassword string

	// SelfUpdateDisabled blocks the in-place binary replacement. Always true
	// under Docker: the image owns the binary.
	SelfUpdateDisabled bool

	GitHub gitHubEnvConfig
}

type gitHubEnvConfig struct {
	// Repo is "owner/name". Accepts a full https://github.com/owner/name URL.
	Repo string
	// Branch defaults to the repository's default branch.
	Branch string
	// Path is the config file's path inside the repository.
	Path string
	// Token is a read-only PAT. Held in memory only, never written to the
	// database and never rendered into a page.
	Token string
	// PollInterval is how often the repository is checked for a new commit.
	// Zero disables polling (webhook-only).
	PollInterval time.Duration
	// WebhookSecret, when set, enables the /github-config-webhook endpoint.
	WebhookSecret string
	// APIURL points at a GitHub Enterprise Server API root.
	APIURL string
}

// Managed reports whether the environment owns the configuration, which makes
// the config settings page read-only.
func (g gitHubEnvConfig) Managed() bool {
	return g.Repo != "" && g.Token != ""
}

var repoPathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

var domainPattern = regexp.MustCompile(
	`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`,
)

// loadEnv reads the STATUSNOOK_* environment. Invalid values are fatal: a
// container that silently ignores half its configuration is worse than one
// that refuses to start.
func loadEnv() (envConfig, error) {
	cfg := envConfig{
		DataDir:            envString("DATA_DIR", "statusnook-data"),
		TrustProxy:         false,
		Name:               envString("NAME", ""),
		SelfUpdateDisabled: *dockerFlag,
	}

	var err error

	cfg.TrustProxy, err = envBool("TRUST_PROXY", false)
	if err != nil {
		return cfg, err
	}

	disableUpdate, err := envBool("DISABLE_SELF_UPDATE", cfg.SelfUpdateDisabled)
	if err != nil {
		return cfg, err
	}
	cfg.SelfUpdateDisabled = disableUpdate

	cfg.Domain = strings.ToLower(envString("DOMAIN", ""))
	if cfg.Domain != "" && !domainPattern.MatchString(cfg.Domain) {
		return cfg, fmt.Errorf("STATUSNOOK_DOMAIN is not a valid hostname: %q", cfg.Domain)
	}

	cfg.TLS = strings.ToLower(envString("TLS", ""))
	switch cfg.TLS {
	case "", "off", "auto":
	default:
		return cfg, fmt.Errorf("STATUSNOOK_TLS must be auto or off, got %q", cfg.TLS)
	}
	if cfg.TLS == "auto" && cfg.Domain == "" {
		return cfg, fmt.Errorf("STATUSNOOK_TLS=auto requires STATUSNOOK_DOMAIN")
	}

	cfg.AdminUsername = envString("ADMIN_USERNAME", "")
	cfg.AdminPassword, err = envSecret("ADMIN_PASSWORD")
	if err != nil {
		return cfg, err
	}
	if (cfg.AdminUsername == "") != (cfg.AdminPassword == "") {
		return cfg, fmt.Errorf(
			"STATUSNOOK_ADMIN_USERNAME and STATUSNOOK_ADMIN_PASSWORD must be set together",
		)
	}
	if cfg.AdminPassword != "" && len(cfg.AdminPassword) < 8 {
		return cfg, fmt.Errorf("STATUSNOOK_ADMIN_PASSWORD must be at least 8 characters")
	}

	gh := gitHubEnvConfig{
		Branch: envString("GITHUB_BRANCH", ""),
		Path:   envString("GITHUB_CONFIG_PATH", "statusnook.yaml"),
		APIURL: strings.TrimSuffix(envString("GITHUB_API_URL", "https://api.github.com"), "/"),
	}

	gh.Repo, err = normalizeRepo(envString("GITHUB_REPO", ""))
	if err != nil {
		return cfg, err
	}

	gh.Token, err = envSecret("GITHUB_TOKEN")
	if err != nil {
		return cfg, err
	}

	gh.WebhookSecret, err = envSecret("GITHUB_WEBHOOK_SECRET")
	if err != nil {
		return cfg, err
	}

	gh.PollInterval, err = envDuration("GITHUB_POLL_INTERVAL", time.Minute)
	if err != nil {
		return cfg, err
	}
	if gh.PollInterval != 0 && gh.PollInterval < 10*time.Second {
		return cfg, fmt.Errorf("STATUSNOOK_GITHUB_POLL_INTERVAL must be 0 or at least 10s")
	}

	if gh.Repo != "" && gh.Token == "" {
		return cfg, fmt.Errorf(
			"STATUSNOOK_GITHUB_REPO is set but STATUSNOOK_GITHUB_TOKEN is empty",
		)
	}
	if gh.Token != "" && gh.Repo == "" {
		return cfg, fmt.Errorf(
			"STATUSNOOK_GITHUB_TOKEN is set but STATUSNOOK_GITHUB_REPO is empty",
		)
	}
	if gh.Repo != "" && gh.Path == "" {
		return cfg, fmt.Errorf("STATUSNOOK_GITHUB_CONFIG_PATH must not be empty")
	}

	cfg.GitHub = gh

	return cfg, nil
}

// normalizeRepo accepts "owner/name" or any GitHub URL pointing at a
// repository and returns "owner/name".
func normalizeRepo(v string) (string, error) {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "/"))
	if v == "" {
		return "", nil
	}

	if strings.Contains(v, "://") {
		parsed, err := url.Parse(v)
		if err != nil {
			return "", fmt.Errorf("STATUSNOOK_GITHUB_REPO: %w", err)
		}
		v = strings.Trim(parsed.Path, "/")
	}

	v = strings.TrimSuffix(v, ".git")

	if !repoPathPattern.MatchString(v) {
		return "", fmt.Errorf(
			"STATUSNOOK_GITHUB_REPO must be owner/name, got %q", v,
		)
	}

	return v, nil
}

func envString(name string, fallback string) string {
	v, ok := os.LookupEnv("STATUSNOOK_" + name)
	if !ok {
		return fallback
	}

	return strings.TrimSpace(v)
}

// envSecret reads a secret from STATUSNOOK_<name>, or from the file named by
// STATUSNOOK_<name>_FILE. The file form keeps secrets out of the process
// environment, which is what Docker/Kubernetes secret mounts provide.
func envSecret(name string) (string, error) {
	file := envString(name+"_FILE", "")
	inline := envString(name, "")

	if file != "" && inline != "" {
		return "", fmt.Errorf(
			"STATUSNOOK_%s and STATUSNOOK_%s_FILE are mutually exclusive", name, name,
		)
	}

	if file == "" {
		return inline, nil
	}

	contents, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("STATUSNOOK_%s_FILE: %w", name, err)
	}

	return strings.TrimSpace(string(contents)), nil
}

// envInt reads a port or count. Zero means "not set".
func envInt(name string, fallback int) (int, error) {
	v := envString(name, "")
	if v == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(v)
	if err != nil {
		return fallback, fmt.Errorf("STATUSNOOK_%s must be a number, got %q", name, v)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	v := envString(name, "")
	if v == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseBool(strings.ToLower(v))
	if err != nil {
		return fallback, fmt.Errorf("STATUSNOOK_%s must be true or false, got %q", name, v)
	}

	return parsed, nil
}

// envDuration accepts a Go duration ("90s", "5m") or a bare number of seconds.
func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	v := envString(name, "")
	if v == "" {
		return fallback, nil
	}

	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return fallback, fmt.Errorf("STATUSNOOK_%s must not be negative", name)
		}
		return time.Duration(secs) * time.Second, nil
	}

	parsed, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf(
			"STATUSNOOK_%s must be a duration such as 60s or 5m, got %q", name, v,
		)
	}
	if parsed < 0 {
		return fallback, fmt.Errorf("STATUSNOOK_%s must not be negative", name)
	}

	return parsed, nil
}

// isFlagSet reports whether a flag was given on the command line, which lets a
// flag win over its environment variable counterpart.
func isFlagSet(name string) bool {
	found := false

	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})

	return found
}
