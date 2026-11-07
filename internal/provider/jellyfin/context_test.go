package jellyfin

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func stepEnv(t *testing.T, maxSteps int) *env {
	return newEnv(t, func(c *Config) {
		c.JumpMaxSteps = maxSteps
		c.JumpStepDelay = 5 * time.Millisecond
	})
}

// longQueuePhone plays tracks[0] and reports tracks[1:] as upcoming.
func longQueuePhone(e *env, tracks []string, stale bool) *fakejellyfin.Client {
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.ItemID, c.Title, c.StaleTrackReports = tracks[0], "Song 000", stale
		c.PositionTicks = 600_000_000
		c.Queue = nil
		for _, id := range tracks[1:] {
			c.Queue = append(c.Queue, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
		}
	})
	return ph
}

func received(e *env, ph *fakejellyfin.Client) []string {
	return e.srv.ClientState(ph.SessionID).Received
}

func jump(t *testing.T, e *env, id string, index int) {
	t.Helper()
	if err := e.prov.Command(context.Background(), id, model.Command{Name: model.CmdQueueJump, Index: model.Ptr(index)}); err != nil {
		t.Fatalf("jump %d: %v", index, err)
	}
}

func TestQueueJumpForwardUsesNextTrackAndFallsBack(t *testing.T) {
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	_, tracks := bigPlaylist(e.srv, 30)
	ph := longQueuePhone(e, tracks, false)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })

	// 3 ahead: three NextTrack commands, no PlayNow.
	jump(t, e, s.ID, 3)
	if got := received(e, ph); !slices.Equal(got, []string{"NextTrack", "NextTrack", "NextTrack"}) {
		t.Fatalf("forward jump sent %v", got)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == tracks[3] })

	// The queue is now tracks[3] + upcoming; 20 ahead is beyond the limit.
	jump(t, e, s.ID, 20)
	got := received(e, ph)
	if last := got[len(got)-1]; !strings.HasPrefix(last, "Play(PlayNow:") || !strings.HasSuffix(last, "@20)") || len(got) != 4 {
		t.Errorf("long jump: %v", got)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == tracks[23] })

	// Backwards cannot be done with NextTrack: PlayNow. The reported queue
	// starts at the current track, so first make one earlier track visible.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.Queue = append([]fakejellyfin.QueueEntry{{ID: tracks[23], PlaylistItemID: "queue"}}, c.Queue...)
		c.PlaylistItemID = "queue"
	})
	e.prov.Refresh()
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })
	q, _ := e.prov.Queue(context.Background(), s.ID)
	if q.CurrentIndex == nil || *q.CurrentIndex != 0 {
		t.Fatalf("current %v", q.CurrentIndex)
	}
	before := len(received(e, ph))
	jump(t, e, s.ID, 0) // the current track itself: not a forward jump
	if got := received(e, ph); len(got) != before+1 || !strings.HasPrefix(got[len(got)-1], "Play(PlayNow:") {
		t.Errorf("jump to the current track: %v", got[before:])
	}
}

func TestQueueJumpStepsDisabledAndBudget(t *testing.T) {
	for _, maxSteps := range []int{0, 2} {
		e := stepEnv(t, maxSteps)
		e.srv.SetStoreQueue(true)
		library(e.srv)
		_, tracks := bigPlaylist(e.srv, 10)
		ph := longQueuePhone(e, tracks, false)
		e.login(t)
		e.run(t)
		s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })
		jump(t, e, s.ID, 3)
		if got := received(e, ph); len(got) != 1 || !strings.HasPrefix(got[0], "Play(PlayNow:") || !strings.HasSuffix(got[0], "@3)") {
			t.Errorf("max %d: 3 ahead must use PlayNow: %v", maxSteps, got)
		}
	}
	// Too little time left for the steps: PlayNow.
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	_, tracks := bigPlaylist(e.srv, 20)
	ph := longQueuePhone(e, tracks, false)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdQueueJump, Index: model.Ptr(10)}); err != nil {
		t.Fatal(err)
	}
	if got := received(e, ph); len(got) != 1 || !strings.HasPrefix(got[0], "Play(PlayNow:") {
		t.Errorf("short deadline: %v", got)
	}
}

