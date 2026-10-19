package mpris

import (
	"encoding/xml"
	"math"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

// noTrack is the MPRIS object path meaning "no track".
const noTrack = "/org/mpris/MediaPlayer2/TrackList/NoTrack"

// variantString extracts a string from a variant, tolerating object paths.
func variantString(v dbus.Variant) (string, bool) {
	switch x := v.Value().(type) {
	case string:
		return x, true
	case dbus.ObjectPath:
		return string(x), true
	}
	return "", false
}

// variantStrings extracts a string list, tolerating players that send a
// single string where the spec requires a list.
func variantStrings(v dbus.Variant) []string {
	var out []string
	switch x := v.Value().(type) {
	case []string:
		out = x
	case string:
		out = []string{x}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case []dbus.Variant:
		for _, e := range x {
			if s, ok := variantString(e); ok {
				out = append(out, s)
			}
		}
	}
	clean := make([]string, 0, len(out))
	for _, s := range out {
		if s = strings.TrimSpace(s); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// variantInt64 extracts an integer, tolerating the various numeric types
// players use for mpris:length and Position.
func variantInt64(v dbus.Variant) (int64, bool) {
	switch x := v.Value().(type) {
	case int64:
		return x, true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case int32:
		return int64(x), true
	case uint32:
		return int64(x), true
	case int16:
		return int64(x), true
	case uint16:
		return int64(x), true
	case byte:
		return int64(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int64(x), true
	}
	return 0, false
}

func variantFloat(v dbus.Variant) (float64, bool) {
	switch x := v.Value().(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	case int64, int32, uint32, uint64:
		i, ok := variantInt64(v)
		return float64(i), ok
	}
	return 0, false
}

func variantBool(v dbus.Variant) (bool, bool) {
	b, ok := v.Value().(bool)
	return b, ok
}

// trackFromMetadata maps an MPRIS metadata dictionary to the session model.
// Missing or malformed entries stay unknown (nil).
func trackFromMetadata(md map[string]dbus.Variant) model.Track {
	var t model.Track
	if v, ok := md["xesam:title"]; ok {
		if s, ok := variantString(v); ok {
			t.Title = model.StrPtr(strings.TrimSpace(s))
		}
	}
	if v, ok := md["xesam:artist"]; ok {
		t.Artists = variantStrings(v)
	}
	if v, ok := md["xesam:album"]; ok {
		if s, ok := variantString(v); ok {
			t.Album = model.StrPtr(strings.TrimSpace(s))
		}
	}
	if v, ok := md["xesam:albumArtist"]; ok {
		t.AlbumArtists = variantStrings(v)
	}
	if v, ok := md["mpris:artUrl"]; ok {
		if s, ok := variantString(v); ok {
			if clean, ok := redact.URL(s); ok {
				t.ArtworkURL = model.StrPtr(clean)
			}
		}
	}
	if v, ok := md["xesam:url"]; ok {
		if s, ok := variantString(v); ok {
			if clean, ok := redact.URL(s); ok {
				t.URL = model.StrPtr(clean)
			}
		}
	}
	if v, ok := md["mpris:length"]; ok {
		if us, ok := variantInt64(v); ok && us > 0 {
			t.DurationMS = model.Ptr(us / 1000)
		}
	}
	if v, ok := md["mpris:trackid"]; ok {
		if s, ok := variantString(v); ok && s != noTrack {
			t.TrackID = model.StrPtr(s)
		}
	}
	return t
}

func mapPlaybackStatus(s string) model.PlaybackState {
	switch s {
	case "Playing":
		return model.StatePlaying
	case "Paused":
		return model.StatePaused
	case "Stopped":
		return model.StateStopped
	}
	return model.StateUnknown
}

func mapLoopStatus(s string) (string, bool) {
	switch s {
	case "None":
		return "none", true
	case "Track":
		return "track", true
	case "Playlist":
		return "playlist", true
	}
	return "", false
}

func xmlUnmarshal(s string, v any) error {
	return xml.Unmarshal([]byte(s), v)
}
