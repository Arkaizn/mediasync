// Package provider defines the interface between the engine and media
// providers, and a registry of provider factories.
//
// A provider owns a set of sessions. It reports them to the engine through a
// Sink and executes commands for them. Providers never see sessions of other
// providers and the engine never interprets provider-specific data.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/arkaizn/mediasync/internal/model"
)

// Sink receives updates from a provider. Implementations are safe for
// concurrent use. Sessions passed to Upsert are owned by the sink afterwards.
type Sink interface {
	// Upsert adds or fully replaces a session owned by the provider.
	Upsert(s model.Session)
	// Remove deletes a session owned by the provider.
	Remove(sessionID string)
	// SetStatus updates the provider's health status.
	SetStatus(st model.ProviderStatus)
}

// Provider is a source of media sessions.
type Provider interface {
	// Instance returns the unique provider instance name (e.g. "mpris", "jellyfin:home").
	Instance() string
	// Kind returns the provider kind (e.g. "mpris", "jellyfin").
	Kind() string
	// Origin returns the origin of the sessions this provider produces.
	Origin() model.Origin
	// Run starts the provider and blocks until ctx is cancelled or a fatal
	// error occurs. Before returning, the provider must Remove all of its
	// sessions (or the engine will remove them).
	Run(ctx context.Context, sink Sink) error
	// Command executes cmd on the session. It returns once the provider has
	// confirmed the command was accepted, or with a *model.Error.
	// The engine has already checked that the session advertises the
	// required capability; providers must still reject what they cannot do.
	Command(ctx context.Context, sessionID string, cmd model.Command) error
}

// Refresher is optionally implemented by providers that poll: Refresh
// requests an immediate state update (used to verify command effects).
type Refresher interface {
	Refresh()
}

// Factory creates a provider instance.
type Factory func(deps Deps) (Provider, error)

// Deps are dependencies handed to provider factories.
type Deps struct {
	Logger *slog.Logger
	// Config is the provider-specific configuration value.
	Config any
}

// Registry maps provider kinds to factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

// Register adds a factory. It panics on duplicate registration, which is a
// programming error.
func (r *Registry) Register(kind string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[kind]; ok {
		panic(fmt.Sprintf("provider kind %q registered twice", kind))
	}
	r.factories[kind] = f
}

// New creates a provider of the given kind.
func (r *Registry) New(kind string, deps Deps) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[kind]
	r.mu.RUnlock()
	if !ok {
		return nil, model.Errorf(model.ErrProviderNotFound, "unknown provider kind %q", kind)
	}
	return f(deps)
}

// Kinds returns registered kinds in sorted order.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for k := range r.factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