// A client that keeps reporting the old track (Finamp after remote
// commands): consecutive forward jumps count from where the earlier jump
// went, not from the stale reported position.
func TestQueueJumpStepsWithStaleReportingClient(t *testing.T) {
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	_, tracks := bigPlaylist(e.srv, 12)
	ph := longQueuePhone(e, tracks, true)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })

	jump(t, e, s.ID, 2)
	jump(t, e, s.ID, 5) // three more, not five
	st := e.srv.ClientState(ph.SessionID)
	if n := len(st.Received); n != 5 || st.Playing != tracks[5] || st.ItemID != tracks[0] {
		t.Fatalf("sent %v, playing %s (want %s), reporting %s", st.Received, st.Playing, tracks[5], st.ItemID)
	}
	// Backwards from there: PlayNow with the target.
	jump(t, e, s.ID, 1)
	st = e.srv.ClientState(ph.SessionID)
	if last := st.Received[len(st.Received)-1]; !strings.HasSuffix(last, "@1)") || st.Playing != tracks[1] {
		t.Errorf("backward: %v playing %s", st.Received, st.Playing)
	}
	// Once the client reports again, the remembered position is dropped.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.ItemID, c.StaleTrackReports = tracks[1], false
		c.Queue = nil
		for _, id := range tracks[2:] {
			c.Queue = append(c.Queue, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
		}
	})
	e.prov.Refresh()
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == tracks[1] })
	n := len(e.srv.ClientState(ph.SessionID).Received)
	jump(t, e, s.ID, 2) // index 2 of [t1, t2, ...] = tracks[3]: two steps
	if got := e.srv.ClientState(ph.SessionID).Received[n:]; !slices.Equal(got, []string{"NextTrack", "NextTrack"}) {
		t.Errorf("after a fresh report: %v", got)
	}
}

func TestQueueContextFromPlaylist(t *testing.T) {
	e := stepEnv(t, 15) // Jellyfin 12.x: no reported queue
	library(e.srv)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	if q, _ := e.prov.Queue(ctx, s.ID); q.Available || q.Context != nil || s.Has(model.CapQueue) {
		t.Fatalf("client-started playback must have no queue and no context: %+v", q)
	}

	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
		t.Fatal(err)
	}
	// The fake reports the new track: the context is confirmed.
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		return x.Has(model.CapQueue) && x.Has(model.CapQueueJump) && *x.Track.TrackID == t3
	})
	q, err := e.prov.Queue(ctx, s.ID)
	if err != nil || !q.Available || q.Source == nil || *q.Source != "context" || q.Context == nil ||
		*q.Context != (model.QueueContext{Type: "playlist", ID: pl, Name: "Road Trip"}) {
		t.Fatalf("context queue %+v %v", q, err)
	}
	if len(q.Items) != 2 || q.Items[0].ItemID != t3 || q.Items[1].ItemID != t1 || *q.Items[1].Title != "Track 1" || q.CurrentIndex == nil || *q.CurrentIndex != 0 {
		t.Errorf("items %+v current %v", q.Items, q.CurrentIndex)
	}

	// queue_jump on a context: the whole playlist with the start index.
	jump(t, e, s.ID, 1)
	if got := received(e, ph); got[len(got)-1] != "Play(PlayNow:"+t3+","+t1+"@1)" {
		t.Errorf("context jump sent %v", got)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == t1 })
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context == nil || q.CurrentIndex == nil || *q.CurrentIndex != 1 || len(q.Items) != 2 {
		t.Errorf("after the jump %+v", q)
	}

	// Playing something outside the playlist ends the context.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.ItemID, c.Title = t2, "Track 2" })
	e.prov.Refresh()
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return !x.Has(model.CapQueue) })
	if q, _ := e.prov.Queue(ctx, s.ID); q.Available || q.Context != nil {
		t.Errorf("context should have ended: %+v", q)
	}
}

