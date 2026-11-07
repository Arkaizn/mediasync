package jellyfin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/lyrics"
	"github.com/arkaizn/mediasync/internal/model"
)

// Bounds for the per-provider caches.
const (
	itemCacheSize   = 4096
	lyricsCacheSize = 512
	lyricsCacheTTL  = 30 * time.Minute
	// maxExpandTracks bounds playlist expansion. Only the part that fits
	// into one request URL is sent (see maxPlayRequestTarget).
	maxExpandTracks = 10000
	// maxPlayRetries is how often a play command rejected as too long
	// (a proxy with a smaller limit) is retried with half the items.
	maxPlayRetries = 3
)

type cachedLyrics struct {
	lyrics model.Lyrics
	known  bool
	at     time.Time
}

// mediaCache caches item metadata and lyrics. Lyrics are fetched once per
// item (not per position update) and re-fetched after lyricsCacheTTL.
type mediaCache struct {
	mu     sync.Mutex
	items  map[string]BaseItem
	lyrics map[string]cachedLyrics
}

func (c *mediaCache) getItems(ids []string) (map[string]BaseItem, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	found := map[string]BaseItem{}
	var missing []string
	for _, id := range ids {
		if it, ok := c.items[id]; ok {
			found[id] = it
		} else if !slices.Contains(missing, id) {
			missing = append(missing, id)
		}
	}
	return found, missing
}

func (c *mediaCache) putItems(items []BaseItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil || len(c.items)+len(items) > itemCacheSize {
		c.items = map[string]BaseItem{}
	}
	for _, it := range items {
		if it.ID != "" {
			c.items[it.ID] = it
		}
	}
}

