package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

// After queue_jump / play_items "now", the new track reported by the client
// (NowPlayingItem and PlaylistItemId change) must reach subscribers as
// session.updated and sessions.get. A client that keeps reporting the old
// track (Finamp while remote-controlled) must give an honest
// "acknowledged", and mediasync must not invent the new track.
func TestEndToEndTrackAfterRemotePlayNow(t *testing.T) {
	h := newHarness(t, true, func(c *config.Config) {
		c.Commands.RemoteVerifyTimeout = config.Duration(1500 * time.Millisecond)
		c.Jellyfin.QueueJumpMaxSteps = 0 // this test is about the PlayNow path
	})
	h.jf.SetStoreQueue(true)
	ids := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa3"}
	for i, id := range ids {
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: "Track " + string(rune('1'+i)), Type: "Audio", Artists: []string{"Artist A"}, RunTimeTicks: 1_800_000_000})
	}
	h.jf.AddItem(fakejellyfin.Item{ID: "plist000000000000000000000000001", Name: "Road Trip", Type: "Playlist", Children: []string{ids[2], ids[1]}})
	newPhone := func(device string, stale bool) *fakejellyfin.Client {
		return h.jf.AddClient(&fakejellyfin.Client{
			UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: device, DeviceID: device,
			Controllable: true, ApplyCommands: true, StaleTrackReports: stale,
			ItemID: ids[0], Title: "Track 1", Artists: []string{"Artist A"}, RunTimeTicks: 1_800_000_000,
			PositionTicks: 600_000_000, // 1:00, so a restart at 0:00 is visible
			Queue:         []fakejellyfin.QueueEntry{{ID: ids[1], PlaylistItemID: "queue"}, {ID: ids[2], PlaylistItemID: "queue"}},
		})
	}
	good := newPhone("iphone-good", false)
	stale := newPhone("iphone-stale", true)

	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	sessionOf := func(device string) model.Session {
		var out model.Session
		waitList(t, c, map[string]any{"include_hidden": true}, device, func(s snap) bool {
			for _, x := range s.Sessions {
				if x.Player.DeviceID != nil && *x.Player.DeviceID == device && x.Has(model.CapQueueJump) {
					out = x
					return true
				}
			}
			return false
		})
		return out
	}
	gs, ss := sessionOf("iphone-good"), sessionOf("iphone-stale")
	call[map[string]any](t, c, "subscribe", map[string]any{"include_hidden": true})

	// A client that reports the new track: verified, session.updated with
	// the new track, and sessions.get agrees.
	res := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": gs.ID, "command": "queue_jump", "index": 2})
	if res.Outcome != model.OutcomeVerified || res.Session == nil || *res.Session.Track.TrackID != ids[2] {
		t.Fatalf("queue_jump on a reporting client: %+v", res)
	}
	nextEvent(t, c, "session.updated with the new track", func(e event) bool {
		return e.Type == "session.updated" && e.SessionID == gs.ID && e.Session != nil && *e.Session.Track.TrackID == ids[2]
	})
	if got := call[struct{ Session model.Session }](t, c, "sessions.get", map[string]any{"session_id": gs.ID}).Session; *got.Track.TrackID != ids[2] || *got.Track.Title != "Track 3" {
		t.Errorf("sessions.get after jump: %v %v", *got.Track.TrackID, *got.Track.Title)
	}
	if st := h.jf.ClientState(good.SessionID); st.PlaylistItemID != ids[0] {
		t.Errorf("fake did not change PlaylistItemId: %q", st.PlaylistItemID)
	}

	// A client that keeps reporting the old track: acknowledged, with a
	// detail that says so; the old track stays (nothing invented).
	for _, tc := range []struct {
		cmd    map[string]any
		detail string
	}{
		// play_items first: the client then still reports 1:00, so its
		// stale report (old track, 0:00) is a restart under the old id,
		// which is not proof of the new track.
		{map[string]any{"session_id": ss.ID, "command": "play_items", "item_ids": []string{"plist000000000000000000000000001"}, "mode": "now"},
			"position restarted, but it still reports the previous track"},
		// The queue is now the playlist (context): track 3, track 2. The
		// position is already at 0:00, so only "no new track" is known.
		{map[string]any{"session_id": ss.ID, "command": "queue_jump", "index": 1}, "has not reported a new track"},
	} {
		cmd := tc.cmd
		res := call[model.CommandResult](t, c, "player.command", cmd)
		if res.Outcome != model.OutcomeAcknowledged || !strings.Contains(res.Detail, tc.detail) {
			t.Errorf("%s on a stale-reporting client: outcome %s, detail %q", cmd["command"], res.Outcome, res.Detail)
		}
		if res.Session == nil || *res.Session.Track.TrackID != ids[0] {
			t.Errorf("%s: session must keep showing what the server reports", cmd["command"])
		}
	}
	st := h.jf.ClientState(stale.SessionID)
	if st.Playing != ids[1] || st.ItemID != ids[0] { // plays track 2, still reports track 1
		t.Errorf("fake stale client: playing %s, reporting %s", st.Playing, st.ItemID)
	}
	if got := call[struct{ Session model.Session }](t, c, "sessions.get", map[string]any{"session_id": ss.ID}).Session; *got.Track.TrackID != ids[0] {
		t.Errorf("sessions.get invented a track: %v", *got.Track.TrackID)
	}

	// A playlist that cannot be sent at all (a very strict proxy): a clear
	// error with the HTTP status, logged at info level.
	var many []string
	for i := range 20 {
		id := fmt.Sprintf("%032x", 0xc000+i)
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: id, Type: "Audio", RunTimeTicks: 1_800_000_000})
		many = append(many, id)
	}
	h.jf.AddItem(fakejellyfin.Item{ID: "plist000000000000000000000000002", Name: "Many", Type: "Playlist", Children: many})
	h.jf.SetMaxRequestLine(200, 414)
	cerr := callErr(t, c, "player.command", map[string]any{"session_id": gs.ID, "command": "play_items", "item_ids": []string{"plist000000000000000000000000002"}})
	if cerr == nil || cerr.Code != "provider_error" || !strings.Contains(cerr.Message, "too large for one command") || cerr.Details["http_status"] != float64(414) {
		t.Errorf("too-long play command: %+v", cerr)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `level=INFO msg="command failed"`) || !strings.Contains(logs, "http_status=414") {
		t.Errorf("command failure not logged with its HTTP status:\n%s", logs[max(0, len(logs)-2000):])
	}
}
