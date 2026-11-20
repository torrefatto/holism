// Package memory provides the in-memory registry adapter: a mutex-protected
// map keyed by drone id, with lease expiry driven by a Clock. It never
// aliases the Drone values it stores or returns (slices are copied).
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/torrefatto/holism/mind/internal/clock"
	"github.com/torrefatto/holism/mind/internal/registry"
)

// DefaultLeaseTTL is the lease duration used when New gets a non-positive
// ttl: a drone that misses heartbeats for this long expires.
const DefaultLeaseTTL = 30 * time.Second

// Registry is an in-memory registry.Registry.
type Registry struct {
	mu       sync.RWMutex
	drones   map[string]*registry.Drone
	clk      clock.Clock
	leaseTTL time.Duration
}

// New returns an in-memory Registry. clk drives lease expiry (nil falls back
// to the real clock); leaseTTL is the lease granted on register and renewed
// on heartbeat (non-positive falls back to DefaultLeaseTTL).
func New(clk clock.Clock, leaseTTL time.Duration) *Registry {
	if clk == nil {
		clk = clock.RealClock{}
	}
	if leaseTTL <= 0 {
		leaseTTL = DefaultLeaseTTL
	}
	return &Registry{
		drones:   make(map[string]*registry.Drone),
		clk:      clk,
		leaseTTL: leaseTTL,
	}
}

func copyDrone(d *registry.Drone) *registry.Drone {
	if d == nil {
		return nil
	}
	out := *d
	out.Labels = append([]string(nil), d.Labels...)
	out.Capabilities = append([]string(nil), d.Capabilities...)
	return &out
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// hasAll reports whether list contains every want element.
func hasAll(list, want []string) bool {
	for _, w := range want {
		if !hasString(list, w) {
			return false
		}
	}
	return true
}

// leaseValid reports whether the entry's lease still holds at now.
func leaseValid(d *registry.Drone, now time.Time) bool {
	return now.Before(d.LeaseExpiresAt)
}

// Register implements registry.Registry.
func (r *Registry) Register(ctx context.Context, d *registry.Drone) (*registry.Drone, error) {
	if d == nil || d.ID == "" {
		return nil, fmt.Errorf("%w: drone id is required", registry.ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := copyDrone(d)
	stored.Ready = true
	stored.LeaseExpiresAt = r.clk.Now().Add(r.leaseTTL)
	r.drones[stored.ID] = stored
	return copyDrone(stored), nil
}

// Heartbeat implements registry.Registry.
func (r *Registry) Heartbeat(ctx context.Context, id string, load int32) (*registry.Drone, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: drone id is required", registry.ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.drones[id]
	if !ok {
		return nil, fmt.Errorf("%w: drone %q", registry.ErrNotFound, id)
	}
	d.Load = load
	d.LeaseExpiresAt = r.clk.Now().Add(r.leaseTTL)
	return copyDrone(d), nil
}

// Unregister implements registry.Registry.
func (r *Registry) Unregister(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: drone id is required", registry.ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drones[id]; !ok {
		return fmt.Errorf("%w: drone %q", registry.ErrNotFound, id)
	}
	delete(r.drones, id)
	return nil
}

// ExpireLeases implements registry.Registry.
func (r *Registry) ExpireLeases(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clk.Now()
	var expired []string
	for id, d := range r.drones {
		if !leaseValid(d, now) {
			delete(r.drones, id)
			expired = append(expired, id)
		}
	}
	sort.Strings(expired)
	return expired, nil
}

// Ready implements registry.Registry.
func (r *Registry) Ready(ctx context.Context, filter registry.Filter) ([]*registry.Drone, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.clk.Now()
	var out []*registry.Drone
	for _, d := range r.drones {
		if !d.Ready || !leaseValid(d, now) {
			continue
		}
		if !hasAll(d.Labels, filter.Labels) {
			continue
		}
		if !hasAll(d.Capabilities, filter.RequiredCapabilities) {
			continue
		}
		out = append(out, copyDrone(d))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
