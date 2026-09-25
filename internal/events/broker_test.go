package events

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestPublishDelivers(t *testing.T) {
	b := NewBroker()
	ch, err := b.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Event{Kind: KindRun, Namespace: "ns", Stack: "s", RunID: 7}
	b.Publish(want)
	if got := <-ch; got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := NewBroker()
	ch, err := b.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range subscriberQueueDepth + 1 {
		b.Publish(Event{Kind: KindStackSet})
	}
	n := 0
	for range ch { // terminates: the channel is closed on drop
		n++
	}
	if n != subscriberQueueDepth {
		t.Fatalf("drained %d, want %d", n, subscriberQueueDepth)
	}
	if b.Len() != 0 {
		t.Fatalf("Len = %d after drop", b.Len())
	}
}

func TestSubscriberCap(t *testing.T) {
	b := NewBroker()
	for range subscribersMax {
		if _, err := b.Subscribe(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Subscribe(t.Context()); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("err = %v, want ErrTooManySubscribers", err)
	}
}

func TestCancelClosesAndUnregisters(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel still open")
	}
	if b.Len() != 0 {
		t.Fatal("subscriber not removed")
	}
}

func TestPublishAfterCancelDoesNotPanic(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := b.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-ch
	b.Publish(Event{Kind: KindStackSet})
}

func TestEventName(t *testing.T) {
	tests := []struct {
		name string
		give Event
		want string
	}{
		{name: "stack set", give: Event{Kind: KindStackSet}, want: "stacks"},
		{name: "run", give: Event{Kind: KindRun, RunID: 42}, want: "run-42"},
		{
			name: "stack",
			give: Event{Kind: KindStack, Namespace: "ns", Stack: "s"},
			want: StackEventName("ns", "s"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.give.Name(); got != tt.want {
				t.Fatalf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStackEventNameDistinct(t *testing.T) {
	a, c := StackEventName("a", "b-c"), StackEventName("a-b", "c")
	if a == c {
		t.Fatal("collision")
	}
	if !regexp.MustCompile(`^stack-[0-9a-f]{16}$`).MatchString(a) {
		t.Fatalf("bad name %q", a)
	}
}
