package main

import "strings"

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
