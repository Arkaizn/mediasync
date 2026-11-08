package jellyfin

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
)

// expectedTimeout bounds how long an expected track is shown without the
// player confirming it (Finamp reports the old track for 90 s after the
// last remote command, longer if commands keep coming).
const expectedTimeout = 120 * time.Second

// expectation is the track a remote command should have started. It is
// recorded when the command is sent and shown (active) only once the
// engine found the command acknowledged with the player still reporting the
// previous track. It ends when the player reports any other track, after
// expectedTimeout, or with the next command.
type expectation struct {
	track        model.Track
	reason       string
	since        time.Time
	baseTrack    string // the track the player reported when the command was sent
	contextIndex *int
	active       bool
}

func (e *expectation) public() *model.ExpectedTrack {
	t := e.track
	t.Artists = append([]string(nil), t.Artists...)
	t.AlbumArtists = append([]string(nil), t.AlbumArtists...)
	if len(t.Artists) == 0 {
		t.Artists = nil
	}
	if len(t.AlbumArtists) == 0 {
		t.AlbumArtists = nil
	}
	return &model.ExpectedTrack{Track: t, Since: e.since, Reason: e.reason, ContextIndex: e.contextIndex}
}

// expectedRevision is part of the queue revision while an expectation is
// shown (sessions.queue may then mark the expected position).
func expectedRevision(e *expectation) string {
	if e == nil || !e.active {
		return ""
	}
	idx := "-"
	if e.contextIndex != nil {
		idx = strconv.Itoa(*e.contextIndex)
	}
	return ":exp:" + idx
}

// ActivateExpected implements provider.ExpectationSource.
func (p *Provider) ActivateExpected(sessionID string) {
	p.mu.Lock()
	e := p.expected[sessionID]
	if e != nil {
		e.active = true
	}
	p.mu.Unlock()
	if e != nil {
		p.Refresh() // publish it
	}
}

// recordExpected remembers the target of a command just sent (not shown
// until activated). base is the session as reported when it was sent.
func (p *Provider) recordExpected(ctx context.Context, sessionID string, base SessionInfo, targetID, reason string, contextIndex *int) {
	if targetID == "" {
		return
	}
	meta := p.itemMeta(ctx, []string{targetID}, nil)
	it, ok := meta[targetID]
	if !ok {
		it = BaseItem{ID: targetID} // details unknown: only the id
	}
	e := &expectation{track: p.trackOf(it), reason: reason, since: p.now(), baseTrack: nowPlayingID(base), contextIndex: contextIndex}
	p.mu.Lock()
	p.expected[sessionID] = e
	p.mu.Unlock()
}

// updateExpectedLocked ends an expectation once the player reports any
// track other than the one it reported when the command was sent, or after
// expectedTimeout (p.mu held).
func (p *Provider) updateExpectedLocked(id string, si SessionInfo) {
	e := p.expected[id]
	if e == nil {
		return
	}
	if !strings.EqualFold(nowPlayingID(si), e.baseTrack) || p.now().Sub(e.since) > expectedTimeout {
		delete(p.expected, id)
	}
}

// expectStep records the target of next/previous, but only where the order
// is unambiguous: a queue context in its own order (not shuffled by anyone)
// with a known position, which may be the position an earlier command led
// us to expect. previous also needs a reported position under 3 s (players
// restart the current track instead) and no pending expectation.
func (p *Provider) expectStep(ctx context.Context, sessionID string, si SessionInfo, pub model.Session, prev *expectation, cmd model.CommandName) {
	p.mu.Lock()
	c := p.contexts[sessionID]
	if c == nil || c.membership {
		p.mu.Unlock()
		return
	}
	ids := append([]string(nil), c.itemIDs...)
	cur := c.current
	chained := prev != nil && prev.active && prev.contextIndex != nil
	if chained {
		cur = *prev.contextIndex
	}
	p.mu.Unlock()
	if cur < 0 {
		return
	}
	target, reason := cur+1, model.ExpectedNext
	if cmd == model.CmdPrevious {
		pos := pub.Playback.CurrentPosition(p.now())
		if chained || pos == nil || *pos >= 3000 {
			return
		}
		target, reason = cur-1, model.ExpectedPrevious
	}
	if target < 0 || target >= len(ids) {
		return
	}
	p.recordExpected(ctx, sessionID, si, ids[target], reason, model.Ptr(target))
}
