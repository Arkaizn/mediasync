package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func TestEndToEndQueueContext(t *testing.T) {
	h := newHarness(t, true, nil) // Jellyfin 12.x: the server exposes no queue
	ids := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa2", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa3"}
	for i, id := range ids {
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: "Track " + string(rune('1'+i)), Type: "Audio", RunTimeTicks: 1_800_000_000})
	}
	h.jf.AddItem(fakejellyfin.Item{ID: "plist000000000000000000000000001", Name: "Road Trip", Type: "Playlist", Children: []string{ids[2], ids[0], ids[1]}})
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-c",
		Controllable: true, ApplyCommands: true, ItemID: ids[0], Title: "Track 1", RunTimeTicks: 1_800_000_000,
	})
	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	var ph model.Session
	waitList(t, c, nil, "phone", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.Player.DeviceID != nil && *x.Player.DeviceID == "iphone-c" {
				ph = x
				return true
			}
		}
		return false
	})
	call[map[string]any](t, c, "subscribe", nil)

	// Starting a playlist from mediasync: the context starts (queue.changed)
	// and sessions.queue shows the whole playlist.
	res := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "play_items", "item_ids": []string{"plist000000000000000000000000001"}})
	if res.Outcome != model.OutcomeVerified {
		t.Errorf("play_items %+v", res)
	}
	nextEvent(t, c, "queue.changed (context started)", func(e event) bool { return e.Type == "queue.changed" && e.SessionID == ph.ID })
	waitList(t, c, nil, "queue capability", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.ID == ph.ID {
				return x.Has(model.CapQueue) && x.Has(model.CapQueueJump)
			}
		}
		return false
	})
	raw := call[json.RawMessage](t, c, "sessions.queue", map[string]any{"session_id": ph.ID})
	for _, want := range []string{`"context":{"type":"playlist","id":"plist000000000000000000000000001","name":"Road Trip","shuffled":false}`, `"source":"context"`, `"current_index":0`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("sessions.queue lacks %s: %s", want, raw)
		}
	}
	var q model.Queue
	_ = json.Unmarshal(raw, &q)
	if len(q.Items) != 3 || q.Items[1].ItemID != ids[0] {
		t.Fatalf("items %+v", q.Items)
	}

	// Jump inside the playlist: the whole playlist again, verified.
	res = call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": ph.ID, "command": "queue_jump", "index": 2})
	if res.Outcome != model.OutcomeVerified || *res.Session.Track.TrackID != ids[1] {
		t.Errorf("queue_jump %+v", res)
	}
	got := h.jf.ClientState(phone.SessionID).Received
	if got[len(got)-1] != "Play(PlayNow:"+ids[2]+","+ids[0]+","+ids[1]+"@2)" {
		t.Errorf("jump sent %v", got)
	}

	// The client plays something else: the context ends (queue.changed).
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.ItemID, c.Title = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb9", "Elsewhere" })
	nextEvent(t, c, "queue.changed (context ended)", func(e event) bool { return e.Type == "queue.changed" && e.SessionID == ph.ID })
	waitList(t, c, nil, "no queue", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.ID == ph.ID {
				return !x.Has(model.CapQueue)
			}
		}
		return false
	})
	if q := call[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ph.ID}); q.Available || q.Context != nil {
		t.Errorf("after the context ended: %+v", q)
	}
}
