// Package eventbus defines the EventBus port: the mind publishes lifecycle
// events here, and subscribers (SubscribeEvents RPC, external notification
// adapters such as a Slack connector, …) consume them. Adapters live in
// subpackages and import this package — never the other way around.
package eventbus

import (
	"context"
	"errors"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
)

// ErrInvalid is returned when publishing a nil event. Callers should match
// sentinel errors with errors.Is.
var ErrInvalid = errors.New("eventbus: invalid event")

// Filter selects which events a subscriber receives. ProjectID empty means
// events from all projects.
type Filter struct {
	ProjectID string
}

// Bus is the EventBus port. Implementations must be safe for concurrent use.
type Bus interface {
	// Publish delivers ev to every current subscriber whose filter matches.
	// It must not block on slow subscribers. Adapters may stamp the event
	// id/timestamp when they are empty.
	Publish(ctx context.Context, ev *holismv1.Event) error
	// Subscribe returns a channel that receives matching events until ctx
	// is canceled, at which point the subscriber is removed and the
	// channel is closed.
	Subscribe(ctx context.Context, filter Filter) (<-chan *holismv1.Event, error)
}
