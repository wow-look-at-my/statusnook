package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Postmark holds its own suppression list -- addresses that bounced or marked
// a message as spam -- and refuses to deliver to them. When statusnook is not
// managing subscriptions itself it treats that list as the truth, so a
// subscribe request first pulls it down and deactivates anything on it.
type fakePostmark struct {
	server      *httptest.Server
	suppressed  atomic.Pointer[[]string]
	dumpStatus  atomic.Int64
	deleted     atomic.Pointer[[]string]
	deleteCalls atomic.Int64
}

func newFakePostmark(t *testing.T) *fakePostmark {
	t.Helper()

	pm := &fakePostmark{}
	pm.dumpStatus.Store(http.StatusOK)
	pm.suppressed.Store(&[]string{})
	pm.deleted.Store(&[]string{})

	pm.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "postmark-token", r.Header.Get("X-Postmark-Server-Token"))

		if strings.HasSuffix(r.URL.Path, "/suppressions/dump") {
			status := int(pm.dumpStatus.Load())
			if status != http.StatusOK {
				w.WriteHeader(status)
				w.Write([]byte(`{"Message":"nope"}`))
				return
			}

			entries := []map[string]any{}
			for _, address := range *pm.suppressed.Load() {
				entries = append(entries, map[string]any{
					"EmailAddress":      address,
					"SuppressionReason": "HardBounce",
					"Origin":            "Recipient",
					"CreatedAt":         time.Now().UTC(),
				})
			}

			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"Suppressions": entries,
			}))

			return
		}

		if strings.HasSuffix(r.URL.Path, "/suppressions/delete") {
			pm.deleteCalls.Add(1)

			var body struct {
				Suppressions []struct {
					EmailAddress string
				}
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

			deleted := append([]string(nil), *pm.deleted.Load()...)
			for _, s := range body.Suppressions {
				deleted = append(deleted, s.EmailAddress)
			}
			pm.deleted.Store(&deleted)

			w.Write([]byte(`{"Suppressions":[]}`))

			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(pm.server.Close)

	previous := postmarkAPIBaseURL
	postmarkAPIBaseURL = pm.server.URL
	t.Cleanup(func() { postmarkAPIBaseURL = previous })

	return pm
}

func (p *fakePostmark) suppress(addresses ...string) {
	p.suppressed.Store(&addresses)
}

// Wires the alert channel at a fake SMTP server that claims to be Postmark, so
// the suppression sync runs. Managed subscriptions off is what turns it on.
func (a *testApp) usePostmarkForAlerts(smtp *fakeSMTP) {
	a.t.Helper()

	before := a.channelIDs()
	resp := a.post("/admin/notifications/create", url.Values{
		"type":             {"smtp"},
		"display-name":     {"Postmark"},
		"host":             {"smtp.postmarkapp.com"},
		"port":             {strconv.Itoa(smtp.port)},
		"username":         {"postmark-token"},
		"password":         {"postmark-token"},
		"from":             {"status@example.com"},
		"pm-transactional": {"outbound"},
		"pm-broadcast":     {"broadcast"},
	})
	require.Less(a.t, resp.status, 400, resp.body)

	channelID := onlyNewID(a.t, before, a.channelIDs())

	resp = a.post("/admin/alerts/notifications",
		url.Values{"smtp-notification-channel": {strconv.Itoa(channelID)}})
	require.Less(a.t, resp.status, 400, resp.body)
}

// The sync is rate limited to once every ten seconds across the process, so
// each test that wants it to run has to reset that.
func allowSuppressionSync() {
	supressionSyncMu.Lock()
	lastSuppressionSync = time.Time{}
	supressionSyncMu.Unlock()
}

func TestSubscribeSyncsPostmarkSuppressions(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	pm := newFakePostmark(t)
	app.usePostmarkForAlerts(smtp)

	// Three live subscribers, one of whom Postmark has since suppressed.
	for _, address := range []string{
		"bounced@example.com", "fine@example.com", "asking@example.com",
	} {
		app.addPendingSubscription(address, "tok-"+address)
		require.Equal(t, http.StatusFound,
			app.post("/subscribe/email/confirm?token=tok-"+address, nil).status)
	}

	require.True(t, app.subscription("bounced@example.com").Active)

	pm.suppress("bounced@example.com")
	allowSuppressionSync()

	// The sync runs before the handler notices this address is already
	// subscribed, and that early return is what keeps the test off the SMTP
	// send -- the channel claims to be smtp.postmarkapp.com, which is a real
	// host that would be dialled for real.
	resp := app.post("/subscribe/email", url.Values{"email": {"asking@example.com"}})
	require.Less(t, resp.status, 400, resp.body)
	require.Contains(t, resp.body, "already subscribed")

	require.False(t, app.subscription("bounced@example.com").Active,
		"an address Postmark refuses to deliver to must not stay subscribed")
	require.True(t, app.subscription("fine@example.com").Active,
		"the sync must only touch what is actually suppressed")

	// Confirming an address lifts its suppression, or the confirmation itself
	// would be the last message it ever got. The confirm handler sends nothing.
	pm.suppress("bounced@example.com")
	app.addPendingSubscription("bounced@example.com", "tok-again")
	require.Equal(t, http.StatusFound,
		app.post("/subscribe/email/confirm?token=tok-again", nil).status)

	require.Positive(t, pm.deleteCalls.Load(), "the suppression was never lifted")
	require.Contains(t, *pm.deleted.Load(), "bounced@example.com")
}

// Postmark being unreachable or unhappy is a failure, not a subscribe that
// silently skips the check -- the list is the only thing that stops statusnook
// mailing an address Postmark will refuse.
func TestSubscribeFailsWhenPostmarkDoes(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	pm := newFakePostmark(t)
	app.usePostmarkForAlerts(smtp)

	pm.dumpStatus.Store(http.StatusUnauthorized)
	allowSuppressionSync()

	resp := app.post("/subscribe/email", url.Values{"email": {"new@example.com"}})
	require.Equal(t, http.StatusInternalServerError, resp.status)

	previous := postmarkAPIBaseURL
	postmarkAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { postmarkAPIBaseURL = previous })

	allowSuppressionSync()
	resp = app.post("/subscribe/email", url.Values{"email": {"new@example.com"}})
	require.Equal(t, http.StatusInternalServerError, resp.status)
}

