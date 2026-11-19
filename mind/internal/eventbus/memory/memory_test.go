package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
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
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ev := &holismv1.Event{Type: holismv1.Event_TYPE_TASK_QUEUED, ProjectId: "p1", TaskId: "t1", Summary: "task t1 queued"}
	if err := b.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := recv(t, ch)
	if got.Id != "evt-001" {
		t.Fatalf("stamped id = %q, want evt-001", got.Id)
	}
	if got.TimestampUnix != clk.Now().Unix() {
		t.Fatalf("stamped timestamp = %d, want %d", got.TimestampUnix, clk.Now().Unix())
	}
	if got.Summary != "task t1 queued" || got.Type != holismv1.Event_TYPE_TASK_QUEUED {
		t.Fatalf("event = %+v", got)
	}
	// The publisher's message must not be aliased/mutated.
	if ev.Id != "" || ev.TimestampUnix != 0 {
		t.Fatalf("Publish mutated the caller's event: %+v", ev)
	}
}

func TestProjectFilter(t *testing.T) {
	b := New(nil, nil)
	ctx := context.Background()

	p1, err := b.Subscribe(ctx, eventbus.Filter{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	all, err := b.Subscribe(ctx, eventbus.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	_ = b.Publish(ctx, &holismv1.Event{ProjectId: "p2", Summary: "other project"})
	_ = b.Publish(ctx, &holismv1.Event{ProjectId: "p1", Summary: "in p1"})

	if got := recv(t, p1); got.ProjectId != "p1" {
		t.Fatalf("p1 subscriber got %+v", got)
	}
	expectNone(t, p1)
	if got := recv(t, all); got.ProjectId != "p2" {
		t.Fatalf("all subscriber first got %+v", got)
	}
	if got := recv(t, all); got.ProjectId != "p1" {
		t.Fatalf("all subscriber second got %+v", got)
	}
}

func TestPublishIsNonBlocking(t *testing.T) {
	b := New(nil, nil)
	ctx := context.Background()

	ch, err := b.Subscribe(ctx, eventbus.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
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
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	// Exactly the buffer's worth (at most) was delivered.
	n := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("channel closed while subscriber active")
			}
			n++
			if n > subscriberBuffer {
				t.Fatalf("received %d events, more than buffer %d", n, subscriberBuffer)
			}
		default:
			if n == 0 {
				t.Fatal("no events delivered at all")
			}
			return
		}
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	b := New(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := b.Subscribe(ctx, eventbus.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}

	// Publishing after unsubscribe must not panic (no send on closed channel).
	if err := b.Publish(context.Background(), &holismv1.Event{Summary: "late"}); err != nil {
		t.Fatalf("Publish after unsubscribe: %v", err)
	}
}

func TestPublishNil(t *testing.T) {
	b := New(nil, nil)
	if err := b.Publish(context.Background(), nil); !errors.Is(err, eventbus.ErrInvalid) {
		t.Fatalf("Publish(nil) = %v, want ErrInvalid", err)
	}
}