// queueRevision fingerprints the queue state reported by the server.
func queueRevision(si SessionInfo) string {
	h := sha256.New()
	if si.NowPlayingItem != nil {
		h.Write([]byte(si.NowPlayingItem.ID))
	}
	h.Write([]byte{0})
	h.Write([]byte(si.PlaylistItemID))
	for _, e := range si.NowPlayingQueue {
		h.Write([]byte{0})
		h.Write([]byte(e.ID))
		h.Write([]byte{1})
		h.Write([]byte(e.PlaylistItemID))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// queueLayout returns the queue's item ids and the index of the current item.
//
// Clients report the queue differently: Jellyfin Web reports the whole
// playlist and the current entry's PlaylistItemId; Finamp reports only the
// upcoming tracks (its source code labels entries "nextUp"/"queue"). If the
// current item is not identifiable in the reported list, it is put in front.
func queueLayout(si SessionInfo) (ids []string, current int) {
	current = -1
	for i, e := range si.NowPlayingQueue {
		ids = append(ids, e.ID)
		if current < 0 && si.PlaylistItemID != "" && e.PlaylistItemID == si.PlaylistItemID {
			current = i
		}
	}
	np := si.NowPlayingItem
	if current < 0 && np != nil && len(ids) > 0 && strings.EqualFold(ids[0], np.ID) {
		current = 0
	}
	if current < 0 && np != nil {
		ids = append([]string{np.ID}, ids...)
		current = 0
	}
	return ids, current
}

func (p *Provider) userID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.user == nil {
		return ""
	}
	return p.user.ID
}

// ensureUserID returns the user id, asking the server if the provider has
// not validated its token yet (e.g. right after login).
func (p *Provider) ensureUserID(ctx context.Context) (string, error) {
	if id := p.userID(); id != "" {
		return id, nil
	}
	if !p.client.HasToken() {
		return "", model.Errorf(model.ErrNotAuthenticated, "not logged in to Jellyfin server %q", p.cfg.Name)
	}
	u, err := p.client.Me(ctx)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

// itemMeta returns metadata for ids, using known items and the cache, and
// fetching the rest in batches.
func (p *Provider) itemMeta(ctx context.Context, ids []string, known []BaseItem) map[string]BaseItem {
	p.media.putItems(known)
	found, missing := p.media.getItems(ids)
	if len(missing) > 0 {
		if items, err := p.client.Items(ctx, p.userID(), missing); err == nil {
			p.media.putItems(items)
			for _, it := range items {
				found[it.ID] = it
			}
		} else {
			p.log.Debug("cannot fetch queue item details", "err", err)
		}
	}
	return found
}

func (p *Provider) artworkFor(it BaseItem) *string {
	switch {
	case it.ImageTags["Primary"] != "":
		return model.Ptr(p.client.ImageURL(it.ID, it.ImageTags["Primary"]))
	case it.AlbumID != "" && it.AlbumPrimaryImageTag != "":
		return model.Ptr(p.client.ImageURL(it.AlbumID, it.AlbumPrimaryImageTag))
	}
	return nil
}

// Queue implements provider.QueueSource.
func (p *Provider) Queue(ctx context.Context, sessionID string) (model.Queue, error) {
	p.mu.Lock()
	si, ok := p.live[sessionID]
	pub := p.published[sessionID]
	p.mu.Unlock()
	if !ok {
		return model.Queue{}, model.Errorf(model.ErrSessionNotFound, "Jellyfin session %q is not present", sessionID)
	}
	q := model.UnavailableQueue(sessionID)
	if len(si.NowPlayingQueue) == 0 || !pub.Has(model.CapQueue) {
		return q, nil
	}
	ids, current := queueLayout(si)
	known := slices.Clone(si.NowPlayingQueueFullItems)
	if si.NowPlayingItem != nil {
		known = append(known, *si.NowPlayingItem)
	}
	meta := p.itemMeta(ctx, ids, known)
	q.Available = true
	q.Source = model.Ptr("jellyfin")
	if current >= 0 {
		q.CurrentIndex = model.Ptr(current)
	}
	for i, id := range ids {
		qi := model.QueueItem{Index: i, ItemID: id}
		if it, ok := meta[id]; ok {
			qi.Title = model.StrPtr(strings.TrimSpace(it.Name))
			if len(it.Artists) > 0 {
				qi.Artists = slices.Clone(it.Artists)
			}
			qi.Album = model.StrPtr(strings.TrimSpace(it.Album))
			if it.RunTimeTicks != nil && *it.RunTimeTicks > 0 {
				qi.DurationMS = model.Ptr(*it.RunTimeTicks / ticksPerMS)
			}
			qi.ArtworkURL = p.artworkFor(it)
		}
		q.Items = append(q.Items, qi)
	}
	return q, nil
}

// Lyrics implements provider.LyricsSource for the session's current track.
func (p *Provider) Lyrics(ctx context.Context, sessionID string) (model.Lyrics, error) {
	p.mu.Lock()
	si, ok := p.live[sessionID]
	p.mu.Unlock()
	if !ok {
		return model.Lyrics{}, model.Errorf(model.ErrSessionNotFound, "Jellyfin session %q is not present", sessionID)
	}
	if si.NowPlayingItem == nil || si.NowPlayingItem.ID == "" {
		return model.Lyrics{SessionID: sessionID, Status: model.LyricsUnavailable, Lines: []model.LyricLine{}}, nil
	}
	l, _, err := p.ItemLyrics(ctx, si.NowPlayingItem.ID)
	l.SessionID = sessionID
	return l, err
}

// ItemLyrics implements provider.ItemLyricsSource.
func (p *Provider) ItemLyrics(ctx context.Context, itemID string) (model.Lyrics, bool, error) {
	p.media.mu.Lock()
	if c, ok := p.media.lyrics[itemID]; ok && time.Since(c.at) < lyricsCacheTTL {
		p.media.mu.Unlock()
		l := c.lyrics
		l.Lines = slices.Clone(l.Lines)
		return l, c.known, nil
	}
	p.media.mu.Unlock()

	res := model.Lyrics{TrackID: model.Ptr(itemID), Status: model.LyricsUnavailable, Lines: []model.LyricLine{}}
	dto, found, err := p.client.Lyrics(ctx, itemID)
	if err != nil {
		return res, true, err // not cached: transient
	}
	known := true
	if found {
		for _, l := range dto.Lyrics {
			line := model.LyricLine{Text: strings.TrimSpace(l.Text)}
			if l.Start != nil {
				line.StartMS = model.Ptr(max(*l.Start/ticksPerMS, 0))
				res.Synced = true
			}
			res.Lines = append(res.Lines, line)
		}
		res.Lines = lyrics.TrimLines(res.Lines)
		if res.Synced {
			// Unsynced stray lines (e.g. an empty trailer) carry no position.
			res.Lines = slices.DeleteFunc(res.Lines, func(l model.LyricLine) bool { return l.StartMS == nil })
		}
		res.Status = model.LyricsOK
		res.Source = model.Ptr(model.LyricsSourceJellyfin)
		if len(res.Lines) == 0 {
			res.Status = model.LyricsNone
		}
	} else {
		// 404 means "no lyrics" for a known item, and also "unknown item".
		items, err := p.client.Items(ctx, p.userID(), []string{itemID})
		if err != nil {
			return res, true, err
		}
		if len(items) == 0 {
			known = false
		} else {
			res.Status = model.LyricsNone
			res.Source = model.Ptr(model.LyricsSourceJellyfin)
		}
	}
	p.media.mu.Lock()
	if p.media.lyrics == nil || len(p.media.lyrics) >= lyricsCacheSize {
		p.media.lyrics = map[string]cachedLyrics{}
	}
	p.media.lyrics[itemID] = cachedLyrics{lyrics: res, known: known, at: time.Now()}
	p.media.mu.Unlock()
	res.Lines = slices.Clone(res.Lines)
	return res, known, nil
}

// Playlists lists the logged-in user's playlists.
func (p *Provider) Playlists(ctx context.Context) ([]model.Playlist, error) {
	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, err
	}
	items, err := p.client.Playlists(ctx, uid)
	if err != nil {
		return nil, err
	}
	out := make([]model.Playlist, 0, len(items))
	for _, it := range items {
		out = append(out, model.Playlist{
			ID: it.ID, Name: it.Name, ItemCount: it.ChildCount, ArtworkURL: p.artworkFor(it),
			Server: p.cfg.Name, MediaType: model.StrPtr(it.MediaType),
		})
	}
	return out, nil
}

// ServerName returns the configured server name.
func (p *Provider) ServerName() string { return p.cfg.Name }

// expandItems turns the requested ids into the id list for a play command.
// Playlists are expanded here, in playlist order: the server would expand
// them too, but reorders them (verified on 12.2). Albums, artists and tracks
// are passed through. Unknown ids are an error (the server would accept them
// silently and send an empty list to the client).
func (p *Provider) expandItems(ctx context.Context, ids []string) ([]string, error) {
	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, err
	}
	items, err := p.client.Items(ctx, uid, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]BaseItem{}
	for _, it := range items {
		byID[strings.ToLower(it.ID)] = it
	}
	var out []string
	for _, id := range ids {
		it, ok := byID[strings.ToLower(strings.ReplaceAll(id, "-", ""))]
		if !ok {
			it, ok = byID[strings.ToLower(id)]
		}
		if !ok {
			return nil, model.Errorf(model.ErrInvalidArgument, "unknown item id %q", id)
		}
		if strings.EqualFold(it.Type, "Playlist") {
			entries, err := p.client.PlaylistItems(ctx, uid, it.ID)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				out = append(out, e.ID)
			}
			continue
		}
		out = append(out, it.ID)
	}
	if len(out) == 0 {
		return nil, model.Errorf(model.ErrInvalidArgument, "the requested items contain nothing to play")
	}
	if len(out) > maxExpandTracks {
		return nil, model.Errorf(model.ErrInvalidArgument, "too many tracks (%d); at most %d", len(out), maxExpandTracks)
	}
	return out, nil
}

