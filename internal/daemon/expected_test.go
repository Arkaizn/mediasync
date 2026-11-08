package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

// The simulated Finamp keeps reporting the old track after remote commands
// (its 90 s controlled mode). mediasync shows what it sent as "expected",
// marked unconfirmed, without touching the reported track.
func TestEndToEndExpectedTrack(t *testing.T) {
	h := newHarness(t, true, func(c *config.Config) {
		c.Commands.RemoteVerifyTimeout = config.Duration(time.Second)
	})
	var playlist []string
	for i := range 5 {
		id := fmt.Sprintf("%032x", 0xf000+i)
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: fmt.Sprintf("Song %d", i), Type: "Audio", Artists: []string{"Artist A"}, Album: "Album Y", RunTimeTicks: 2_000_000_000})
		playlist = append(playlist, id)
	}
	const oldID = "0000000000000000000000000000bad1"
	h.jf.AddItem(fakejellyfin.Item{ID: oldID, Name: "In My Room", Type: "Audio", RunTimeTicks: 2_000_000_000})
	h.jf.AddItem(fakejellyfin.Item{ID: "plist0000000000000000000000ex9ec", Name: "Electronic", Type: "Playlist", Children: playlist})
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-exp",
		Controllable: true, ApplyCommands: true, StaleTrackReports: true,
		ItemID: oldID, Title: "In My Room", RunTimeTicks: 2_000_000_000, PositionTicks: 600_000_000,
	})
	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	var ph model.Session
	waitList(t, c, nil, "phone", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.Player.DeviceID != nil && *x.Player.DeviceID == "iphone-exp" {
				ph = x
				return true
			}
		}
		return false
	})
	if ph.Expected != nil {
		t.Fatalf("expected without a command: %+v", ph.Expected)
	}
	get := func() model.Session {
		return call[struct{ Session model.Session }](t, c, "sessions.get", map[string]any{"session_id": ph.ID}).Session
	}
	waitExpected := func(what string, cond func(*model.ExpectedTrack) bool) model.Session {
		t.Helper()
		deadline := time.Now().Add(wait)
		for {
			s := get()
			if cond(s.Expected) {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: expected %+v", what, s.Expected)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// next without a known order (no context): nothing to expect.
	res := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "next"})
	if res.Outcome != model.OutcomeAcknowledged {
		t.Fatalf("next: %+v", res)
	}
	time.Sleep(700 * time.Millisecond)
	if s := get(); s.Expected != nil {
		t.Errorf("next with an unknown order set expected: %+v", s.Expected)
	}

	// play_items: acknowledged (old track still reported) → expected.
	res = call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "play_items", "item_ids": []string{"plist0000000000000000000000ex9ec"}})
	if res.Outcome != model.OutcomeAcknowledged {
		t.Fatalf("play_items: %+v", res)
	}
	s := waitExpected("play_items", func(e *model.ExpectedTrack) bool { return e != nil })
	e := s.Expected
	if e.Reason != "play_items" || *e.Track.TrackID != playlist[0] || *e.Track.Title != "Song 0" || e.Track.Artists[0] != "Artist A" ||
		*e.Track.DurationMS != 200_000 || e.ContextIndex == nil || *e.ContextIndex != 0 || e.Since.IsZero() {
		t.Errorf("expected %+v", e)
	}
	if *s.Track.TrackID != oldID || *s.Track.Title != "In My Room" {
		t.Errorf("the reported track must stay: %v", *s.Track.Title)
	}
	q := call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph.ID})
	if q.Context == nil || q.CurrentIndex == nil || *q.CurrentIndex != 0 || !q.CurrentExpected {
		t.Errorf("queue: context %+v current %v expected %v", q.Context, q.CurrentIndex, q.CurrentExpected)
	}

	// next in the context, chained from the expected position.
	call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "next"})
	s = waitExpected("next", func(e *model.ExpectedTrack) bool { return e != nil && e.Reason == "next" })
	if *s.Expected.Track.TrackID != playlist[1] || *s.Expected.ContextIndex != 1 {
		t.Errorf("next expected %+v", s.Expected)
	}
	// queue_jump: its target.
	call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "queue_jump", "index": 3})
	s = waitExpected("queue_jump", func(e *model.ExpectedTrack) bool { return e != nil && e.Reason == "queue_jump" })
	if *s.Expected.Track.TrackID != playlist[3] || *s.Expected.ContextIndex != 3 {
		t.Errorf("jump expected %+v", s.Expected)
	}

	// The client reports the new track: expected is cleared.
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.ItemID, c.Title = playlist[3], "Song 3" })
	s = waitExpected("cleared", func(e *model.ExpectedTrack) bool { return e == nil })
	if *s.Track.TrackID != playlist[3] {
		t.Errorf("reported track %v", *s.Track.TrackID)
	}
	if q := call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph.ID}); q.CurrentExpected || *q.CurrentIndex != 3 {
		t.Errorf("queue after the report: current %v expected %v", q.CurrentIndex, q.CurrentExpected)
	}
}
