package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func TestEndToEndPlayItemsShuffle(t *testing.T) {
	h := newHarness(t, true, nil)
	var tracks []string
	for i := range 12 {
		id := fmt.Sprintf("%032x", 0xd000+i)
		h.jf.AddItem(fakejellyfin.Item{ID: id, Name: fmt.Sprintf("Song %d", i), Type: "Audio", RunTimeTicks: 1_800_000_000})
		tracks = append(tracks, id)
	}
	h.jf.AddItem(fakejellyfin.Item{ID: "plist0000000000000000000000000s1", Name: "Mix", Type: "Playlist", Children: tracks})
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: h.jf.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-s",
		Controllable: true, ApplyCommands: true, ItemID: tracks[0], Title: "Song 0", RunTimeTicks: 1_800_000_000,
	})
	c := h.client()
	call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	var ph model.Session
	waitList(t, c, nil, "phone", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.Player.DeviceID != nil && *x.Player.DeviceID == "iphone-s" {
				ph = x
				return true
			}
		}
		return false
	})
	if !ph.Has(model.CapPlayItems) || !ph.Has(model.CapPlayItemsShuffle) {
		t.Fatalf("caps %v", ph.Capabilities)
	}

	res := call[model.CommandResult](t, c, "player.command", map[string]any{
		"session_id": ph.ID, "command": "play_items", "item_ids": []string{"plist0000000000000000000000000s1"}, "shuffle": true})
	if res.Outcome != model.OutcomeVerified {
		t.Errorf("shuffled play_items: %+v", res)
	}
	// The fake records "Play(PlayNow:id,id,…@start)".
	var sent []string
	for _, r := range h.jf.ClientState(phone.SessionID).Received {
		if rest, ok := strings.CutPrefix(r, "Play(PlayNow:"); ok {
			ids, _, _ := strings.Cut(rest, "@")
			sent = strings.Split(ids, ",")
		}
	}
	a, b := slices.Clone(sent), slices.Clone(tracks)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		t.Errorf("phone received %v, want a permutation of the playlist", sent)
	}

	// "shuffle" belongs to play_items only; unknown fields stay errors.
	for _, params := range []map[string]any{
		{"session_id": ph.ID, "command": "pause", "shuffle": true},
		{"session_id": ph.ID, "command": "queue_jump", "index": 0, "shuffle": false},
		{"session_id": ph.ID, "command": "play_items", "item_ids": []string{tracks[0]}, "shufle": true},
	} {
		if e := callErr(t, c, "player.command", params); e == nil || e.Code != "invalid_argument" {
			t.Errorf("%v: want invalid_argument, got %+v", params, e)
		}
	}
}

func TestEndToEndSetShuffleOnLocalPlayer(t *testing.T) {
	h := newHarness(t, false, nil)
	h.player(feishinOpts())
	c := h.client()
	waitList(t, c, nil, "feishin with shuffle", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.ID == "mpris:Feishin" && x.Has(model.CapShuffle) {
				return true
			}
		}
		return false
	})
	for _, want := range []bool{true, false} {
		res := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": "mpris:Feishin", "command": "set_shuffle", "shuffle": want})
		if res.Outcome != model.OutcomeVerified || res.Session == nil || res.Session.Playback.Shuffle == nil || *res.Session.Playback.Shuffle != want {
			t.Errorf("set_shuffle %v: %+v", want, res)
		}
	}
	if e := callErr(t, c, "player.command", map[string]any{"session_id": "mpris:Feishin", "command": "set_shuffle"}); e == nil || e.Code != "invalid_argument" {
		t.Errorf("set_shuffle without shuffle: %+v", e)
	}
}