func TestQueueContextAlbumShuffleAndReplacement(t *testing.T) {
	e := stepEnv(t, 15)
	library(e.srv)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()

	// A single album played now is expanded by mediasync (its tracks are
	// the context).
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{al}}); err != nil {
		t.Fatal(err)
	}
	if got := received(e, ph); got[len(got)-1] != "Play(PlayNow:"+t1+","+t2+","+t3+"@0)" {
		t.Errorf("album sent %v", got)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) })
	q, _ := e.prov.Queue(ctx, s.ID)
	if q.Context == nil || q.Context.Type != "album" || q.Context.Name != "Album X" || len(q.Items) != 3 {
		t.Fatalf("album context %+v", q)
	}

	// Another play_items replaces it: a single track has no context.
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{t2}}); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context != nil {
		t.Errorf("play_items of a track kept the old context: %+v", q.Context)
	}

	// Shuffled: the context is known and marked shuffled; the items stay
	// what the client reports (none on 12.x).
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}, Shuffle: model.Ptr(true)}); err != nil {
		t.Fatal(err)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) })
	q, _ = e.prov.Queue(ctx, s.ID)
	if q.Context == nil || !q.Context.Shuffled || q.Available || (q.Source != nil && *q.Source == "context") {
		t.Errorf("shuffled context %+v", q)
	}

	// Next/last also change the queue: the context is dropped.
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
		t.Fatal(err)
	}
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{t2}, Mode: "next"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context != nil {
		t.Errorf("play next kept the context: %+v", q.Context)
	}
}

func TestQueueContextChecksReportedUpcomingTracks(t *testing.T) {
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true) // the client's upcoming tracks are visible
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 8)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context != nil {
		t.Fatal("never guess a context for a client-started queue")
	}
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
		t.Fatal(err)
	}
	// The fake reports tracks[0] with tracks[1:] upcoming: consistent.
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == tracks[0] })
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context == nil || len(q.Items) != 8 || *q.CurrentIndex != 0 {
		t.Fatalf("consistent queue: %+v", q)
	}
	// The client adds a track of its own to "next up": the reported queue
	// no longer follows the playlist, so the context ends.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.Queue = append([]fakejellyfin.QueueEntry{{ID: t2, PlaylistItemID: "nextUp"}}, c.Queue...)
	})
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return q.Context == nil && q.Source != nil && *q.Source == "jellyfin" && q.Items[1].ItemID == t2
	})
}

func TestContextPosition(t *testing.T) {
	ids := []string{"a", "b", "c", "a", "d"}
	q := func(xs ...string) []QueueEntry {
		var out []QueueEntry
		for _, x := range xs {
			out = append(out, QueueEntry{ID: x})
		}
		return out
	}
	cases := []struct {
		track string
		queue []QueueEntry
		check bool
		near  int
		want  int
	}{
		{"c", nil, true, 0, 2},
		{"x", nil, false, 0, -1},
		{"a", nil, false, 2, 3},             // the duplicate after near
		{"a", nil, false, 4, 3},             // none after near: the last before
		{"a", q("d"), true, 0, 3},           // only the second "a" is followed by d
		{"a", q("a", "b", "c"), true, 0, 0}, // the list may include the current track
		{"d", q("a", "b"), true, 0, 4},      // wraps around (repeat)
		{"b", q("d"), true, 0, -1},          // inconsistent
	}
	for _, c := range cases {
		if got := contextPosition(ids, c.track, c.queue, c.check, c.near); got != c.want {
			t.Errorf("%s %v check=%v near=%d: %d, want %d", c.track, c.queue, c.check, c.near, got, c.want)
		}
	}
}

func TestFailedPlayItemsKeepsTheContext(t *testing.T) {
	e := stepEnv(t, 15)
	library(e.srv)
	queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
		t.Fatal(err)
	}
	s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) })
	// Rejected before anything reaches the client (unknown id).
	err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{"ffffffffffffffffffffffffffffffff"}})
	if model.CodeOf(err) != model.ErrInvalidArgument {
		t.Fatalf("want invalid_argument: %v", err)
	}
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context == nil || q.Context.ID != pl {
		t.Errorf("a failed play_items dropped the context: %+v", q.Context)
	}
}
