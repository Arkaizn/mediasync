package daemon

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/client"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func queueEntries(ids []string) []fakejellyfin.QueueEntry {
	var out []fakejellyfin.QueueEntry
	for _, id := range ids {
		out = append(out, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
	}
	return out
}

// TestAcceptanceQueueContextWithFinampAndRestarts plays a playlist on a
// simulated Finamp (Jellyfin 12.x snapshot semantics, Finamp's report
// sequence after a remote Play) and restarts the service twice in between
// (it restarts itself after every package update). At every step
// sessions.queue must return the context and the whole playlist, with
// current_index on the reported track (null while that track is not part of
// the playlist).
func TestAcceptanceQueueContextWithFinampAndRestarts(t *testing.T) {
	h := newHarness(t, true, nil)
	h.jf.SetStoreQueue(true) // the fake exposes exactly the snapshots the test reports
	var playlist, old []string
	for i := range 10 {
		id := fmt.Sprintf("%032x", 0xa000+i)
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: fmt.Sprintf("Electronic %d", i), Type: "Audio", RunTimeTicks: 2_000_000_000})
		playlist = append(playlist, id)
	}
	for i := range 100 {
		id := fmt.Sprintf("%032x", 0xb000+i)
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: fmt.Sprintf("Old %d", i), Type: "Audio", RunTimeTicks: 2_000_000_000})
		old = append(old, id)
	}
	const plID = "plist00000000000000000000000e1ec"
	h.jf.AddItem(fakejellyfin.Item{ID: plID, Name: "Electronic", Type: "Playlist", Children: playlist})
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-acc",
		Controllable: true, ApplyCommands: false, // the test plays Finamp's reports
		ItemID: old[0], Title: "In My Room", RunTimeTicks: 2_000_000_000, PositionTicks: 600_000_000,
		Queue: queueEntries(old[1:]),
	})

	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	sessionID := func(c *client.Client) string {
		var id string
		waitList(t, c, nil, "phone", func(s snap) bool {
			for _, x := range s.Sessions {
				if x.Player.DeviceID != nil && *x.Player.DeviceID == "iphone-acc" {
					id = x.ID
					return true
				}
			}
			return false
		})
		return id
	}
	ph := sessionID(c)

	// check waits until sessions.queue shows the context with the whole
	// playlist and the expected current index (-1: null), then requires it
	// to stay that way for a few more polls.
	check := func(step string, wantCurrent int, wantExpected ...bool) {
		t.Helper()
		expected := len(wantExpected) > 0 && wantExpected[0]
		ok := func(q model.Queue) bool {
			if q.CurrentExpected != expected {
				return false
			}
			if q.Context == nil || q.Context.ID != plID || q.Context.Name != "Electronic" || q.Source == nil || *q.Source != "context" ||
				len(q.Items) != 10 || !slices.EqualFunc(q.Items, playlist, func(it model.QueueItem, id string) bool { return it.ItemID == id }) {
				return false
			}
			if wantCurrent < 0 {
				return q.CurrentIndex == nil
			}
			return q.CurrentIndex != nil && *q.CurrentIndex == wantCurrent
		}
		var q model.Queue
		deadline := time.Now().Add(wait)
		for q = call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph}); !ok(q); q = call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph}) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: queue context=%+v source=%v items=%d current=%v expected=%v", step, q.Context, q.Source, len(q.Items), q.CurrentIndex, q.CurrentExpected)
			}
			time.Sleep(50 * time.Millisecond)
		}
		for range 4 { // stays (poll interval 500 ms)
			time.Sleep(250 * time.Millisecond)
			if q = call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph}); !ok(q) {
				t.Fatalf("%s: context did not stay: context=%+v current=%v", step, q.Context, q.CurrentIndex)
			}
		}
	}
	restart := func(step string) {
		t.Helper()
		c.Close()
		h.stop()
		h.start()
		c = h.client()
		if got := sessionID(c); got != ph {
			t.Fatalf("%s: session id changed across the restart: %s", step, got)
		}
	}

	res := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph, "command": "play_items", "item_ids": []string{plID}})
	if res.Outcome != model.OutcomeAcknowledged {
		t.Logf("play_items: %s", res.Outcome)
	}
	if got := h.jf.ClientState(phone.SessionID).Received; !strings.HasPrefix(got[len(got)-1], "Play(PlayNow:"+playlist[0]) {
		t.Fatalf("phone received %v", got)
	}
	// The old track is still reported: the playlist's first track is the
	// expected position (marked as such).
	check("after play_items", 0, true)

	// (1) Old track, changed queue snapshot (Finamp's controlled mode).
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.Queue = queueEntries(old[5:60]); c.PositionTicks = 0 })
	check("1: old track, changed snapshot", 0, true)
	restart("1")
	// The expectation is not kept across a restart: the reported (old)
	// track is not in the playlist, so no position.
	check("1: after a restart while pending", -1)

	// (2) The new track, with the stale pre-switch snapshot.
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.ItemID, c.Title = playlist[0], "Electronic 0" })
	check("2: new track, stale snapshot", 0)

	// (3) 90 s of stale reports: the same track and snapshot, advancing
	// positions (compressed in time).
	for i := range 6 {
		h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.PositionTicks = int64(i+1) * 150_000_000 })
		if i == 3 {
			restart("3")
		}
		check(fmt.Sprintf("3: stale report %d", i+1), 0)
	}

	// (4) A fresh snapshot in playlist order (Finamp's next track change).
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) {
		c.ItemID, c.Title, c.Queue, c.PositionTicks = playlist[1], "Electronic 1", queueEntries(playlist[2:]), 0
	})
	check("4: fresh in-order snapshot", 1)

	// The context is in the state file, so a third restart keeps it too.
	data, err := os.ReadFile(h.paths.StateFile)
	if err != nil || !strings.Contains(string(data), `"queue_contexts"`) || !strings.Contains(string(data), plID) {
		t.Fatalf("state file: %v %s", err, data)
	}
	restart("4")
	check("4: after a restart", 1)
}
