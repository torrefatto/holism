// Package registry defines the drone-registry port: the mind's in-memory
// view of connected drones — labels, capability names, readiness, lease
// state, and the opaque send-queue handle of the live stream. Adapters live
// in subpackages and import this package — never the other way around.
//
// The registry is why the mind must be a single instance: this state lives
// only in the mind process.
package registry

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors returned by Registry implementations. Callers should match
// them with errors.Is.
var (
	// ErrNotFound is returned when the drone id is not registered.
	ErrNotFound = errors.New("registry: not found")
	// ErrInvalid is returned for contract violations the registry enforces
	// itself (e.g. a drone without an id).
	ErrInvalid = errors.New("registry: invalid")
)

// Drone is a registry entry: the mind's view of one connected drone.
type Drone struct {
	// ID is the drone's identity (drone_id from Register); the registry key.
	ID string
	// Labels are the drone's free-form labels (e.g. "gpu", "region:eu").
	Labels []string
	// Capabilities holds the declared capability names only (e.g.
	// "tool:kubectl"); matching is on names, per the contract.
	Capabilities []string
	// Load is the last heartbeat's in-flight task count.
	Load int32
	// Ready reports whether the drone is ready to receive tasks. Register
	// marks the drone ready; lease expiry removes the entry.
	Ready bool
	// LeaseExpiresAt is when the entry expires unless renewed by a
	// heartbeat. The registry stamps it from its Clock.
	LeaseExpiresAt time.Time
	// Queue is the opaque send-queue handle of the drone's live stream. The
	// registry stores it as-is; wiring it to the gRPC stream is the
	// server's job, not the registry's.
	Queue any
}

// Filter selects drones in Registry.Ready. The drone must carry ALL of
// Labels (empty means any labels). RequiredCapabilities mirrors
// TaskSpec.required_capabilities: the drone's capability names must be a
// superset of it (empty means any drone).
type Filter struct {
	Labels               []string
	RequiredCapabilities []string
}

// Registry is the drone-registry port. Implementations must be safe for
// concurrent use and must not alias the Drone values they store or return.
type Registry interface {
	// Register stores d as a READY drone with a fresh lease, replacing any
	// previous entry with the same id, including its queue handle.
	// Returns ErrInvalid if d is nil or has an empty id.
	Register(ctx context.Context, d *Drone) (*Drone, error)
	// Heartbeat renews the lease of drone id and records its load.
	// Returns ErrNotFound for an unknown id, ErrInvalid for an empty id.
	Heartbeat(ctx context.Context, id string, load int32) (*Drone, error)
	// Unregister removes drone id. Returns ErrNotFound for an unknown id,
	// ErrInvalid for an empty id.
	Unregister(ctx context.Context, id string) error
	// ExpireLeases removes every entry whose lease has expired (per the
	// registry's Clock) and returns the expired ids, sorted. The server
	// calls it on a ticker; Registry.Ready also enforces lease validity at
	// query time.
	ExpireLeases(ctx context.Context) ([]string, error)
	// Ready returns the drones that are registered, ready, lease-valid,
	// carry ALL of filter.Labels, and whose capability names are a superset
	// of filter.RequiredCapabilities, sorted by id.
	Ready(ctx context.Context, filter Filter) ([]*Drone, error)
}
