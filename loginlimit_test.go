package main

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestLoginRateLimiter(t *testing.T) {
	const key = "203.0.113.7"
	clearLoginFailures(key)

	for i := 0; i < loginMaxFailures; i++ {
		if loginRateLimited(key) {
			t.Fatalf("limited after %d failures, want %d", i, loginMaxFailures)
		}
		recordLoginFailure(key)
	}

	if !loginRateLimited(key) {
		t.Fatalf("not limited after %d failures", loginMaxFailures)
	}

	// A successful login clears the bucket, so a legitimate user who mistyped
	// is not locked out by their own next attempt.
	clearLoginFailures(key)
	if loginRateLimited(key) {
		t.Error("still limited after a successful login")
	}
}

// The unknown-username branch must spend real bcrypt time, or the difference
// between "no such user" and "wrong password" is measurable.
func TestDummyPasswordHashIsUsable(t *testing.T) {
	err := bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte("anything"))
	if err == nil {
		t.Fatal("dummyPasswordHash accepted a password")
	}
	if err == bcrypt.ErrHashTooShort {
		t.Fatalf("dummyPasswordHash is not a valid bcrypt hash: %v", err)
	}
}
