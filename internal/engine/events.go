package engine

import (
	"reflect"

	"github.com/arkaizn/mediasync/internal/model"
)

// EventType identifies an engine event.
type EventType string

const (
	EventSessionAdded     EventType = "session.added"
	EventSessionUpdated   EventType = "session.updated"
	EventSessionRemoved   EventType = "session.removed"
	EventSelectionChanged EventType = "selection.changed"
	EventProviderStatus   EventType = "provider.status"
	EventProviderRemoved  EventType = "provider.removed"
	EventSettingsChanged  EventType = "settings.changed"
)

// Event is a change notification. Revision increases by one per event.
type Event struct {
	Revision         uint64                `json:"revision"`
	Type             EventType             `json:"type"`
	SessionID        string                `json:"session_id,omitempty"`
	Session          *model.Session        `json:"session,omitempty"`
	Reason           string                `json:"reason,omitempty"`
	Provider         *model.ProviderStatus `json:"provider,omitempty"`
	ProviderInstance string                `json:"provider_instance,omitempty"`
	Settings         *Settings             `json:"settings,omitempty"`
}

// CloseReason says why a subscription ended.
type CloseReason string

const (
	CloseUnsubscribed CloseReason = "unsubscribed"
	// CloseOverflow means the subscriber did not keep up; it must take a new
	// snapshot.
	CloseOverflow CloseReason = "overflow"
	CloseShutdown CloseReason = "shutdown"
)

// Subscription delivers events to one consumer.
type Subscription struct {
	e      *Engine
	ch     chan Event
	closed bool
	reason CloseReason
}

// Events returns the event channel. It is closed when the subscription ends.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Reason returns why the subscription was closed (valid after Events is closed).
func (s *Subscription) Reason() CloseReason {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	return s.reason
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	s.closeLocked(CloseUnsubscribed)
}

func (s *Subscription) closeLocked(r CloseReason) {
	if s.closed {
		return
	}
	s.closed = true
	s.reason = r
	close(s.ch)
	delete(s.e.subs, s)
}

// Subscribe atomically returns a full snapshot and a subscription that
// receives every event after the snapshot's revision.
func (e *Engine) Subscribe(f Filter) (Snapshot, *Subscription) {
	e.mu.Lock()
	defer e.mu.Unlock()
	sub := &Subscription{e: e, ch: make(chan Event, e.opts.SubscriberBuffer)}
	e.subs[sub] = struct{}{}
	return e.snapshotLocked(f), sub
}

// emitLocked assigns a revision and delivers ev to all subscribers without
// blocking. Subscribers whose queue is full are closed with CloseOverflow.
func (e *Engine) emitLocked(ev Event) {
	e.revision++
	ev.Revision = e.revision
	for s := range e.subs {
		select {
		case s.ch <- ev:
		default:
			e.log.Warn("subscriber too slow; dropping subscription")
			s.closeLocked(CloseOverflow)
		}
	}
}

func sessionsEqual(a, b *model.Session) bool {
	return reflect.DeepEqual(a, b)
}
