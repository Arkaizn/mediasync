package jellyfin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func reversed(ids []string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

// Finamp with its own shuffle on reorders the list it receives: a context
// is then checked by membership, not order.
func TestShuffledContextWithClientShuffleSurvivesNextTrack(t *testing.T) {
	logs := &syncBuf{}
	e := graceEnv(t, 0, logs)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 12)
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Shuffled = true }) // Finamp's shuffle mode on
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Playback.Shuffle != nil && *x.Playback.Shuffle })
	ctx := context.Background()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}, Shuffle: model.Ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// The fake starts the first sent track; Finamp's shuffle then reorders
	// the rest of what it received.
	st := e.srv.ClientState(ph.SessionID)
	var rest []string
	for _, q := range st.Queue {
		rest = append(rest, q.ID)
	}
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Queue = entries(reversed(rest)) })
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) && *x.Track.TrackID == st.ItemID })

	for i := range 3 {
		before := e.srv.ClientState(ph.SessionID).Queue[0].ID
		if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdNext}); err != nil {
			t.Fatalf("next %d: %v", i, err)
		}
		e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == before })
		e.prov.Refresh()
		e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == before })
		q, _ := e.prov.Queue(ctx, s.ID)
		if q.Context == nil || !q.Context.Shuffled {
			t.Fatalf("next %d: context ended: %s", i, logs.String())
		}
		// The client's reported list, with current_index into it.
		if *q.Source != "jellyfin" || q.CurrentIndex == nil || q.Items[*q.CurrentIndex].ItemID != before || len(q.Items) != 12-i-1 {
			t.Errorf("next %d: queue source %v current %v items %d", i, *q.Source, q.CurrentIndex, len(q.Items))
		}
	}
	// Repeat ("playlist") reports tracks again: still fine.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Queue = append(c.Queue, entries(tracks[:3])...) })
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return len(q.Items) > 0 && q.Items[len(q.Items)-1].ItemID == tracks[2]
	})
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context == nil {
		t.Fatal("repeated context tracks ended the context")
	}

	// A track outside the playlist ends it, with the reason in the log.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.Queue = append([]fakejellyfin.QueueEntry{{ID: t2, PlaylistItemID: "nextUp"}}, c.Queue...)
	})
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return q.Context == nil
	})
	if l := logs.String(); !strings.Contains(l, `msg="queue context ended"`) || !strings.Contains(l, `reason="an upcoming track is not part of the context"`) {
		t.Errorf("log: %s", l)
	}
}

// A non-shuffled context whose client switches its shuffle on: checked by
// membership from then on (not ended); shown as the reported list.
func TestContextSwitchesToMembershipWhenClientShuffles(t *testing.T) {
	e := graceEnv(t, 0, nil)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 8)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
		t.Fatal(err)
	}
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return q.Context != nil && *q.Source == "context" && q.CurrentIndex != nil && *q.CurrentIndex == 0
	})
	// The client's shuffle goes on and it reorders the upcoming tracks.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Shuffled = true; c.Queue = entries(reversed(tracks[1:])) })
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return q.Context != nil && *q.Source == "jellyfin" && q.Items[1].ItemID == tracks[7]
	})
	q, _ := e.prov.Queue(ctx, s.ID)
	if q.Context.Shuffled || q.CurrentIndex == nil || *q.CurrentIndex != 0 || len(q.Items) != 8 {
		t.Errorf("membership view: %+v current %v items %d", q.Context, q.CurrentIndex, len(q.Items))
	}
	// A jump in that view uses the reported list (not the playlist order),
	// and the context stays.
	jump(t, e, s.ID, 2)
	got := received(e, ph)
	if last := got[len(got)-1]; !strings.HasPrefix(last, "Play(PlayNow:"+tracks[0]+","+tracks[7]+","+tracks[6]) {
		t.Errorf("jump sent %s", last)
	}
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == tracks[6] })
	if q, _ := e.prov.Queue(ctx, s.ID); q.Context == nil {
		t.Error("the jump ended the context")
	}

	// Shuffle off again: the strict order check applies, so an out-of-order
	// list ends the context.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Shuffled = false; c.Queue = entries([]string{tracks[2], tracks[1]}) })
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(ctx, x.ID)
		return q.Context == nil
	})
}
