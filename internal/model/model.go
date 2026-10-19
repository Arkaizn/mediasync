// Package model defines the provider-independent media session schema.
//
// The schema is versioned (SchemaVersion). Additive changes (new optional
// fields, new enum values that consumers must tolerate) do not bump the
// version; removals or semantic changes do.
//
// Unknown information is represented explicitly: pointer fields are nil
// (JSON null) and enum fields use their "unknown" value. Providers must never
// invent metadata to fill gaps.
package model

import (
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the version of the session schema exposed over IPC.
const SchemaVersion = 1

// Origin says where the playback is happening relative to this machine.
type Origin string

const (
	// OriginLocal is a player running in the current user's session on this machine.
	OriginLocal Origin = "local"
	// OriginRemote is a player on another device, observed through a server.
	OriginRemote Origin = "remote"
)

// PlaybackState is the playback status of a session.
type PlaybackState string

const (
	StatePlaying PlaybackState = "playing"
	StatePaused  PlaybackState = "paused"
	StateStopped PlaybackState = "stopped"
	StateUnknown PlaybackState = "unknown"
)

// ConnectionStatus describes how fresh the information about a session is.
type ConnectionStatus string

const (
	// StatusConnected means the provider is receiving current information.
	StatusConnected ConnectionStatus = "connected"
	// StatusStale means the session still exists but has not reported recently.
	StatusStale ConnectionStatus = "stale"
	// StatusDisconnected means the provider lost contact with the session's source
	// (e.g. the server is unreachable). Data is the last known state.
	StatusDisconnected ConnectionStatus = "disconnected"
)

// Capability is a control operation a session supports.
type Capability string

const (
	CapPlay      Capability = "play"
	CapPause     Capability = "pause"
	CapPlayPause Capability = "play_pause"
	CapStop      Capability = "stop"
	CapNext      Capability = "next"
	CapPrevious  Capability = "previous"
	CapSeek      Capability = "seek"
	CapVolume    Capability = "volume"
)

// AllCapabilities lists every capability in a stable order.
var AllCapabilities = []Capability{CapPlay, CapPause, CapPlayPause, CapStop, CapNext, CapPrevious, CapSeek, CapVolume}

// Player describes the application or device that is playing.
type Player struct {
	// Name is a human readable name for display (identity or device name).
	Name string `json:"name"`
	// Identity is the application name as reported by the provider, if any.
	Identity *string `json:"identity"`
	// Client is the client application name for remote sessions (e.g. "Finamp").
	Client *string `json:"client"`
	// ClientVersion is the version of the client application, if known.
	ClientVersion *string `json:"client_version"`
	// Device is the device name for remote sessions.
	Device *string `json:"device"`
	// DeviceID is the provider's device identifier.
	DeviceID *string `json:"device_id"`
	// DesktopEntry is the freedesktop desktop entry name (MPRIS), if any.
	DesktopEntry *string `json:"desktop_entry"`
	// BusName is the D-Bus well-known name (MPRIS only).
	BusName *string `json:"bus_name"`
	// Address is the network address the remote client connects from, as
	// reported by the server (used for same-host detection).
	Address *string `json:"address"`
}

// Server describes the media server a remote session belongs to.
type Server struct {
	// Instance is the configured provider instance name (e.g. "home").
	Instance string  `json:"instance"`
	ID       *string `json:"id"`
	Name     *string `json:"name"`
	Version  *string `json:"version"`
	UserID   *string `json:"user_id"`
	UserName *string `json:"user_name"`
}

// Track is the currently loaded media item.
type Track struct {
	Title        *string  `json:"title"`
	Artists      []string `json:"artists"`
	Album        *string  `json:"album"`
	AlbumArtists []string `json:"album_artists"`
	// ArtworkURL never contains credentials.
	ArtworkURL *string `json:"artwork_url"`
	DurationMS *int64  `json:"duration_ms"`
	// TrackID is the provider's identifier for the item (MPRIS trackid or Jellyfin item id).
	TrackID *string `json:"track_id"`
	// URL is the media location if the provider reports one (MPRIS xesam:url).
	// It is omitted when it would contain credentials.
	URL *string `json:"url"`
	// MediaType is e.g. "Audio" or "Video" when known.
	MediaType *string `json:"media_type"`
}

// Playback is the playback state with a position sample.
//
// PositionMS was sampled at PositionAt. When State is playing, consumers can
// extrapolate: position = PositionMS + (now - PositionAt) * Rate.
// The service does not emit events for pure position extrapolation.
type Playback struct {
	State      PlaybackState `json:"state"`
	PositionMS *int64        `json:"position_ms"`
	PositionAt *time.Time    `json:"position_at"`
	Rate       *float64      `json:"rate"`
	// Volume is 0.0–1.0 (may exceed 1.0 for players that allow amplification).
	Volume  *float64 `json:"volume"`
	Muted   *bool    `json:"muted"`
	Shuffle *bool    `json:"shuffle"`
	// Repeat is "none", "track", "playlist" when known.
	Repeat *string `json:"repeat"`
}

// ErrorInfo describes the last error affecting a session.
type ErrorInfo struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Session is a single media playback experience.
type Session struct {
	// ID is stable across metadata updates and, where the provider allows,
	// across player restarts. Treat it as opaque.
	ID string `json:"id"`
	// Provider is the provider kind ("mpris", "jellyfin").
	Provider string `json:"provider"`
	// ProviderInstance is the provider instance that owns the session
	// (e.g. "mpris" or "jellyfin:home").
	ProviderInstance string `json:"provider_instance"`
	// ProviderSessionID is the provider's own identifier (bus name, Jellyfin session id).
	ProviderSessionID string `json:"provider_session_id"`
	Origin            Origin `json:"origin"`
	// Controllable is false for sessions that can be observed but not controlled.
	Controllable bool             `json:"controllable"`
	Capabilities []Capability     `json:"capabilities"`
	Player       Player           `json:"player"`
	Server       *Server          `json:"server"`
	Track        Track            `json:"track"`
	Playback     Playback         `json:"playback"`
	Status       ConnectionStatus `json:"status"`
	// LastSeen is when the provider last confirmed the session exists.
	LastSeen time.Time `json:"last_seen"`
	// UpdatedAt is when any field other than LastSeen last changed.
	UpdatedAt time.Time  `json:"updated_at"`
	Error     *ErrorInfo `json:"error"`

	// Fields computed by the engine (not set by providers).

	// DuplicateOf is set when this session was recognised as the same playback
	// as another (primary) session. Hidden from default listings.
	DuplicateOf *string `json:"duplicate_of,omitempty"`
	// Linked lists sessions recognised as duplicates of this one.
	Linked []string `json:"linked,omitempty"`
	// Hidden is set when the session is excluded from default listings:
	// "duplicate", "mode", "view_only" or "stale".
	Hidden string `json:"hidden,omitempty"`
	// Kind summarises origin, controllability and status:
	// "local", "remote_controllable", "remote_view_only", "stale", "disconnected".
	Kind SessionKind `json:"kind"`
}

// SessionKind is a coarse classification derived from other fields.
type SessionKind string

const (
	KindLocal              SessionKind = "local"
	KindRemoteControllable SessionKind = "remote_controllable"
	KindRemoteViewOnly     SessionKind = "remote_view_only"
	KindStale              SessionKind = "stale"
	KindDisconnected       SessionKind = "disconnected"
)

// ComputeKind derives the session kind.
func (s *Session) ComputeKind() SessionKind {
	switch {
	case s.Status == StatusDisconnected:
		return KindDisconnected
	case s.Status == StatusStale:
		return KindStale
	case s.Origin == OriginLocal:
		return KindLocal
	case s.Controllable:
		return KindRemoteControllable
	default:
		return KindRemoteViewOnly
	}
}

// Has reports whether the session advertises capability c.
func (s *Session) Has(c Capability) bool {
	return slices.Contains(s.Capabilities, c)
}

// Clone returns a deep copy of the session.
func (s Session) Clone() Session {
	c := s
	c.Capabilities = slices.Clone(s.Capabilities)
	c.Track.Artists = slices.Clone(s.Track.Artists)
	c.Track.AlbumArtists = slices.Clone(s.Track.AlbumArtists)
	c.Linked = slices.Clone(s.Linked)
	if s.Server != nil {
		srv := *s.Server
		c.Server = &srv
	}
	if s.Error != nil {
		e := *s.Error
		c.Error = &e
	}
	// Remaining pointer fields point to immutable values by convention:
	// providers always allocate fresh values and never mutate after publishing.
	return c
}

// CurrentPosition extrapolates the playback position at time now.
// It returns nil when the position is unknown.
func (p Playback) CurrentPosition(now time.Time) *int64 {
	if p.PositionMS == nil {
		return nil
	}
	pos := *p.PositionMS
	if p.State == StatePlaying && p.PositionAt != nil {
		rate := 1.0
		if p.Rate != nil {
			rate = *p.Rate
		}
		pos += int64(float64(now.Sub(*p.PositionAt).Milliseconds()) * rate)
	}
	if pos < 0 {
		pos = 0
	}
	return &pos
}

// SortCapabilities sorts capabilities into the canonical order and removes duplicates.
func SortCapabilities(caps []Capability) []Capability {
	out := make([]Capability, 0, len(caps))
	for _, c := range AllCapabilities {
		if slices.Contains(caps, c) {
			out = append(out, c)
		}
	}
	return out
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// StrPtr returns nil for empty strings, otherwise a pointer to s.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// MPRISPrefix is the well-known bus name prefix of MPRIS players.
const MPRISPrefix = "org.mpris.MediaPlayer2."

// Matches reports whether a user-supplied player pattern refers to this
// player. Matching is case-insensitive against:
//   - the MPRIS bus name suffix ("feishin" matches org.mpris.MediaPlayer2.feishin
//     and org.mpris.MediaPlayer2.feishin.instance123),
//   - the identity, desktop entry, client name and device name (exact match).
func (p Player) Matches(pattern string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}
	if p.BusName != nil {
		key := strings.ToLower(strings.TrimPrefix(*p.BusName, MPRISPrefix))
		if key == pattern || strings.HasPrefix(key, pattern+".") {
			return true
		}
	}
	for _, v := range []*string{p.Identity, p.DesktopEntry, p.Client, p.Device} {
		if v != nil && strings.ToLower(*v) == pattern {
			return true
		}
	}
	return strings.ToLower(p.Name) == pattern
}
