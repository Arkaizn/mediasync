package engine

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
)

// LyricsFallback looks up lyrics by track metadata (e.g. LRCLIB). It is only
// configured when the user enabled it.
type LyricsFallback interface {
	Lookup(ctx context.Context, t model.Track) (lines []model.LyricLine, synced, found bool, err error)
}

// linkedCaps are capabilities a local player can borrow from the remote
// session it is linked to (the same playback seen through the server).
var linkedCaps = []model.Capability{model.CapQueue, model.CapLyrics, model.CapQueueJump, model.CapPlayItems, model.CapPlayItemsShuffle}

func keepCaps(caps, keep []model.Capability) []model.Capability {
	out := []model.Capability{}
	for _, c := range caps {
		if slices.Contains(keep, c) {
			out = append(out, c)
		}
	}
	return out
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// mediaServerItemID extracts a media-server item id from a local player's
// track id. Feishin, for example, uses ".../track/<jellyfin item id>".
func mediaServerItemID(trackID *string) string {
	if trackID == nil {
		return ""
	}
	seg := *trackID
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	seg = strings.ToLower(strings.ReplaceAll(seg, "-", ""))
	if hex32.MatchString(seg) {
		return seg
	}
	return ""
}

func (e *Engine) hasItemLyricsSourceLocked() bool {
	for _, pe := range e.providers {
		if _, ok := pe.p.(provider.ItemLyricsSource); ok && pe.status.State == model.ProviderRunning {
			return true
		}
	}
	return false
}

// augmentMediaLocked adds capabilities that come from outside a session's
// own provider: a local player borrows queue/lyrics (and, if it is
// controllable, queue commands) from its linked remote session; lyrics can
// also come from a media server by item id or from the lyrics fallback.
func (e *Engine) augmentMediaLocked(all []*model.Session) {
	byID := make(map[string]*model.Session, len(all))
	for _, s := range all {
		byID[s.ID] = s
	}
	itemLyrics := e.hasItemLyricsSourceLocked()
	for _, s := range all {
		caps := slices.Clone(s.Capabilities)
		if s.Origin == model.OriginLocal {
			for _, rid := range s.Linked {
				r := byID[rid]
				if r == nil || r.Status == model.StatusDisconnected {
					continue
				}
				for _, c := range linkedCaps {
					if r.Has(c) && !slices.Contains(caps, c) && (s.Controllable || slices.Contains(model.ReadOnlyCapabilities, c)) {
						caps = append(caps, c)
					}
				}
				if s.QueueRevision == "" && r.Has(model.CapQueue) {
					s.QueueRevision = "linked:" + r.QueueRevision
				}
			}
			if !slices.Contains(caps, model.CapLyrics) && itemLyrics && mediaServerItemID(s.Track.TrackID) != "" {
				caps = append(caps, model.CapLyrics)
			}
		}
		if !slices.Contains(caps, model.CapLyrics) && e.opts.LyricsFallback != nil &&
			s.Track.Title != nil && len(s.Track.Artists) > 0 && s.Status != model.StatusDisconnected {
			caps = append(caps, model.CapLyrics)
		}
		s.Capabilities = model.SortCapabilities(caps)
	}
}

// viaLinked returns the linked remote session that serves capability c for
// a local session whose own provider does not support it.
func (e *Engine) viaLinked(sessionID string, c model.Capability) (string, provider.Provider, model.Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent := e.sessions[sessionID]
	v, ok := e.view[sessionID]
	if !ok || ent == nil || ent.raw.Has(c) {
		return "", nil, model.Session{}, false
	}
	for _, rid := range v.Linked {
		r, ok := e.view[rid]
		rent := e.sessions[rid]
		if !ok || rent == nil || !r.Has(c) || r.Status == model.StatusDisconnected {
			continue
		}
		if pe := e.providers[rent.instance]; pe != nil {
			return rid, pe.p, r.Clone(), true
		}
	}
	return "", nil, model.Session{}, false
}

func (e *Engine) providerOfLocked(id string) provider.Provider {
	if ent := e.sessions[id]; ent != nil {
		if pe := e.providers[ent.instance]; pe != nil {
			return pe.p
		}
	}
	return nil
}

// Queue returns the queue of a session. A local player linked to a remote
// session that reports a queue gets that queue.
func (e *Engine) Queue(ctx context.Context, sessionID string) (model.Queue, error) {
	e.mu.Lock()
	v, ok := e.view[sessionID]
	if !ok {
		e.mu.Unlock()
		return model.Queue{}, model.Errorf(model.ErrSessionNotFound, "session %q does not exist", sessionID)
	}
	target, prov := "", provider.Provider(nil)
	if ent := e.sessions[sessionID]; ent != nil && ent.raw.Has(model.CapQueue) {
		target, prov = sessionID, e.providerOfLocked(sessionID)
	} else {
		for _, rid := range v.Linked {
			if r, ok := e.view[rid]; ok && r.Has(model.CapQueue) {
				target, prov = rid, e.providerOfLocked(rid)
				break
			}
		}
	}
	e.mu.Unlock()
	qs, ok := prov.(provider.QueueSource)
	if target == "" || !ok {
		return model.UnavailableQueue(sessionID), nil
	}
	q, err := qs.Queue(ctx, target)
	if err != nil {
		if model.CodeOf(err) == model.ErrSessionNotFound && target != sessionID {
			return model.UnavailableQueue(sessionID), nil
		}
		return model.Queue{}, err
	}
	q.SessionID = sessionID
	if q.Items == nil {
		q.Items = []model.QueueItem{}
	}
	return q, nil
}

// Lyrics returns lyrics for a session's current track. Sources, in order:
// the session's own provider (e.g. lyrics published by the player over
// MPRIS, or the media server for remote sessions), the linked remote
// session, a media server by item id (local players playing server items),
// and the optional fallback. Text is never invented: "none" means a source
// answered that it has no lyrics, "unavailable" that none could answer.
func (e *Engine) Lyrics(ctx context.Context, sessionID string) (model.Lyrics, error) {
	e.mu.Lock()
	v, ok := e.view[sessionID]
	if !ok {
		e.mu.Unlock()
		return model.Lyrics{}, model.Errorf(model.ErrSessionNotFound, "session %q does not exist", sessionID)
	}
	v = v.Clone()
	type src struct {
		id   string
		prov provider.LyricsSource
	}
	var sources []src
	if ls, ok := e.providerOfLocked(sessionID).(provider.LyricsSource); ok {
		sources = append(sources, src{sessionID, ls})
	}
	for _, rid := range v.Linked {
		if ls, ok := e.providerOfLocked(rid).(provider.LyricsSource); ok {
			sources = append(sources, src{rid, ls})
		}
	}
	var itemSources []provider.ItemLyricsSource
	itemID := ""
	if v.Origin == model.OriginLocal {
		itemID = mediaServerItemID(v.Track.TrackID)
		for _, pe := range e.providers {
			if is, ok := pe.p.(provider.ItemLyricsSource); ok && pe.status.State == model.ProviderRunning {
				itemSources = append(itemSources, is)
			}
		}
	}
	fallback := e.opts.LyricsFallback
	e.mu.Unlock()

	res := model.Lyrics{SessionID: sessionID, TrackID: v.Track.TrackID, Status: model.LyricsUnavailable, Lines: []model.LyricLine{}}
	sawNone := false
	accept := func(l model.Lyrics) bool {
		switch l.Status {
		case model.LyricsOK:
			res.Status, res.Synced, res.Lines, res.Source = l.Status, l.Synced, l.Lines, l.Source
			return true
		case model.LyricsNone:
			sawNone = true
		}
		return false
	}
	for _, s := range sources {
		l, err := s.prov.Lyrics(ctx, s.id)
		if err != nil {
			e.log.Debug("lyrics source failed", "session", s.id, "err", err)
			continue
		}
		if accept(l) {
			return res, nil
		}
	}
	if itemID != "" {
		for _, is := range itemSources {
			l, known, err := is.ItemLyrics(ctx, itemID)
			if err != nil || !known {
				continue
			}
			if accept(l) {
				return res, nil
			}
			break
		}
	}
	if fallback != nil && v.Track.Title != nil && len(v.Track.Artists) > 0 {
		lines, synced, found, err := fallback.Lookup(ctx, v.Track)
		switch {
		case err != nil:
			e.log.Debug("lyrics fallback failed", "err", err)
		case found:
			res.Status, res.Synced, res.Lines, res.Source = model.LyricsOK, synced, lines, model.Ptr(model.LyricsSourceLRCLIB)
			return res, nil
		default:
			sawNone = true
		}
	}
	if sawNone {
		res.Status = model.LyricsNone
	}
	return res, nil
}
