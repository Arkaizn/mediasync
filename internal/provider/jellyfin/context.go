package jellyfin

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/arkaizn/mediasync/internal/model"
)

// queueContext is a queue mediasync started from one playlist or album
// (play_items "now"). Clients do not report where their queue came from
// (Finamp reports only the upcoming tracks), so this is only known for
// playback mediasync started, and never guessed.
type queueContext struct {
	model.QueueContext
	// itemIDs is the expanded track list in the order it was sent (the
	// playlist order, or the shuffled order).
	itemIDs []string
	// pending: the client has not reported anything since the context was
	// (re)started; base* is what it reported then. Clients such as Finamp
	// keep reporting the previous track for a while after remote commands.
	pending              bool
	baseTrack, baseQueue string
	// current is the position of the reported track in itemIDs (-1: unknown).
	current int
}

// stepJump remembers where NextTrack jumps left a session's queue, as long
// as the session reports nothing new (Finamp reports the old track for up to
// 90 s after remote commands, so the reported position cannot be trusted).
type stepJump struct {
	baseTrack, baseQueue string
	ids                  []string // the queue items the jump was counted in
	index                int      // where the jumps should have arrived
}

func nowPlayingID(si SessionInfo) string {
	if si.NowPlayingItem == nil {
		return ""
	}
	return si.NowPlayingItem.ID
}

// queueOnlyRevision fingerprints the reported queue alone (not the track).
func queueOnlyRevision(si SessionInfo) string {
	h := sha256.New()
	h.Write([]byte(si.PlaylistItemID))
	for _, e := range si.NowPlayingQueue {
		h.Write([]byte{0})
		h.Write([]byte(e.ID))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// contextPosition finds the reported track in the context and checks the
// reported upcoming tracks against the context order (when checkQueue).
// It returns -1 if the session no longer plays from the context. Of several
// occurrences (duplicates in a playlist) the one nearest after near wins.
func contextPosition(ids []string, track string, queue []QueueEntry, checkQueue bool, near int) int {
	var upcoming []string
	for _, e := range queue {
		upcoming = append(upcoming, e.ID)
	}
	if len(upcoming) > 0 && strings.EqualFold(upcoming[0], track) {
		upcoming = upcoming[1:] // Jellyfin Web style: the list includes the current track
	}
	// The first match at or after near, else the last one before it.
	after, before := -1, -1
	for i, id := range ids {
		if !strings.EqualFold(id, track) || (checkQueue && !followsInOrder(ids, i, upcoming)) {
			continue
		}
		if i >= near {
			if after < 0 {
				after = i
			}
		} else {
			before = i
		}
	}
	if after >= 0 {
		return after
	}
	return before
}

// followsInOrder reports whether upcoming continues ids after position i
// (wrapping around for clients that repeat the queue).
func followsInOrder(ids []string, i int, upcoming []string) bool {
	for j, id := range upcoming {
		if !strings.EqualFold(ids[(i+1+j)%len(ids)], id) {
			return false
		}
	}
	return true
}

// updateContextLocked re-evaluates the session's context against the latest
// report (p.mu held). The context ends when the session plays something
// outside it or reports upcoming tracks that do not follow its order.
func (p *Provider) updateContextLocked(id string, si SessionInfo) {
	c := p.contexts[id]
	if c == nil {
		return
	}
	track, queue := nowPlayingID(si), queueOnlyRevision(si)
	if c.pending && strings.EqualFold(track, c.baseTrack) && queue == c.baseQueue {
		return // nothing new reported yet
	}
	c.pending = false
	// Only a queue reported after the context started can be compared (on
	// Jellyfin 12.x the server keeps an older snapshot until the next stop).
	pos := contextPosition(c.itemIDs, track, si.NowPlayingQueue, queue != c.baseQueue, max(c.current, 0))
	if pos < 0 {
		p.log.Debug("queue context ended", "session", id, "context", c.ID)
		delete(p.contexts, id)
		return
	}
	c.current = pos
}

// startContextLocked records a context for a play command just sent
// (p.mu held). before is the session as reported before the command.
func (p *Provider) startContextLocked(id string, before SessionInfo, qc model.QueueContext, ids []string) {
	p.contexts[id] = &queueContext{QueueContext: qc, itemIDs: ids, pending: true,
		baseTrack: nowPlayingID(before), baseQueue: queueOnlyRevision(before), current: -1}
}

// contextRevision is part of the session's queue revision, so that a
// context starting, ending or moving emits queue.changed.
func contextRevision(c *queueContext) string {
	if c == nil {
		return ""
	}
	return "ctx:" + c.ID + ":" + strconv.FormatBool(c.Shuffled) + ":" + strconv.FormatBool(c.pending) + ":" + strconv.Itoa(c.current)
}

// contextType maps a Jellyfin item type to a context type ("" if none).
func contextType(itemType string) string {
	switch {
	case strings.EqualFold(itemType, "Playlist"):
		return model.ContextPlaylist
	case strings.EqualFold(itemType, "MusicAlbum"):
		return model.ContextAlbum
	}
	return ""
}
