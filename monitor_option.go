package main

import (
	"slices"
	"strconv"
	"strings"
)

// A monitor's frequency, timeout and attempt count come from fixed sets: the
// forms render them as radio groups and the config file is validated against
// the same lists. Keep these in sync with the markup in monitor_create.go and
// monitor_edit_markup.go.
var (
	// monitorFrequencies are the seconds between checks. The longer intervals
	// exist for endpoints that should not be hit every minute.
	monitorFrequencies = []int{10, 30, 60, 300, 900}
	monitorTimeouts    = []int{5, 10, 15, 30}
	monitorAttempts    = []int{1, 2, 3}
)

func validMonitorFrequency(seconds int) bool {
	return slices.Contains(monitorFrequencies, seconds)
}

func validMonitorTimeout(seconds int) bool {
	return slices.Contains(monitorTimeouts, seconds)
}

func validMonitorAttempts(attempts int) bool {
	return slices.Contains(monitorAttempts, attempts)
}

// joinInts renders an allow-list for an error message.
func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}

	return strings.Join(parts, ", ")
}
