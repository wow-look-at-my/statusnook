package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A notification channel is decoded as a bare map, so its fields arrive as
// whatever YAML made of them. Every one of those has a type the app needs, and
// a value of the wrong shape has to be reported rather than silently stored as
// a zero.
func TestApplyConfigReportsWronglyTypedChannelFields(t *testing.T) {
	tx := beginConfigTx(t)

	joined := strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
notification-channels:
  numbers:
    type: 12
  named:
    type: slack
    name: 34
    webhook-url: https://hooks.example.com/x
  strings:
    type: smtp
    name: Strings
    host: 99
    port: "587"
    username: 1
    password: 2
    from: 3
    headers: not-a-map
    misc: not-a-map
  values:
    type: smtp
    name: Values
    host: smtp.example.com
    port: 587
    username: statusnook
    password: shh
    from: status@example.com
    headers:
      X-Bad: [1, 2]
      X-Number: 7
    misc:
      pm-bad: {nested: 1}
`), "\n")

	require.Contains(t, joined, "notification-channels.numbers: type is invalid")
	require.Contains(t, joined, "notification-channels.named: name is invalid")
	require.Contains(t, joined, "notification-channels.strings: host is invalid")
	require.Contains(t, joined, "notification-channels.strings: port is invalid")
	require.Contains(t, joined, "notification-channels.strings: username is invalid")
	require.Contains(t, joined, "notification-channels.strings: headers is invalid")
	require.Contains(t, joined, "notification-channels.strings: misc is invalid")
	require.Contains(t, joined, "invalid header value X-Bad")
	require.Contains(t, joined, "invalid misc value pm-bad")

	// A number is a legal header value; it is only stored as its text.
	require.NotContains(t, joined, "invalid header value X-Number")
}

// A missing field is different from one that is present and empty, and both
// have to be reported -- the config file is the whole interface here.
func TestApplyConfigReportsMissingAndEmptyChannelFields(t *testing.T) {
	tx := beginConfigTx(t)

	joined := strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
notification-channels:
  absent:
    type: smtp
  empty:
    type: smtp
    name: ""
    host: ""
    port: 0
    username: ""
    password: ""
    from: ""
  badfrom:
    type: smtp
    name: Bad From
    host: smtp.example.com
    port: 587
    username: statusnook
    password: shh
    from: not-an-address
  noslack:
    type: slack
    name: No Webhook
`), "\n")

	require.Contains(t, joined, "notification-channels.absent: name is required")
	require.Contains(t, joined, "notification-channels.absent: host is required")
	require.Contains(t, joined, "notification-channels.empty: port is required")
	require.Contains(t, joined, "notification-channels.empty: password is required")
	require.Contains(t, joined, "notification-channels.badfrom: from is an invalid email address")
	require.Contains(t, joined, "notification-channels.noslack: webhook-url is required")
}

// The monitor fields the app will not send with, and the two shapes a request
// body is allowed to take.
func TestApplyConfigReportsBadMonitorValues(t *testing.T) {
	tx := beginConfigTx(t)

	joined := strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
services:
  Bad_Service_Slug:
    name: Bad
  nameless:
    description: no name
monitors:
  bad:
    name: ""
    url: ""
    method: TRACE
    frequency: 45
    timeout: 7
    attempts: 9
  badbody:
    name: Bad body
    url: https://example.com
    method: POST
    frequency: 60
    timeout: 5
    attempts: 1
    body: 7
  badbodyvalue:
    name: Bad body value
    url: https://example.com
    method: POST
    frequency: 60
    timeout: 5
    attempts: 1
    body:
      enabled: true
  Bad_Monitor_Slug:
    name: Bad slug
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
`), "\n")

	require.Contains(t, joined, "monitors.bad: name is required")
	require.Contains(t, joined, "monitors.bad: url is required")
	require.Contains(t, joined, "monitors.bad: frequency must be one of 10, 30, 60")
	require.Contains(t, joined, "monitors.bad: timeout must be one of 5, 10, 15")
	require.Contains(t, joined, "monitors.bad: attempts must be one of 1, 2, 3")
	require.Contains(t, joined, "monitors.badbody: body is invalid")
	require.Contains(t, joined, "monitors.badbodyvalue: invalid body value enabled")
	require.Contains(t, joined, "monitors.Bad_Monitor_Slug: must only contain")
	require.Contains(t, joined, "services.Bad_Service_Slug: must only contain")
	require.Contains(t, joined, "services.nameless: name is required")
}

// Renaming reaches into four different tables, and each has its own branch for
// a source that is not there and a target that is taken.
func TestApplyConfigRenamesEveryEntityType(t *testing.T) {
	tx := beginConfigTx(t)

	require.Empty(t, applyTestConfig(t, tx, fullConfig))

	// A source that does not exist, once per entity type.
	joined := strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
rename:
  services.nothing: a
  monitors.nothing: b
  notification-channels.nothing: c
  mail-groups.nothing: d
services:
  a:
    name: A
monitors:
  b:
    name: B
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
notification-channels:
  c:
    type: slack
    name: C
    webhook-url: https://hooks.example.com/x
mail-groups:
  d:
    name: D
    members:
      - d@example.com
`), "\n")

	for _, key := range []string{
		"services.nothing", "monitors.nothing",
		"notification-channels.nothing", "mail-groups.nothing",
	} {
		require.Contains(t, joined, key, "the missing %s rename went unreported", key)
	}

	// And a target each entity type already holds. The rename stage runs before
	// the entity stages, so the collision only happens when the target is
	// already in the database -- which is what this first apply puts there.
	require.Empty(t, applyTestConfig(t, tx, taken))

	joined = strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
rename:
  services.a: taken-service
  monitors.b: taken-monitor
  notification-channels.c: taken-channel
  mail-groups.d: taken-group
`+takenBody), "\n")

	require.Contains(t, joined, "would not be unique")
}

// Monitors name their channels and mail groups by slug, and a name that
// resolves to nothing means an outage nobody is told about.
func TestApplyConfigReportsDanglingMonitorReferences(t *testing.T) {
	tx := beginConfigTx(t)

	joined := strings.Join(applyTestConfig(t, tx, `
general-settings:
  name: Test Status
notification-channels:
  chat:
    type: slack
    name: Chat
    webhook-url: https://hooks.example.com/x
mail-groups:
  core:
    name: Core
    members:
      - core@example.com
monitors:
  good:
    name: Good
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
    notification-channels:
      - chat
    mail-groups:
      - core
  dangling:
    name: Dangling
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
    notification-channels:
      - chat
      - nowhere
    mail-groups:
      - core
      - nobody
`), "\n")

	require.Contains(t, joined, `unknown channel "nowhere"`)
	require.Contains(t, joined, `unknown mail group "nobody"`)
	require.NotContains(t, joined, "monitors.good")
}

// Both keys of every pair, so a rename onto the second one collides.
const takenBody = `services:
  a:
    name: A
  taken-service:
    name: Taken
monitors:
  b:
    name: B
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
  taken-monitor:
    name: Taken
    url: https://example.com
    method: GET
    frequency: 60
    timeout: 5
    attempts: 1
notification-channels:
  c:
    type: slack
    name: C
    webhook-url: https://hooks.example.com/x
  taken-channel:
    type: slack
    name: Taken
    webhook-url: https://hooks.example.com/y
mail-groups:
  d:
    name: D
    members:
      - d@example.com
  taken-group:
    name: Taken
    members:
      - taken@example.com
`

const taken = `general-settings:
  name: Test Status
` + takenBody
