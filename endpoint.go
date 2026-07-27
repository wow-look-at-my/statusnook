package main

// Third-party endpoints, in one place. They are variables rather than constants
// so tests can point them at a local server: every one of these code paths used
// to be unreachable without talking to the real service.
var (
	// releaseAPIURL is the GitHub API release feed the update check reads.
	releaseAPIURL = "https://api.github.com/repos/goksan/statusnook/releases/latest"

	// slackTokenURL exchanges a Slack OAuth code for a webhook.
	slackTokenURL = "https://slack.com/api/oauth.v2.access"

	// postmarkAPIURL is the base for Postmark's suppression endpoints, used to
	// keep managed email subscriptions in step with bounces.
	postmarkAPIURL = "https://api.postmarkapp.com"
)
