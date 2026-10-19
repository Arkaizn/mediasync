package providertest

import (
	"context"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
)

// Fake is an in-memory provider for engine and IPC tests.
type Fake struct {
	instance string
	kind     string
	origin   model.Origin

	mu       sync.Mutex
	sink     provider.Sink
	sessions map[string]model.Session
	calls    []Call
	ready    chan struct{}
	// Behaviour knobs.
	CommandErr   error
	CommandDelay time.Duration
	// ApplyEffects makes commands change session state like a real player.
	ApplyEffects bool
	// EffectDelay delays the visible effect after the command returns.
	EffectDelay time.Duration
	refreshes   int
}

// Call records a command.
type Call struct {
	SessionID string
	Command   model.Command
}

// NewFake creates a fake provider.
func NewFake(instance, kind string, origin model.Origin) *Fake {
	return &Fake{instance: instance, kind: kind, origin: origin, sessions: map[string]model.Session{}, ready: make(chan struct{}), ApplyEffects: true}
}

func (f *Fake) Instance() string     { return f.instance }
func (f *Fake) Kind() string         { return f.kind }
func (f *Fake) Origin() model.Origin { return f.origin }

// Run publishes the current sessions and blocks.
func (f *Fake) Run(ctx context.Context, sink provider.Sink) error {
	f.mu.Lock()
	f.sink = sink
	for _, s := range f.sessions {
		sink.Upsert(s.Clone())
	}
	select {
	case <-f.ready:
	default:
		close(f.ready)
	}
	f.mu.Unlock()
	sink.SetStatus(model.ProviderStatus{State: model.ProviderRunning})
	<-ctx.Done()
	f.mu.Lock()
	f.sink = nil
	f.mu.Unlock()
	return nil
}

// WaitReady blocks until Run has started.
func (f *Fake) WaitReady() { <-f.ready }

// Set adds or replaces a session.
func (f *Fake) Set(s model.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.Provider = f.kind
	if s.Playback.State == "" {
		s.Playback.State = model.StateUnknown
	}
	f.sessions[s.ID] = s
	if f.sink != nil {
		f.sink.Upsert(s.Clone())
	}
}

// Delete removes a session.
func (f *Fake) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, id)
	if f.sink != nil {
		f.sink.Remove(id)
	}
}

// Calls returns recorded commands.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Refreshes returns how often Refresh was called.
func (f *Fake) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

// Refresh implements provider.Refresher.
func (f *Fake) Refresh() {
	f.mu.Lock()
	f.refreshes++
	f.mu.Unlock()
}

// Command implements provider.Provider.
func (f *Fake) Command(ctx context.Context, id string, cmd model.Command) error {
	f.mu.Lock()
	f.calls = append(f.calls, Call{id, cmd})
	s, ok := f.sessions[id]
	errv, delay := f.CommandErr, f.CommandDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if errv != nil {
		return errv
	}
	if !ok {
		return model.Errorf(model.ErrSessionNotFound, "no such session")
	}
	if !f.ApplyEffects {
		return nil
	}
	apply := func() {
		now := time.Now()
		switch cmd.Name {
		case model.CmdPlay:
			s.Playback.State = model.StatePlaying
		case model.CmdPause:
			s.Playback.State = model.StatePaused
		case model.CmdPlayPause:
			if s.Playback.State == model.StatePlaying {
				s.Playback.State = model.StatePaused
			} else {
				s.Playback.State = model.StatePlaying
			}
		case model.CmdStop:
			s.Playback.State = model.StateStopped
		case model.CmdNext, model.CmdPrevious:
			t := "Other track"
			if s.Track.Title != nil {
				t = *s.Track.Title + " (" + string(cmd.Name) + ")"
			}
			s.Track.Title = &t
			s.Playback.PositionMS, s.Playback.PositionAt = model.Ptr[int64](0), &now
		case model.CmdSeek:
			p := int64(0)
			if cmd.PositionMS != nil {
				p = *cmd.PositionMS
			} else if cur := s.Playback.CurrentPosition(now); cur != nil {
				p = *cur + *cmd.OffsetMS
			}
			s.Playback.PositionMS, s.Playback.PositionAt = &p, &now
		case model.CmdSetVolume:
			s.Playback.Volume = model.Ptr(*cmd.Volume)
		}
		f.Set(s)
	}
	if f.EffectDelay > 0 {
		go func() { time.Sleep(f.EffectDelay); apply() }()
	} else {
		apply()
	}
	return nil
}

// Session builds a test session.
func Session(id string, origin model.Origin, title string, state model.PlaybackState, caps ...model.Capability) model.Session {
	now := time.Now()
	s := model.Session{
		ID: id, Origin: origin, Controllable: len(caps) > 0, Capabilities: caps,
		Player:   model.Player{Name: id},
		Playback: model.Playback{State: state, PositionMS: model.Ptr[int64](10_000), PositionAt: &now},
		Status:   model.StatusConnected,
	}
	if title != "" {
		s.Track.Title = &title
	}
	return s
}

// AllCaps is every capability.
var AllCaps = model.AllCapabilities
