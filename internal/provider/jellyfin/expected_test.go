package jellyfin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func TestExpectedTrackTimeoutAndNextCommand(t *testing.T) {
	var offset atomic.Int64 // test clock: real time plus offset
	e := stepEnv(t, 15)
	e.prov.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	library(e.srv)
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.StaleTrackReports = true })
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	play := func() {
		t.Helper()
		if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
			t.Fatal(err)
		}
	}

	// Recorded but not shown until the engine activates it.
	play()
	e.prov.Refresh()
	time.Sleep(150 * time.Millisecond)
	if x, _ := e.sink.Session(s.ID); x.Expected != nil {
		t.Fatal("expected shown before activation")
	}
	e.prov.ActivateExpected(s.ID)
	x := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Expected != nil })
	if *x.Expected.Track.TrackID != t3 || *x.Expected.Track.Title != "Track 3" || *x.Track.TrackID != t1 || x.Playback.PositionMS == nil {
		t.Errorf("expected %+v, reported track %v", x.Expected, *x.Track.TrackID)
	}

	// After the timeout it is gone, although the old track is still reported.
	offset.Store(int64(expectedTimeout + time.Second))
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Expected == nil && *x.Track.TrackID == t1 })

	// The next command clears it at once.
	offset.Store(0)
	play()
	e.prov.ActivateExpected(s.ID)
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Expected != nil })
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPause}); err != nil {
		t.Fatal(err)
	}
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Expected == nil })
}
