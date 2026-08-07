package main

import (
	"encoding/json"
	"strings"
)

// headerValue strips CR and LF from anything interpolated into a mail header.
//
// smtp.SendMail validates the envelope addresses it is given but never the
// message it sends, so a "\r\nBcc: ..." in an alert title or in the instance
// name injected headers into every subscriber's copy -- past the recipient
// list the admin could see.
var headerValueReplacer = strings.NewReplacer("\r", " ", "\n", " ")

func headerValue(v string) string {
	return headerValueReplacer.Replace(v)
}

// jsonString escapes a value for interpolation into a JSON string literal.
//
// The Slack payloads are built by text/template, which does no escaping at
// all, so a double quote or a backslash in an alert title or message produced
// malformed JSON -- or injected Slack blocks. Marshalling the whole payload
// would be better still, but this keeps the block layout in one readable
// place while making every interpolation safe.
func jsonString(v string) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		// json.Marshal of a string cannot fail; invalid UTF-8 is replaced.
		return ""
	}

	return string(encoded[1 : len(encoded)-1])
}
