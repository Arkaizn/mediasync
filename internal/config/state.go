package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/model"
)

// Overrides are settings changed at runtime over IPC. Nil fields keep the
// value from the configuration file.
type Overrides struct {
	Mode            *string `json:"mode,omitempty"`
	PreferredPlayer *string `json:"preferred_player,omitempty"`
	PreferOrigin    *string `json:"prefer_origin,omitempty"`
	ShowViewOnly    *bool   `json:"show_view_only,omitempty"`
	ShowStale       *bool   `json:"show_stale,omitempty"`
	Dedup           *bool   `json:"dedup,omitempty"`
	LocalEnabled    *bool   `json:"local_enabled,omitempty"`
	RemoteEnabled   *bool   `json:"remote_enabled,omitempty"`
}

// State is persisted service state. It contains no secrets.
type State struct {
	Version   int       `json:"version"`
	DeviceID  string    `json:"device_id"`
	Overrides Overrides `json:"overrides"`
}

// LoadState reads the state file, creating a device id if needed. A corrupt
// state file is reported, not silently replaced.
func LoadState(path string) (State, error) {
	st := State{Version: 1}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return st, model.Wrap(model.ErrInvalidConfig, err, "cannot read state file "+path)
	default:
		if err := json.Unmarshal(data, &st); err != nil {
			return st, model.Errorf(model.ErrInvalidConfig, "state file %s is corrupt; remove it to reset runtime settings", path)
		}
	}
	if st.DeviceID == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return st, model.Wrap(model.ErrInternal, err, "cannot generate device id")
		}
		st.DeviceID = "mediasync-" + hex.EncodeToString(b)
		if err := SaveState(path, st); err != nil {
			return st, err
		}
	}
	return st, nil
}

// SaveState writes the state file atomically (0600 in a 0700 directory).
func SaveState(path string, st State) error {
	st.Version = 1
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot create state directory")
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot encode state")
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot write state file")
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return model.Wrap(model.ErrInternal, err, "cannot write state file")
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return model.Wrap(model.ErrInternal, err, "cannot write state file")
	}
	if err := tmp.Close(); err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot write state file")
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot write state file")
	}
	return nil
}

// Apply overlays overrides on engine settings and enablement flags.
func (o Overrides) Apply(s engine.Settings, localEnabled, remoteEnabled bool) (engine.Settings, bool, bool) {
	if o.Mode != nil {
		s.Mode = engine.Mode(*o.Mode)
	}
	if o.PreferredPlayer != nil {
		s.PreferredPlayer = *o.PreferredPlayer
	}
	if o.PreferOrigin != nil {
		s.PreferOrigin = engine.OriginPreference(*o.PreferOrigin)
	}
	if o.ShowViewOnly != nil {
		s.ShowViewOnly = *o.ShowViewOnly
	}
	if o.ShowStale != nil {
		s.ShowStale = *o.ShowStale
	}
	if o.Dedup != nil {
		s.Dedup = *o.Dedup
	}
	if o.LocalEnabled != nil {
		localEnabled = *o.LocalEnabled
	}
	if o.RemoteEnabled != nil {
		remoteEnabled = *o.RemoteEnabled
	}
	return s, localEnabled, remoteEnabled
}

// Merge returns o with non-nil fields of u applied.
func (o Overrides) Merge(u Overrides) Overrides {
	if u.Mode != nil {
		o.Mode = u.Mode
	}
	if u.PreferredPlayer != nil {
		o.PreferredPlayer = u.PreferredPlayer
	}
	if u.PreferOrigin != nil {
		o.PreferOrigin = u.PreferOrigin
	}
	if u.ShowViewOnly != nil {
		o.ShowViewOnly = u.ShowViewOnly
	}
	if u.ShowStale != nil {
		o.ShowStale = u.ShowStale
	}
	if u.Dedup != nil {
		o.Dedup = u.Dedup
	}
	if u.LocalEnabled != nil {
		o.LocalEnabled = u.LocalEnabled
	}
	if u.RemoteEnabled != nil {
		o.RemoteEnabled = u.RemoteEnabled
	}
	return o
}