// With statusnook managing subscriptions the list is its own, so Postmark is
// never asked.
func TestManagedSubscriptionsSkipPostmark(t *testing.T) {
	app := withTestApp(t)
	smtp := newFakeSMTP(t)
	pm := newFakePostmark(t)

	before := app.channelIDs()
	resp := app.post("/admin/notifications/create", url.Values{
		"type":             {"smtp"},
		"display-name":     {"Postmark"},
		"host":             {"smtp.postmarkapp.com"},
		"port":             {strconv.Itoa(smtp.port)},
		"username":         {"postmark-token"},
		"password":         {"postmark-token"},
		"from":             {"status@example.com"},
		"pm-transactional": {"outbound"},
		"pm-broadcast":     {"broadcast"},
	})
	require.Less(t, resp.status, 400, resp.body)

	channelID := onlyNewID(t, before, app.channelIDs())
	require.Less(t, app.post("/admin/alerts/notifications", url.Values{
		"smtp-notification-channel": {strconv.Itoa(channelID)},
		"managed-subscriptions":     {"on"},
	}).status, 400)

	app.addPendingSubscription("asking@example.com", "tok")
	require.Equal(t, http.StatusFound, app.post("/subscribe/email/confirm?token=tok", nil).status)

	pm.dumpStatus.Store(http.StatusInternalServerError)
	allowSuppressionSync()

	// Already subscribed, so the handler answers before the SMTP send. What is
	// being checked is that Postmark answering badly did not stop it.
	resp = app.post("/subscribe/email", url.Values{"email": {"asking@example.com"}})
	require.Less(t, resp.status, 400, resp.body,
		"Postmark answering badly must not matter when its list is not consulted")
	require.Zero(t, pm.deleteCalls.Load())
}
