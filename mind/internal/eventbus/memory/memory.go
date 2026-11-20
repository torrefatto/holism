// Package memory provides the in-memory eventbus adapter: fan-out to
// subscriber channels with non-blocking publish (a slow subscriber drops
// events instead of blocking the publisher).
package memory

import (
	"context"
	"log/slog"
	"sync"

	"google.golang.org/protobuf/proto"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/clock"
	"github.com/torrefatto/holism/mind/internal/eventbus"
)

// subscriberBuffer is the per-subscriber channel capacity. Once full, the
// subscriber starts dropping events.
const subscriberBuffer = 16

type subscriber struct {
	filter eventbus.Filter
	ch     chan *holismv1.Event
}

// Bus is an in-memory eventbus.Bus.
type Bus struct {
	mu     sync.Mutex
	subs   map[uint64]subscriber
	nextID uint64

	clk clock.Clock
	ids clock.IDGen
}

// New returns an in-memory Bus. clk stamps events and ids generates event
// ids when the publisher left them empty; nil values fall back to the real
// implementations.
func New(clk clock.Clock, ids clock.IDGen) *Bus {
	if clk == nil {
		clk = clock.RealClock{}
	}
	if ids == nil {
		ids = clock.RealIDGen{}
	}
	return &Bus{
		subs: make(map[uint64]subscriber),
		clk:  clk,
		ids:  ids,
	}
}

func matches(filter eventbus.Filter, ev *holismv1.Event) bool {
	return filter.ProjectID == "" || ev.ProjectId == filter.ProjectID
}

// Publish implements eventbus.Bus. The event is deep-copied; empty id and
// timestamp are stamped. Delivery is non-blocking: subscribers whose buffer
// is full drop the event. Sends and channel closes happen under the same
// mutex, so a canceled subscriber's channel is never sent on after close.
func (b *Bus) Publish(ctx context.Context, ev *holismv1.Event) error {
	if ev == nil {
		return eventbus.ErrInvalid
	}
	out := proto.Clone(ev).(*holismv1.Event)
	if out.Id == "" {
		out.Id = b.ids.NewID("evt")
	}
	if out.TimestampUnix == 0 {
		out.TimestampUnix = b.clk.Now().Unix()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if matches(sub.filter, out) {
			select {
			case sub.ch <- out:
			default: // slow subscriber: drop rather than block
				slog.Warn("eventbus: subscriber buffer full, dropping event",
					"event_id", out.Id,
					"event_type", out.Type.String(),
					"project_id", out.ProjectId,
					"task_id", out.TaskId,
				)
			}
		}
	}
	return nil
}

// Subscribe implements eventbus.Bus.
func (b *Bus) Subscribe(ctx context.Context, filter eventbus.Filter) (<-chan *holismv1.Event, error) {
	ch := make(chan *holismv1.Event, subscriberBuffer)
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[id] = subscriber{filter: filter, ch: ch}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub.ch)
		}
	}()
	return ch, nil
}
