package jellyfin

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// pending: the client has not reported a track of the context since it
	// was (re)started. baseTrack is the track it reported then. Clients such
	// as Finamp keep reporting the previous track (while their queue
	// snapshot may change) for a while after remote commands, so only a
	// reported context track ends this, a track outside it, or the time box.
	pending   bool
	baseTrack string
	startedAt time.Time
	// switchQueue is the queue snapshot reported together with the first
	// context track. On Jellyfin 12.x that snapshot can predate the switch
	// (the server keeps it until the client's next stop report), so the
	// upcoming order is only checked against later snapshots.
	switchQueue string
	// current is the position of the reported track in itemIDs (-1: unknown).
	current int
}

// contextPendingTimeout bounds how long a context waits for the client to
// report one of its tracks. Finamp stays in its "controlled mode" for 90 s
// after a remote command and then reports the real track only with its next
// playback event (usually the next track change), so this is generous; a
// client that ignored the command ends the context earlier, when it reports
// a track outside it.
const contextPendingTimeout = 10 * time.Minute

// stepJump remembers where NextTrack jumps left a session's queue, as long
// as the session reports the same track (Finamp reports the old track, with
// a changing queue snapshot, for up to 90 s after remote commands, so the
// reported position cannot be trusted).
type stepJump struct {
	baseTrack string   // the track reported when the jump was sent
	ids       []string // the queue items the jump was counted in
	index     int      // where the jumps should have arrived
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
// report (p.mu held). The context ends when the client plays something
// outside it, when a queue snapshot reported after the switch does not
// follow its order, or when no context track is reported in time.
func (p *Provider) updateContextLocked(id string, si SessionInfo) {
	c := p.contexts[id]
	if c == nil {
		return
	}
	track, queue := nowPlayingID(si), queueOnlyRevision(si)
	inContext := slices.ContainsFunc(c.itemIDs, func(x string) bool { return strings.EqualFold(x, track) })
	end := func(reason string) {
		p.log.Info("queue context ended", "session", id, "context", c.Name, "context_id", c.ID, "reason", reason)
		delete(p.contexts, id)
	}
	if c.pending {
		stillOld := track == "" || strings.EqualFold(track, c.baseTrack)
		timedOut := p.now().Sub(c.startedAt) > contextPendingTimeout
		switch {
		case inContext && (!stillOld || timedOut):
			// The first context track (or, after waiting, the old track
			// that is also part of the context).
			c.pending, c.switchQueue = false, queue
			c.current = contextPosition(c.itemIDs, track, nil, false, 0)
		case stillOld && !timedOut:
			// Still the previous track, whatever the queue snapshot does.
		case stillOld:
			end("timeout: the client never reported a track of the context")
		default:
			end("track outside the context")
		}
		return
	}
	if !inContext {
		end("track outside the context")
		return
	}
	// Only a snapshot reported after the switch says anything about the
	// client's queue now; the one that came with the switch may be older.
	checkQueue := queue != c.switchQueue && len(si.NowPlayingQueue) > 0
	pos := contextPosition(c.itemIDs, track, si.NowPlayingQueue, checkQueue, max(c.current, 0))
	if pos < 0 {
		end("upcoming tracks do not follow the context order")
		return
	}
	c.current = pos
}

// startContextLocked records a context for a play command just sent
// (p.mu held). before is the session as reported before the command.
func (p *Provider) startContextLocked(id string, before SessionInfo, qc model.QueueContext, ids []string) {
	p.contexts[id] = &queueContext{QueueContext: qc, itemIDs: ids, pending: true,
		baseTrack: nowPlayingID(before), startedAt: p.now(), current: -1}
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
