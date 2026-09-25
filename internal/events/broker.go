// Package events is a bounded in-process pub/sub for store change notifications.
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
)

const (
	// One browser tab per viewer and a small team: 256 concurrent streams is far above
	// expected use and caps memory at 256 * 32 events.
	subscribersMax = 256
	// Events are tiny triggers; 32 absorbs a burst from a full re-list of about 30 stacks.
	// On overflow the subscriber is dropped, not blocked, so a slow client never stalls
	// the store's write path.
	subscriberQueueDepth = 32
)

// Kind says what changed. The zero value is invalid.
type Kind int

// Event kinds.
const (
	KindInvalid  Kind = iota // zero value, never published
	KindStack         // one stack row changed
	KindStackSet      // a stack appeared, reappeared or was deleted
	KindRun           // one run changed
)

// Event is a change notification. It carries identity only; readers fetch current state.
type Event struct {
	Kind      Kind
	Namespace string
	Stack     string // stack name
	RunID     int64
}

// Name is the SSE event name: "stack-<16 hex>", "stacks" or "run-<id>".
func (e Event) Name() string {
	switch e.Kind {
	case KindStack:
		return StackEventName(e.Namespace, e.Stack)
	case KindStackSet:
		return "stacks"
	case KindRun:
		return "run-" + strconv.FormatInt(e.RunID, 10)
	default:
		panic("invariant violated: event kind " + strconv.Itoa(int(e.Kind)))
	}
}

// StackEventName hashes namespace and name so the result is safe inside hx-trigger.
// "/" cannot appear in Kubernetes names, so the joined input is unambiguous.
func StackEventName(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return "stack-" + hex.EncodeToString(sum[:8])
}

// ErrTooManySubscribers is returned by Subscribe when the subscriber cap is reached.
var ErrTooManySubscribers = errors.New("too many subscribers")

type subscriber struct {
	ch     chan Event
	closed bool
}

// Broker fans events out to subscribers without ever blocking the publisher.
type Broker struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

// NewBroker returns an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[*subscriber]struct{}, subscribersMax)}
}

// Publish never blocks. A subscriber whose queue is full is removed and its channel
// closed; the browser reconnects and receives "resync".
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.ch <- e:
		default:
			b.removeLocked(s)
		}
	}
}

// Subscribe returns a channel that is closed when ctx ends or the subscriber is dropped.
func (b *Broker) Subscribe(ctx context.Context) (<-chan Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) >= subscribersMax {
		return nil, ErrTooManySubscribers
	}
	s := &subscriber{ch: make(chan Event, subscriberQueueDepth)}
	b.subs[s] = struct{}{}
	// AfterFunc runs once when ctx ends; no goroutine outlives the subscription.
	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.removeLocked(s)
	})
	return s.ch, nil
}

// Len returns the number of active subscribers.
func (b *Broker) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

func (b *Broker) removeLocked(s *subscriber) {
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	delete(b.subs, s)
}
