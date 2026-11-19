// Package clock defines the trivial Clock and IDGen ports of the mind, plus
// their real (production) implementations. Tests substitute fakes to keep
// ids and timestamps deterministic.
package clock

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Clock abstracts time. All mind components that need "now" take a Clock.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock, backed by time.Now.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// IDGen generates unique, human-readable ids with a prefix, e.g. "task-0f3a…".
type IDGen interface {
	NewID(prefix string) string
}

// RealIDGen generates ids from 8 random bytes (16 hex chars).
type RealIDGen struct{}

// NewID returns "<prefix>-<16 hex chars>", or the bare hex string when prefix
// is empty. It panics only if crypto/rand fails, which is unrecoverable.
func (RealIDGen) NewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	if prefix == "" {
		return hex.EncodeToString(b[:])
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