// playWindow returns the part ids[lo:hi] that contains index and fits into
// budget bytes of query-escaped ids joined by separators of sep bytes. The
// items from index onwards come first (what plays next matters most), then
// the ones before it. At least ids[index] is always included.
func playWindow(ids []string, index, budget, sep int) (lo, hi int) {
	if len(ids) == 0 {
		return 0, 0
	}
	index = min(max(index, 0), len(ids)-1)
	cost := func(id string) int { return len(url.QueryEscape(id)) }
	used := cost(ids[index])
	lo, hi = index, index+1
	for hi < len(ids) && used+sep+cost(ids[hi]) <= budget {
		used += sep + cost(ids[hi])
		hi++
	}
	for lo > 0 && used+sep+cost(ids[lo-1]) <= budget {
		used += sep + cost(ids[lo-1])
		lo--
	}
	return lo, hi
}

// sendPlay sends a play command with as many of ids as fit into one request
// URL, around index (the start index with startAt). Jellyfin only takes the
// ids in the query string, so long playlists are cut, not split: clients
// handle follow-up PlayLast commands concurrently and sort them (Finamp
// fetches PlayNext/PlayLast items sorted by IndexNumber), which would scramble
// the order.
func (p *Provider) sendPlay(ctx context.Context, jfSessionID, playCommand string, ids []string, index int, startAt bool) error {
	budget, sep := p.client.PlayIDBudget(jfSessionID, playCommand)
	var tooLong *model.Error // the first rejection as too long
	for attempt := 0; ; attempt++ {
		lo, hi := playWindow(ids, index, budget, sep)
		var start *int
		if startAt {
			start = model.Ptr(index - lo)
		}
		if hi-lo < len(ids) {
			p.log.Info("sending only part of the items: the request URL would be too long",
				"command", playCommand, "sent", hi-lo, "total", len(ids), "from", lo)
		}
		err := p.client.PlayCommand(ctx, jfSessionID, playCommand, ids[lo:hi], start)
		if err == nil {
			return nil
		}
		e := model.AsError(err)
		if e.Data["items"] != nil && tooLong == nil {
			tooLong = e
		}
		if tooLong != nil && e.Data["http_status"] == tooLong.Data["http_status"] {
			// Still rejected with the same status once the URL is short
			// (a 400 then is no longer recognisable as "too long").
			err, e = tooLong, tooLong
		}
		if e.Data["items"] == nil || hi-lo <= 1 || attempt >= maxPlayRetries {
			return err
		}
		// Rejected as too long: a proxy with a smaller limit. Retry with fewer.
		p.log.Info("play command rejected as too long; retrying with fewer items",
			"items", hi-lo, "http_status", e.Data["http_status"])
		budget /= 2
	}
}

