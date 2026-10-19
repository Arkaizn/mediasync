package engine

import (
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/arkaizn/mediasync/internal/model"
)

// HostInfo describes the local machine for same-host detection.
type HostInfo struct {
	Hostname string
	// Addresses are the IP addresses assigned to local interfaces.
	Addresses []string
	// IgnoreAddresses disables address-based same-host detection (for
	// servers that see every client through one local address, e.g. behind
	// Docker's userland proxy).
	IgnoreAddresses bool
}

// DetectHost gathers hostname and interface addresses.
func DetectHost() HostInfo {
	h := HostInfo{}
	h.Hostname, _ = os.Hostname()
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				h.Addresses = append(h.Addresses, ipn.IP.String())
			}
		}
	}
	return h
}

func (h HostInfo) isLocalAddress(addr string) bool {
	host := addr
	if hh, _, err := net.SplitHostPort(addr); err == nil {
		host = hh
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, a := range h.Addresses {
		if la := net.ParseIP(a); la != nil && la.Equal(ip) {
			return true
		}
	}
	return false
}

// remoteOnThisHost reports evidence that a remote-origin session is
// actually a client running on this machine.
func (h HostInfo) remoteOnThisHost(r *model.Session) bool {
	if !h.IgnoreAddresses && r.Player.Address != nil && h.isLocalAddress(*r.Player.Address) {
		return true
	}
	if r.Player.Device != nil && h.Hostname != "" {
		dev := strings.ToLower(*r.Player.Device)
		host := strings.ToLower(h.Hostname)
		if dev == host || strings.SplitN(host, ".", 2)[0] == dev {
			return true
		}
	}
	return false
}

// normalize lower-cases and strips punctuation/whitespace for comparisons.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tracksAgree reports whether two sessions are playing the same track,
// requiring a known, equal title and no contradicting artist/album/duration.
func tracksAgree(a, b *model.Session) bool {
	if a.Track.Title == nil || b.Track.Title == nil {
		return false
	}
	if normalize(*a.Track.Title) == "" || normalize(*a.Track.Title) != normalize(*b.Track.Title) {
		return false
	}
	if len(a.Track.Artists) > 0 && len(b.Track.Artists) > 0 {
		overlap := false
		for _, x := range a.Track.Artists {
			for _, y := range b.Track.Artists {
				if normalize(x) == normalize(y) {
					overlap = true
				}
			}
		}
		if !overlap {
			return false
		}
	}
	if a.Track.Album != nil && b.Track.Album != nil && normalize(*a.Track.Album) != normalize(*b.Track.Album) {
		return false
	}
	if a.Track.DurationMS != nil && b.Track.DurationMS != nil &&
		math.Abs(float64(*a.Track.DurationMS-*b.Track.DurationMS)) > 3000 {
		return false
	}
	return true
}

// itemIDReferenced reports whether the remote item id appears in the local
// player's track id, URL or artwork URL (e.g. a Jellyfin client exposing
// "/Items/<id>/..." URLs or the item id as MPRIS trackid).
func itemIDReferenced(local, remote *model.Session) bool {
	if remote.Track.TrackID == nil {
		return false
	}
	id := strings.ToLower(strings.ReplaceAll(*remote.Track.TrackID, "-", ""))
	if len(id) < 16 { // too short to be a meaningful identifier
		return false
	}
	for _, f := range []*string{local.Track.TrackID, local.Track.URL, local.Track.ArtworkURL} {
		if f != nil && strings.Contains(strings.ToLower(strings.ReplaceAll(*f, "-", "")), id) {
			return true
		}
	}
	return false
}

func linkMatches(l DedupLink, local, remote *model.Session) bool {
	if !local.Player.Matches(l.LocalPlayer) {
		return false
	}
	eq := func(want string, got *string) bool {
		return want == "" || (got != nil && strings.EqualFold(want, *got))
	}
	return eq(l.RemoteClient, remote.Player.Client) &&
		eq(l.RemoteDevice, remote.Player.Device) &&
		eq(l.RemoteDeviceID, remote.Player.DeviceID)
}

// DuplicateReason explains why two sessions were merged.
type DuplicateReason string

const (
	DupLink     DuplicateReason = "configured_link"
	DupItemID   DuplicateReason = "item_id_same_host"
	DupSameHost DuplicateReason = "same_host_same_track"
)

// isDuplicate decides whether remote session r represents the same playback
// as local session l. The rules are deliberately conservative: matching
// metadata alone is never enough, there must also be evidence that the two
// sessions are the same software on the same machine.
func isDuplicate(settings Settings, host HostInfo, l, r *model.Session) (DuplicateReason, bool) {
	if l.Origin != model.OriginLocal || r.Origin != model.OriginRemote {
		return "", false
	}
	for _, link := range settings.DedupLinks {
		if linkMatches(link, l, r) && (tracksAgree(l, r) || itemIDReferenced(l, r)) {
			return DupLink, true
		}
	}
	onHost := host.remoteOnThisHost(r)
	if onHost && itemIDReferenced(l, r) {
		return DupItemID, true
	}
	if onHost && tracksAgree(l, r) {
		return DupSameHost, true
	}
	return "", false
}

// applyDedup annotates sessions in place: remote duplicates get DuplicateOf,
// local primaries get Linked. Local sessions are never modified otherwise
// and remote data never replaces local data.
func applyDedup(settings Settings, host HostInfo, sessions []*model.Session) map[string]DuplicateReason {
	reasons := map[string]DuplicateReason{}
	for _, s := range sessions {
		s.DuplicateOf = nil
		s.Linked = nil
	}
	if !settings.Dedup {
		return reasons
	}
	var locals, remotes []*model.Session
	for _, s := range sessions {
		if s.Origin == model.OriginLocal {
			locals = append(locals, s)
		} else {
			remotes = append(remotes, s)
		}
	}
	// Deterministic order: iterate by ID.
	sort.Slice(locals, func(i, j int) bool { return locals[i].ID < locals[j].ID })
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].ID < remotes[j].ID })
	for _, r := range remotes {
		for _, l := range locals {
			if reason, ok := isDuplicate(settings, host, l, r); ok {
				id := l.ID
				r.DuplicateOf = &id
				l.Linked = append(l.Linked, r.ID)
				reasons[r.ID] = reason
				break
			}
		}
	}
	return reasons
}
