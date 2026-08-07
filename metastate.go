package main

import "sync/atomic"

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
