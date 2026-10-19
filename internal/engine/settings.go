package engine

import (
	"fmt"
	"strings"

	"github.com/arkaizn/mediasync/internal/model"
)

// Mode selects which sessions are presented.
type Mode string

const (
	ModeCombined   Mode = "combined"
	ModeLocalOnly  Mode = "local_only"
	ModeRemoteOnly Mode = "remote_only"
)

// OriginPreference breaks ties between local and remote sessions.
type OriginPreference string

const (
	PreferLocal  OriginPreference = "local"
	PreferRemote OriginPreference = "remote"
	PreferNone   OriginPreference = "none"
)

// DedupLink is a user-declared relation between a local player and a remote
// client: "this local player is the same software as that Jellyfin client".
// Sessions are only merged when the link matches AND the tracks agree.
type DedupLink struct {
	// LocalPlayer is matched with model.Player.Matches against local sessions.
	LocalPlayer string `json:"local_player" toml:"local_player"`
	// RemoteClient, RemoteDevice and RemoteDeviceID are matched (case-insensitive)
	// against the remote session; empty fields match anything, but at least
	// one must be set.
	RemoteClient   string `json:"remote_client,omitempty" toml:"remote_client"`
	RemoteDevice   string `json:"remote_device,omitempty" toml:"remote_device"`
	RemoteDeviceID string `json:"remote_device_id,omitempty" toml:"remote_device_id"`
}

// Settings are the presentation settings of the engine. They can be updated
// at runtime over IPC.
type Settings struct {
	Mode            Mode             `json:"mode"`
	PreferredPlayer string           `json:"preferred_player"`
	PreferOrigin    OriginPreference `json:"prefer_origin"`
	// ShowViewOnly lists remote sessions that cannot be controlled.
	ShowViewOnly bool `json:"show_view_only"`
	// ShowStale lists stale and disconnected sessions.
	ShowStale bool `json:"show_stale"`
	// Dedup enables duplicate detection.
	Dedup      bool        `json:"dedup"`
	DedupLinks []DedupLink `json:"dedup_links"`
}

// DefaultSettings returns the default settings.
func DefaultSettings() Settings {
	return Settings{
		Mode:         ModeCombined,
		PreferOrigin: PreferLocal,
		ShowViewOnly: true,
		ShowStale:    true,
		Dedup:        true,
	}
}

// Validate checks settings values.
func (s Settings) Validate() error {
	switch s.Mode {
	case ModeCombined, ModeLocalOnly, ModeRemoteOnly:
	default:
		return model.Errorf(model.ErrInvalidArgument, "mode must be one of combined, local_only, remote_only (got %q)", s.Mode)
	}
	switch s.PreferOrigin {
	case PreferLocal, PreferRemote, PreferNone:
	default:
		return model.Errorf(model.ErrInvalidArgument, "prefer_origin must be one of local, remote, none (got %q)", s.PreferOrigin)
	}
	for i, l := range s.DedupLinks {
		if strings.TrimSpace(l.LocalPlayer) == "" {
			return model.Errorf(model.ErrInvalidArgument, "dedup link %d: local_player is required", i)
		}
		if l.RemoteClient == "" && l.RemoteDevice == "" && l.RemoteDeviceID == "" {
			return model.Errorf(model.ErrInvalidArgument, "dedup link %d: one of remote_client, remote_device, remote_device_id is required", i)
		}
	}
	return nil
}

// Filter narrows a session listing. Empty fields match everything.
type Filter struct {
	Providers  []string       `json:"providers,omitempty"`
	Origins    []model.Origin `json:"origins,omitempty"`
	Players    []string       `json:"players,omitempty"`
	Devices    []string       `json:"devices,omitempty"`
	SessionIDs []string       `json:"session_ids,omitempty"`
	// IncludeHidden also returns sessions hidden by mode, settings or deduplication.
	IncludeHidden bool `json:"include_hidden,omitempty"`
}

// Validate checks filter values.
func (f Filter) Validate() error {
	for _, o := range f.Origins {
		if o != model.OriginLocal && o != model.OriginRemote {
			return model.Errorf(model.ErrInvalidArgument, "unknown origin %q", o)
		}
	}
	return nil
}

func (f Filter) match(s *model.Session) bool {
	if len(f.Providers) > 0 && !containsFold(f.Providers, s.Provider) && !containsFold(f.Providers, s.ProviderInstance) {
		return false
	}
	if len(f.Origins) > 0 {
		ok := false
		for _, o := range f.Origins {
			ok = ok || o == s.Origin
		}
		if !ok {
			return false
		}
	}
	if len(f.Players) > 0 {
		ok := false
		for _, p := range f.Players {
			ok = ok || s.Player.Matches(p)
		}
		if !ok {
			return false
		}
	}
	if len(f.Devices) > 0 {
		ok := false
		for _, d := range f.Devices {
			ok = ok || (s.Player.Device != nil && strings.EqualFold(*s.Player.Device, d)) ||
				(s.Player.DeviceID != nil && strings.EqualFold(*s.Player.DeviceID, d))
		}
		if !ok {
			return false
		}
	}
	if len(f.SessionIDs) > 0 {
		ok := false
		for _, id := range f.SessionIDs {
			ok = ok || id == s.ID
		}
		if !ok {
			return false
		}
	}
	return true
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// HiddenReason explains why a session is not in the default listing.
type HiddenReason string

const (
	HiddenNone      HiddenReason = ""
	HiddenDuplicate HiddenReason = "duplicate"
	HiddenMode      HiddenReason = "mode"
	HiddenViewOnly  HiddenReason = "view_only"
	HiddenStale     HiddenReason = "stale"
)

func (s Settings) hiddenReason(sess *model.Session) HiddenReason {
	switch {
	case s.Mode == ModeLocalOnly && sess.Origin != model.OriginLocal,
		s.Mode == ModeRemoteOnly && sess.Origin != model.OriginRemote:
		return HiddenMode
	case sess.DuplicateOf != nil:
		return HiddenDuplicate
	case !s.ShowStale && (sess.Status == model.StatusStale || sess.Status == model.StatusDisconnected):
		return HiddenStale
	case !s.ShowViewOnly && sess.Origin == model.OriginRemote && !sess.Controllable:
		return HiddenViewOnly
	}
	return HiddenNone
}

func (s Settings) String() string {
	return fmt.Sprintf("mode=%s preferred=%q prefer_origin=%s", s.Mode, s.PreferredPlayer, s.PreferOrigin)
}
