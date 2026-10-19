// Package providertest provides helpers for testing providers.
package providertest

import (
	"sync"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
)

// Sink records provider output.
type Sink struct {
	mu       sync.Mutex
	sessions map[string]model.Session
	status   model.ProviderStatus
	upserts  int
	removes  []string
	changed  chan struct{}
}

// NewSink returns an empty recording sink.
func NewSink() *Sink {
	return &Sink{sessions: map[string]model.Session{}, changed: make(chan struct{}, 1)}
}

func (s *Sink) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Sink) Upsert(sess model.Session) {
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.upserts++
	s.mu.Unlock()
	s.notify()
}

func (s *Sink) Remove(id string) {
	s.mu.Lock()
	if _, ok := s.sessions[id]; ok {
		delete(s.sessions, id)
		s.removes = append(s.removes, id)
	}
	s.mu.Unlock()
	s.notify()
}

func (s *Sink) SetStatus(st model.ProviderStatus) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
	s.notify()
}

// Session returns a session by ID.
func (s *Sink) Session(id string) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[id]
	return v, ok
}

// Sessions returns all sessions.
func (s *Sink) Sessions() map[string]model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]model.Session, len(s.sessions))
	for k, v := range s.sessions {
		out[k] = v
	}
	return out
}

// Status returns the last status.
func (s *Sink) Status() model.ProviderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Removes returns removed IDs in order.
func (s *Sink) Removes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.removes...)
}

// WaitFor polls cond until it returns true or the timeout expires.
func (s *Sink) WaitFor(t testing.TB, timeout time.Duration, what string, cond func(*Sink) bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond(s) {
			return
		}
		select {
		case <-s.changed:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s; sessions=%+v status=%+v", what, s.Sessions(), s.Status())
		}
	}
}

// WaitSession waits until the session exists and cond holds for it.
func (s *Sink) WaitSession(t testing.TB, id string, timeout time.Duration, what string, cond func(model.Session) bool) model.Session {
	t.Helper()
	var got model.Session
	s.WaitFor(t, timeout, what, func(s *Sink) bool {
		v, ok := s.Session(id)
		if ok && (cond == nil || cond(v)) {
			got = v
			return true
		}
		return false
	})
	return got
}
