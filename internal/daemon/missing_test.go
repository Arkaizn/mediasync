package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/client"
	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

// collect returns the events received within d.
func collect(c *client.Client, d time.Duration) []event {
	var out []event
	deadline := time.After(d)
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				return out
			}
			var ev event
			if json.Unmarshal(n.Params, &ev) == nil {
				out = append(out, ev)
			}
		case <-deadline:
			return out
		}
	}
}

// A Jellyfin session that drops out of GET /Sessions for a poll (Finamp
// during some remote commands) must not be removed and re-added, nor lose
// an explicit selection; one that stays away longer than
// remove_missing_after is removed once.
func TestEndToEndMissingSessionBlip(t *testing.T) {
	h := newHarness(t, true, func(c *config.Config) {
		c.Jellyfin.RemoveMissingAfter = config.Duration(2 * time.Second) // 4 polls at 500 ms
	})
	h.jf.AddItem(fakejellyfin.Item{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", Name: "Track 1", Type: "Audio", RunTimeTicks: 1_800_000_000})
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-blip",
		Controllable: true, ApplyCommands: true, ItemID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", Title: "Track 1", RunTimeTicks: 1_800_000_000,
	})
	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	var ph model.Session
	waitList(t, c, nil, "phone", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.Player.DeviceID != nil && *x.Player.DeviceID == "iphone-blip" {
				ph = x
				return true
			}
		}
		return false
	})
	call[map[string]any](t, c, "sessions.select", map[string]any{"session_id": ph.ID})
	call[map[string]any](t, c, "subscribe", nil)
	collect(c, 300*time.Millisecond) // settle

	// One missed poll and a bit (the server's list skips the session).
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.Hidden = true })
	time.Sleep(800 * time.Millisecond)
	if l := call[snap](t, c, "sessions.list", nil); len(l.Sessions) != 1 || l.SelectedID == nil || *l.SelectedID != ph.ID {
		t.Errorf("during the blip: %d sessions, selected %v", len(l.Sessions), l.SelectedID)
	}
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.Hidden = false })
	for _, ev := range collect(c, 1500*time.Millisecond) {
		if ev.Type == "session.removed" || ev.Type == "session.added" || ev.Type == "selection.changed" {
			t.Errorf("blip produced %s %s", ev.Type, ev.SessionID)
		}
	}
	sel := call[map[string]any](t, c, "sessions.selected", nil)
	if s, _ := sel["session"].(map[string]any); s == nil || s["id"] != ph.ID || sel["reason"] != "user" {
		t.Errorf("explicit selection lost: %v", sel)
	}

	// Gone for longer than the grace: removed once.
	h.jf.Update(phone.SessionID, func(c *fakejellyfin.Client) { c.Hidden = true })
	evs := collect(c, 4*time.Second)
	removed := 0
	for _, ev := range evs {
		if ev.Type == "session.removed" && ev.SessionID == ph.ID {
			removed++
		}
		if ev.Type == "session.added" {
			t.Errorf("unexpected session.added")
		}
	}
	if removed != 1 {
		t.Errorf("session.removed %d times: %+v", removed, evs)
	}
}
