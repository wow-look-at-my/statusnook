package main

import (
	_ "embed"
	"github.com/caddyserver/certmagic"
)

var BUILD = "dev"
var CA = certmagic.LetsEncryptStagingCA
var VERSION = "v0.3.0"

//go:embed schema.sql
var sqlSchema string

const SELF_SIGNED_CERT_NAME = "self-signed-cert.pem"
const SELF_SIGNED_KEY_NAME = "self-signed-key.pem"
