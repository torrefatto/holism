package memory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/torrefatto/holism/mind/internal/registry"
)

// fakeClock returns a fixed time, advanced manually by tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

const testTTL = 30 * time.Second

func newTestRegistry() (*Registry, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return New(clk, testTTL), clk
}

func mustRegister(t *testing.T, r *Registry, d *registry.Drone) *registry.Drone {
	t.Helper()
	out, err := r.Register(context.Background(), d)
	require.NoError(t, err)
	return out
}

func readyIDs(t *testing.T, r *Registry, filter registry.Filter) []string {
	t.Helper()
	drones, err := r.Ready(context.Background(), filter)
	require.NoError(t, err)
	var ids []string
	for _, d := range drones {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestRegisterAndReady(t *testing.T) {
	r, clk := newTestRegistry()
	ctx := context.Background()
	queue := make(chan string, 1)

	in := &registry.Drone{
		ID:           "drone-1",
		Labels:       []string{"gpu", "region:eu"},
		Capabilities: []string{"tool:kubectl", "vcs:git-push"},
		Queue:        queue,
	}
	stored := mustRegister(t, r, in)
	assert.Equal(t, "drone-1", stored.ID)
	assert.True(t, stored.Ready, "register marks the drone ready")
	assert.Equal(t, clk.Now().Add(testTTL), stored.LeaseExpiresAt, "register grants a fresh lease")
	assert.Equal(t, []string{"gpu", "region:eu"}, stored.Labels)
	assert.Equal(t, []string{"tool:kubectl", "vcs:git-push"}, stored.Capabilities)
	gotQueue, ok := stored.Queue.(chan string)
	require.True(t, ok, "the opaque queue handle must come back as-is")
	assert.True(t, gotQueue == queue)

	// The registry must not alias the caller's message or its own copy.
	in.Labels[0] = "mutated"
	in.Capabilities[0] = "mutated"
	drones, err := r.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, drones, 1)
	assert.Equal(t, "gpu", drones[0].Labels[0])
	assert.Equal(t, "tool:kubectl", drones[0].Capabilities[0])
	drones[0].Labels[0] = "mutated-again"
	again, err := r.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, "gpu", again[0].Labels[0], "Ready must not alias the stored entry")
}

func TestRegisterInvalid(t *testing.T) {
	r, _ := newTestRegistry()
	_, err := r.Register(context.Background(), nil)
	assert.ErrorIs(t, err, registry.ErrInvalid)
	_, err = r.Register(context.Background(), &registry.Drone{})
	assert.ErrorIs(t, err, registry.ErrInvalid)
}

func TestRegisterReplacesExisting(t *testing.T) {
	r, _ := newTestRegistry()
	ctx := context.Background()

	q1 := make(chan string, 1)
	mustRegister(t, r, &registry.Drone{ID: "d1", Labels: []string{"old"}, Capabilities: []string{"a"}, Queue: q1})
	q2 := make(chan string, 1)
	stored := mustRegister(t, r, &registry.Drone{ID: "d1", Labels: []string{"new"}, Capabilities: []string{"b"}, Load: 7, Queue: q2})
	assert.Equal(t, []string{"new"}, stored.Labels)
	assert.Equal(t, []string{"b"}, stored.Capabilities)
	assert.Equal(t, int32(7), stored.Load)

	drones, err := r.Ready(ctx, registry.Filter{})
	require.NoError(t, err)
	require.Len(t, drones, 1, "re-registering must replace the entry, not duplicate it")
	assert.Equal(t, []string{"new"}, drones[0].Labels)
	gotQueue, ok := drones[0].Queue.(chan string)
	require.True(t, ok)
	assert.True(t, gotQueue == q2, "re-registering must replace the old stream handle")
}

func TestHeartbeatRenewsLease(t *testing.T) {
	r, clk := newTestRegistry()
	ctx := context.Background()
	mustRegister(t, r, &registry.Drone{ID: "d1"})

	clk.advance(testTTL - time.Second)
	hb, err := r.Heartbeat(ctx, "d1", 3)
	require.NoError(t, err)
	assert.Equal(t, int32(3), hb.Load)
	assert.Equal(t, clk.Now().Add(testTTL), hb.LeaseExpiresAt, "heartbeat renews the lease")

	clk.advance(time.Second) // past the original lease expiry
	assert.Equal(t, []string{"d1"}, readyIDs(t, r, registry.Filter{}), "renewed lease keeps the drone ready")
}

func TestHeartbeatErrors(t *testing.T) {
	r, _ := newTestRegistry()
	_, err := r.Heartbeat(context.Background(), "ghost", 0)
	assert.ErrorIs(t, err, registry.ErrNotFound)
	_, err = r.Heartbeat(context.Background(), "", 0)
	assert.ErrorIs(t, err, registry.ErrInvalid)
}