func (p *Provider) playItems(ctx context.Context, jfSessionID string, cmd model.Command) error {
	ids, err := p.expandItems(ctx, cmd.ItemIDs)
	if err != nil {
		return err
	}
	pc := map[string]string{model.PlayModeNow: "PlayNow", model.PlayModeNext: "PlayNext", model.PlayModeLast: "PlayLast"}[cmd.PlayMode()]
	return p.sendPlay(ctx, jfSessionID, pc, ids, 0, false)
}

// queueJump plays queue item index. Jellyfin has no "jump" command, so the
// queue is sent again with PlayNow and a start index (clients such as
// Finamp then rebuild their queue from these items).
func (p *Provider) queueJump(ctx context.Context, sessionID, jfSessionID string, index int) error {
	q, err := p.Queue(ctx, sessionID)
	if err != nil {
		return err
	}
	if !q.Available {
		return model.Errorf(model.ErrUnsupported, "the session's queue is not available")
	}
	if index >= len(q.Items) {
		return model.Errorf(model.ErrInvalidArgument, "index %d is outside the queue (%d items)", index, len(q.Items))
	}
	ids := make([]string, 0, len(q.Items))
	for _, it := range q.Items {
		ids = append(ids, it.ItemID)
	}
	return p.sendPlay(ctx, jfSessionID, "PlayNow", ids, index, true)
}
