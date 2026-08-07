package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Slack payload is assembled by text/template, which escapes nothing, so
// an alert title containing a quote or a backslash produced malformed JSON --
// or injected Slack blocks of its own.
func TestJSONStringKeepsThePayloadValid(t *testing.T) {
	hostile := `Outage", "type": "mrkdwn", "x": "` + "\\" + "\n\ttab\there"

	payload := `{"text": "` + jsonString(hostile) + `"}`

	var decoded struct {
		Text string `json:"text"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded),
		"escaped payload is not valid JSON: %s", payload)

	// Round-trips: escaping must not lose or alter the text.
	require.Equal(t, hostile, decoded.Text)

	// And the injected keys stayed inside the string rather than becoming
	// siblings of it.
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &raw))
	require.Len(t, raw, 1)
}
