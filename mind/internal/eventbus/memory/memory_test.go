package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/eventbus"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) Now() time.Time { return f.t }

type fakeIDs struct{ n int }

func (f *fakeIDs) NewID(prefix string) string {
	f.n++
	return fmt.Sprintf("%s-%03d", prefix, f.n)
}

func recv(t *testing.T, ch <-chan *holismv1.Event) *holismv1.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		require.True(t, ok, "channel closed unexpectedly")
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return nil
	}
}

func expectNone(t *testing.T, ch <-chan *holismv1.Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("unexpected event (ok=%v): %+v", ok, ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishSubscribe(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	b := New(clk, &fakeIDs{})
	ctx := context.Background()

	ch, err := b.Subscribe(ctx, eventbus.Filter{})
	require.NoError(t, err)

	ev := &holismv1.Event{Type: holismv1.Event_TYPE_TASK_QUEUED, ProjectId: "p1", TaskId: "t1", Summary: "task t1 queued"}
	require.NoError(t, b.Publish(ctx, ev))
	got := recv(t, ch)
	assert.Equal(t, "evt-001", got.Id, "bus should stamp an id")
	assert.Equal(t, clk.Now().Unix(), got.TimestampUnix, "bus should stamp a timestamp")
	assert.Equal(t, "task t1 queued", got.Summary)
	assert.Equal(t, holismv1.Event_TYPE_TASK_QUEUED, got.Type)
	// The publisher's message must not be aliased/mutated.
	assert.Empty(t, ev.Id)
	assert.Zero(t, ev.TimestampUnix)
}

func TestProjectFilter(t *testing.T) {
	b := New(nil, nil)
	ctx := context.Background()

	p1, err := b.Subscribe(ctx, eventbus.Filter{ProjectID: "p1"})
	require.NoError(t, err)
	all, err := b.Subscribe(ctx, eventbus.Filter{})
	require.NoError(t, err)

	require.NoError(t, b.Publish(ctx, &holismv1.Event{ProjectId: "p2", Summary: "other project"}))
	require.NoError(t, b.Publish(ctx, &holismv1.Event{ProjectId: "p1", Summary: "in p1"}))

	assert.Equal(t, "p1", recv(t, p1).ProjectId)
	expectNone(t, p1)
	assert.Equal(t, "p2", recv(t, all).ProjectId)
	assert.Equal(t, "p1", recv(t, all).ProjectId)
}

func TestPublishIsNonBlocking(t *testing.T) {
	b := New(nil, nil)
	ctx := context.Background()

	ch, err := b.Subscribe(ctx, eventbus.Filter{})
	require.NoError(t, err)
	// Never read from ch: publish many more events than the buffer. Publish
	// must return without blocking; the extra events are dropped.
	done := make(chan error, 1)
	go func() {
		for i := 0; i < subscriberBuffer*10; i++ {
			if err := b.Publish(ctx, &holismv1.Event{Summary: fmt.Sprintf("e%d", i)}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	// At most the buffer's worth was delivered.
	n := 0
	for {
		select {
		case _, ok := <-ch:
			require.True(t, ok, "channel closed while subscriber active")
			n++
			if n > subscriberBuffer {
				t.Fatalf("received %d events, more than buffer %d", n, subscriberBuffer)
			}
		default:
			assert.Greater(t, n, 0, "no events delivered at all")
			return
		}
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := New(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := b.Subscribe(ctx, eventbus.Filter{})
	require.NoError(t, err)
	cancel()

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "expected closed channel after cancel")
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}

	// Publishing after unsubscribe must not panic (no send on closed channel).
	require.NoError(t, b.Publish(context.Background(), &holismv1.Event{Summary: "late"}))
}

func TestPublishNil(t *testing.T) {
	b := New(nil, nil)
	assert.ErrorIs(t, b.Publish(context.Background(), nil), eventbus.ErrInvalid)
}
