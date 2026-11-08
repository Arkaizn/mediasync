package jellyfin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
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
	var qc *model.QueueContext
	var ctxIDs []string
	ctxCurrent := -1
	if c := p.contexts[sessionID]; c != nil {
		qc = model.Ptr(c.QueueContext)
		ctxIDs = slices.Clone(c.itemIDs)
		if !c.pending {
			ctxCurrent = c.current
		}
	}
	p.mu.Unlock()
	if !ok {
		return model.Queue{}, model.Errorf(model.ErrSessionNotFound, "Jellyfin session %q is not present", sessionID)
	}
	q := model.UnavailableQueue(sessionID)
	q.Context = qc
	if !pub.Has(model.CapQueue) {
		return q, nil
	}
	known := slices.Clone(si.NowPlayingQueueFullItems)
	if si.NowPlayingItem != nil {
		known = append(known, *si.NowPlayingItem)
	}
	if qc != nil && !qc.Shuffled {
		// The whole playlist or album in its order, earlier tracks
		// included; the current position only once the client reported it.
		q.Available, q.Source, q.Items = true, model.Ptr(model.QueueSourceContext), p.queueItems(ctx, ctxIDs, known)
		if ctxCurrent >= 0 {
			q.CurrentIndex = model.Ptr(ctxCurrent)
		}
		return q, nil
	}
	if len(si.NowPlayingQueue) == 0 {
		return q, nil
	}
	ids, current := queueLayout(si)
	q.Available = true
	q.Source = model.Ptr("jellyfin")
	if current >= 0 {
		q.CurrentIndex = model.Ptr(current)
	}
	q.Items = p.queueItems(ctx, ids, known)
	return q, nil
}

// queueItems builds queue entries with item details (unknown details stay nil).
func (p *Provider) queueItems(ctx context.Context, ids []string, known []BaseItem) []model.QueueItem {
	meta := p.itemMeta(ctx, ids, known)
	items := make([]model.QueueItem, 0, len(ids))
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
		items = append(items, qi)
	}
	return items
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

// containerTypes are item types the server expands into their tracks when
// asked to play them (TranslateItemForPlayback).
var containerTypes = map[string]bool{"MusicAlbum": true, "MusicArtist": true, "MusicGenre": true,
	"Folder": true, "CollectionFolder": true, "BoxSet": true}

// expandItems turns the requested ids into the id list for a play command.
// Playlists are expanded here, in playlist order: the server would expand
// them too, but reorders them (verified on 12.2). Albums, artists, genres
// and folders are passed through for the server to expand, or expanded here
// with containers (shuffling needs the whole track list). Unknown ids are an
// error (the server would accept them silently and send an empty list to the
// client).
func (p *Provider) expandItems(ctx context.Context, ids []string, expand func(BaseItem) bool) ([]string, []BaseItem, error) {
	uid, err := p.ensureUserID(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := p.client.Items(ctx, uid, ids)
	if err != nil {
		return nil, nil, err
	}
	var requested []BaseItem
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
			return nil, nil, model.Errorf(model.ErrInvalidArgument, "unknown item id %q", id)
		}
		requested = append(requested, it)
		if strings.EqualFold(it.Type, "Playlist") {
			entries, err := p.client.PlaylistItems(ctx, uid, it.ID)
			if err != nil {
				return nil, nil, err
			}
			for _, e := range entries {
				out = append(out, e.ID)
			}
			continue
		}
		if containerTypes[it.Type] && expand(it) {
			tracks, err := p.client.ContainerTracks(ctx, uid, it, maxExpandTracks+1)
			if err != nil {
				return nil, nil, err
			}
			for _, tr := range tracks {
				out = append(out, tr.ID)
			}
			continue
		}
		out = append(out, it.ID)
	}
	if len(out) == 0 {
		return nil, nil, model.Errorf(model.ErrInvalidArgument, "the requested items contain nothing to play")
	}
	if len(out) > maxExpandTracks {
		return nil, nil, model.Errorf(model.ErrInvalidArgument, "too many tracks (%d); at most %d", len(out), maxExpandTracks)
	}
	return out, requested, nil
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

// shuffleIDs puts ids into random order (Fisher–Yates).
func shuffleIDs(ids []string) {
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
}

