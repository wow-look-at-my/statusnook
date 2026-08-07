package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
)

// The meta* globals are read by getPageCtx on essentially every request and
// written by admin handlers and by monitorUnconfirmedDomainLoop, a background
// goroutine on a one-minute timer. Unsynchronised, that is a data race the Go
// race detector flags immediately: a string header is a pointer and a length,
// so a reader can observe a torn value, and there is no happens-before edge to
// stop a handler serving a stale domain indefinitely.
//
// metaConfigFileEnabled is worse than stale text -- it gates every mutating
// admin handler, so a racy read is a correctness gate read wrong.
type atomicString struct {
	v atomic.Pointer[string]
}

func (a *atomicString) Load() string {
	p := a.v.Load()
	if p == nil {
		return ""
	}

	return *p
}

func (a *atomicString) Store(s string) {
	a.v.Store(&s)
}

// Fills the meta* atomics from the meta table, and seeds the two rows a fresh
// install has never written: ssl (defaulted by the caller, since it depends on
// how the process was started) and the secret key. A missing row is not an
// error -- every value has a meaningful zero.
func loadMetaState(tx *sql.Tx, sslDefault string) error {
	for _, m := range []struct {
		key   string
		store func(string)
	}{
		{"setup", metaSetup.Store},
		{"name", metaName.Store},
		{"domain", metaDomain.Store},
		{"unconfirmedDomain", metaUnconfirmedDomain.Store},
		{"unconfirmedDomainProblem", metaUnconfirmedDomainProblem.Store},
		{"configFileEnabled", func(v string) { metaConfigFileEnabled.Store(v == "true") }},
	} {
		value, err := getMetaValue(tx, m.key)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loadMetaState.getMetaValue %s: %w", m.key, err)
		}

		m.store(value)
	}

	ssl, err := getMetaValue(tx, "ssl")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("loadMetaState.getMetaValue ssl: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		metaSSL.Store(sslDefault)

		if err := updateMetaValue(tx, "ssl", sslDefault); err != nil {
			return fmt.Errorf("loadMetaState.updateMetaValue ssl: %w", err)
		}
	} else {
		metaSSL.Store(ssl)
	}

	// The key signs everything the instance hands out, so it is generated once
	// and never regenerated -- rewriting it would invalidate every live token.
	if _, err := getMetaValue(tx, "secretKey"); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("loadMetaState.getMetaValue secretKey: %w", err)
		}

		keyBytes := make([]byte, 32)
		if _, err := rand.Read(keyBytes); err != nil {
			return fmt.Errorf("loadMetaState.Read: %w", err)
		}

		err = updateMetaValue(tx, "secretKey", base64.StdEncoding.EncodeToString(keyBytes))
		if err != nil {
			return fmt.Errorf("loadMetaState.updateMetaValue secretKey: %w", err)
		}
	}

	return nil
}
