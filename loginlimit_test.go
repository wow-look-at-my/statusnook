package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestLoginRateLimiter(t *testing.T) {
	const key = "203.0.113.7"
	clearLoginFailures(key)

	for i := 0; i < loginMaxFailures; i++ {
		require.False(t, loginRateLimited(key))

		recordLoginFailure(key)
	}

	require.True(t, loginRateLimited(key))

	// A successful login clears the bucket, so a legitimate user who mistyped
	// is not locked out by their own next attempt.
	clearLoginFailures(key)
	assert.False(t, loginRateLimited(key))

}

// The unknown-username branch must spend real bcrypt time, or the difference
// between "no such user" and "wrong password" is measurable.
func TestDummyPasswordHashIsUsable(t *testing.T) {
	err := bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte("anything"))
	require.NotNil(t, err)

	require.NotEqual(t, bcrypt.ErrHashTooShort, err)

}