func (p *Provider) playItems(ctx context.Context, sessionID string, si SessionInfo, cmd model.Command) error {
	jfSessionID := si.ID
	now := cmd.PlayMode() == model.PlayModeNow
	single := len(cmd.ItemIDs) == 1
	// Shuffling needs the whole track list; a single album played now is
	// expanded too, so that its tracks are known as the queue context.
	ids, requested, err := p.expandItems(ctx, cmd.ItemIDs, func(it BaseItem) bool {
		return cmd.Shuffled() || (now && single && contextType(it.Type) == model.ContextAlbum)
	})
	if err != nil {
		return err
	}
	if cmd.Shuffled() {
		// Shuffle the whole list first, so that a list cut to the URL
		// budget is a random part of it, not the beginning.
		shuffleIDs(ids)
		// A shuffled "play now" should not start with the track that is
		// already playing (it would look as if nothing happened).
		if now && len(ids) > 1 && si.NowPlayingItem != nil && strings.EqualFold(ids[0], si.NowPlayingItem.ID) {
			if j := slices.IndexFunc(ids, func(id string) bool { return !strings.EqualFold(id, ids[0]) }); j > 0 {
				k := j + rand.IntN(len(ids)-j)
				if strings.EqualFold(ids[k], ids[0]) { // a duplicate of the current track
					k = j
				}
				ids[0], ids[k] = ids[k], ids[0]
			}
		}
	}
	pc := map[string]string{model.PlayModeNow: "PlayNow", model.PlayModeNext: "PlayNext", model.PlayModeLast: "PlayLast"}[cmd.PlayMode()]
	if err := p.sendPlay(ctx, jfSessionID, pc, ids, 0, false); err != nil {
		return err // the client's queue is unchanged: so is what we know about it
	}
	// The client's queue changed: what mediasync knew about it (context,
	// jump position) no longer holds; a new context may start.
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.contexts, sessionID)
	delete(p.jumps, sessionID)
	if t := contextType(requested[0].Type); now && single && t != "" {
		p.startContextLocked(sessionID, si, model.QueueContext{Type: t, ID: requested[0].ID,
			Name: strings.TrimSpace(requested[0].Name), Shuffled: cmd.Shuffled()}, ids)
	}
	return nil
}

// queueJump plays queue item index. Jellyfin has no "jump" command:
//   - On a queue mediasync started from a playlist or album (context), the
//     whole list is sent again with PlayNow and the start index, so it stays
//     the client's queue.
//   - A short forward jump (up to JumpMaxSteps) is sent as that many
//     NextTrack commands, which keeps the client's own queue, history,
//     shuffle state and source.
//   - Otherwise the reported queue is sent with PlayNow and the start index;
//     clients such as Finamp then rebuild their queue from these items only.
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
	p.mu.Lock()
	c := p.contexts[sessionID]
	jump := p.jumps[sessionID]
	p.mu.Unlock()

	if c != nil && !c.Shuffled {
		if err := p.sendPlay(ctx, jfSessionID, "PlayNow", ids, index, true); err != nil {
			return err
		}
		p.mu.Lock()
		if c := p.contexts[sessionID]; c != nil {
			// Still the same playlist; wait for the client to report again.
			p.startContextLocked(sessionID, p.live[sessionID], c.QueueContext, c.itemIDs)
		}
		delete(p.jumps, sessionID)
		p.mu.Unlock()
		return nil
	}

	cur := -1
	if q.CurrentIndex != nil {
		cur = *q.CurrentIndex
	}
	if jump != nil && slices.Equal(jump.ids, ids) {
		// Our earlier NextTrack jumps moved the client although it has not
		// reported anything new (poll drops the record once it does).
		cur = jump.index
	}
	if steps := index - cur; cur >= 0 && steps > 0 && steps <= p.cfg.JumpMaxSteps && p.stepsFit(ctx, steps) {
		return p.stepForward(ctx, sessionID, jfSessionID, ids, cur, steps)
	}

	err = p.sendPlay(ctx, jfSessionID, "PlayNow", ids, index, true)
	p.mu.Lock()
	delete(p.jumps, sessionID)
	if err == nil && c != nil {
		// A shuffled context: the client's queue is now the reported list.
		delete(p.contexts, sessionID)
	}
	p.mu.Unlock()
	return err
}

// stepsFit reports whether steps NextTrack commands fit into the command's
// time budget (each is a request plus the step delay).
func (p *Provider) stepsFit(ctx context.Context, steps int) bool {
	dl, ok := ctx.Deadline()
	return !ok || time.Until(dl) > time.Duration(steps)*(p.cfg.JumpStepDelay+200*time.Millisecond)
}

// stepForward sends steps NextTrack commands, one at a time: each is
// accepted by the server before the next, with JumpStepDelay in between so
// the client handles them in order.
func (p *Provider) stepForward(ctx context.Context, sessionID, jfSessionID string, ids []string, cur, steps int) error {
	p.mu.Lock()
	base := p.live[sessionID]
	p.mu.Unlock()
	record := func(done int) {
		p.mu.Lock()
		p.jumps[sessionID] = &stepJump{baseTrack: nowPlayingID(base), baseQueue: queueOnlyRevision(base), ids: ids, index: cur + done}
		p.mu.Unlock()
	}
	p.log.Debug("queue_jump as NextTrack commands", "session", sessionID, "from", cur, "steps", steps)
	for done := 0; done < steps; done++ {
		if done > 0 && p.cfg.JumpStepDelay > 0 {
			select {
			case <-time.After(p.cfg.JumpStepDelay):
			case <-ctx.Done():
				record(done)
				return ctx.Err()
			}
		}
		if err := p.client.PlaystateCommand(ctx, jfSessionID, "NextTrack", nil); err != nil {
			record(done)
			if done > 0 {
				return model.Wrap(model.CodeOf(err), err, fmt.Sprintf("queue_jump stopped after %d of %d steps: %s", done, steps, model.AsError(err).Message))
			}
			return err
		}
	}
	record(steps)
	return nil
}
