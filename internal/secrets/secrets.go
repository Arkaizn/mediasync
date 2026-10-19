// Package secrets stores credentials (Jellyfin access tokens).
//
// Backends:
//   - secret-service: the freedesktop Secret Service D-Bus API (GNOME Keyring,
//     KWallet ≥ 5.97, KeePassXC, …). Preferred.
//   - file: a JSON file readable only by the user (mode 0600 in a 0700
//     directory). Tokens are stored unencrypted at rest; it must be enabled
//     explicitly and is documented as a fallback for systems without a
//     Secret Service.
//   - memory: for tests.
package secrets

import (
	"context"
	"errors"
	"sync"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

// ErrNotFound is returned when no secret exists for a key.
var ErrNotFound = errors.New("secret not found")

// Attributes identify a secret. All values are non-secret.
type Attributes struct {
	// Kind is e.g. "jellyfin-token".
	Kind string
	// Instance is the provider instance name.
	Instance string
	// Server is the server URL.
	Server string
	// User is the user name (informational).
	User string
}

func (a Attributes) key() string { return a.Kind + "\x00" + a.Instance + "\x00" + a.Server }

// Store is a credential store.
type Store interface {
	// Get returns ErrNotFound if no secret is stored.
	Get(ctx context.Context, a Attributes) (redact.Secret, error)
	Set(ctx context.Context, a Attributes, label string, s redact.Secret) error
	// Delete succeeds if the secret does not exist.
	Delete(ctx context.Context, a Attributes) error
	// Backend names the backend ("secret-service", "file", "memory").
	Backend() string
	// Check verifies that the backend is usable.
	Check(ctx context.Context) error
}

// Memory is an in-memory store for tests.
type Memory struct {
	mu   sync.Mutex
	m    map[string]redact.Secret
	Fail error // if set, every operation fails with this error
}

// NewMemory returns an empty memory store.
func NewMemory() *Memory { return &Memory{m: map[string]redact.Secret{}} }

func (s *Memory) Backend() string { return "memory" }

func (s *Memory) Check(context.Context) error { return s.Fail }

func (s *Memory) Get(_ context.Context, a Attributes) (redact.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return "", s.Fail
	}
	v, ok := s.m[a.key()]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *Memory) Set(_ context.Context, a Attributes, _ string, v redact.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	s.m[a.key()] = v
	return nil
}

func (s *Memory) Delete(_ context.Context, a Attributes) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail != nil {
		return s.Fail
	}
	delete(s.m, a.key())
	return nil
}

// storeError wraps backend failures in a structured, secret-free error.
func storeError(backend string, err error, op string) error {
	if errors.Is(err, ErrNotFound) {
		return err
	}
	return model.Wrap(model.ErrCredentialStore, err, backend+": cannot "+op+" credential: "+redact.String(err.Error()))
}