func TestUnregister(t *testing.T) {
	r, _ := newTestRegistry()
	ctx := context.Background()
	mustRegister(t, r, &registry.Drone{ID: "d1"})
	require.NoError(t, r.Unregister(ctx, "d1"))
	assert.Empty(t, readyIDs(t, r, registry.Filter{}))
	assert.ErrorIs(t, r.Unregister(ctx, "d1"), registry.ErrNotFound)
	assert.ErrorIs(t, r.Unregister(ctx, ""), registry.ErrInvalid)
}

func TestExpireLeases(t *testing.T) {
	r, clk := newTestRegistry()
	ctx := context.Background()
	mustRegister(t, r, &registry.Drone{ID: "d1"})
	mustRegister(t, r, &registry.Drone{ID: "d2"})

	clk.advance(testTTL - time.Second)
	expired, err := r.ExpireLeases(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "nothing expires before the lease ends")
	assert.Len(t, readyIDs(t, r, registry.Filter{}), 2)

	clk.advance(time.Second) // both leases now expired
	expired, err = r.ExpireLeases(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"d1", "d2"}, expired, "expired ids are returned sorted")
	assert.Empty(t, readyIDs(t, r, registry.Filter{}), "expired drones are no longer ready")

	expired, err = r.ExpireLeases(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "expiry is one-shot")
}

func TestExpireLeasesOnlyStaleEntries(t *testing.T) {
	r, clk := newTestRegistry()
	ctx := context.Background()
	mustRegister(t, r, &registry.Drone{ID: "stale"})
	clk.advance(testTTL - time.Second)
	mustRegister(t, r, &registry.Drone{ID: "fresh"}) // fresh lease granted later
	clk.advance(time.Second)                         // "stale" expired, "fresh" still valid
	expired, err := r.ExpireLeases(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"stale"}, expired)
	assert.Equal(t, []string{"fresh"}, readyIDs(t, r, registry.Filter{}))
}

func TestReadyChecksLeaseValidity(t *testing.T) {
	r, clk := newTestRegistry()
	mustRegister(t, r, &registry.Drone{ID: "d1"})
	clk.advance(testTTL) // exactly at expiry: the lease no longer holds
	assert.Empty(t, readyIDs(t, r, registry.Filter{}),
		"Ready must filter lease-expired drones even before ExpireLeases runs")
}

func TestReadyCapabilitySuperset(t *testing.T) {
	r, _ := newTestRegistry()
	mustRegister(t, r, &registry.Drone{ID: "d1", Capabilities: []string{"tool:kubectl", "vcs:git-push"}})
	mustRegister(t, r, &registry.Drone{ID: "d2"})

	cases := []struct {
		name     string
		required []string
		want     []string
	}{
		{"empty required means any drone", nil, []string{"d1", "d2"}},
		{"single covered capability", []string{"tool:kubectl"}, []string{"d1"}},
		{"full declared set covered", []string{"tool:kubectl", "vcs:git-push"}, []string{"d1"}},
		{"required superset not covered", []string{"tool:kubectl", "notify:slack"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, readyIDs(t, r, registry.Filter{RequiredCapabilities: tc.required}))
		})
	}
}

func TestReadyLabelMatching(t *testing.T) {
	r, _ := newTestRegistry()
	mustRegister(t, r, &registry.Drone{ID: "gpu-eu", Labels: []string{"gpu", "region:eu"}})
	mustRegister(t, r, &registry.Drone{ID: "cpu", Labels: []string{"cpu"}})

	cases := []struct {
		name   string
		labels []string
		want   []string
	}{
		{"no label filter", nil, []string{"cpu", "gpu-eu"}},
		{"one matching label", []string{"gpu"}, []string{"gpu-eu"}},
		{"all filter labels must be carried", []string{"gpu", "region:eu"}, []string{"gpu-eu"}},
		{"missing label", []string{"gpu", "region:us"}, nil},
		{"unknown label", []string{"arm"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, readyIDs(t, r, registry.Filter{Labels: tc.labels}))
		})
	}
}

func TestReadyCombinedFilter(t *testing.T) {
	r, _ := newTestRegistry()
	mustRegister(t, r, &registry.Drone{ID: "d1", Labels: []string{"gpu"}, Capabilities: []string{"tool:kubectl"}})
	mustRegister(t, r, &registry.Drone{ID: "d2", Labels: []string{"gpu"}, Capabilities: []string{"vcs:git-push"}})
	mustRegister(t, r, &registry.Drone{ID: "d3", Labels: []string{"cpu"}, Capabilities: []string{"tool:kubectl"}})

	got := readyIDs(t, r, registry.Filter{Labels: []string{"gpu"}, RequiredCapabilities: []string{"tool:kubectl"}})
	assert.Equal(t, []string{"d1"}, got, "labels AND capabilities must both match")
}

func TestNewDefaults(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r := New(clk, 0)
	stored := mustRegister(t, r, &registry.Drone{ID: "d1"})
	assert.Equal(t, clk.Now().Add(DefaultLeaseTTL), stored.LeaseExpiresAt)

	r2 := New(nil, 0) // nil clock falls back to the real clock
	s2 := mustRegister(t, r2, &registry.Drone{ID: "d2"})
	assert.True(t, s2.LeaseExpiresAt.After(time.Now()))
}
