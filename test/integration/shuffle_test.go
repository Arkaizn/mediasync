//go:build integration

package integration

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/model"
)

// TestRealJellyfinPlayItemsShuffle checks shuffled play_items against a real
// server: mediasync expands an album and an artist itself (with the same
// tracks the server would play) and sends a permutation of them.
func TestRealJellyfinPlayItemsShuffle(t *testing.T) {
	env := jellyfinEnv(t)
	byName := audioItemsByName(t, env)
	t1, t2, t3 := byName["Track 1"], byName["Track 2"], byName["Track 3"]
	if t1 == "" || t2 == "" || t3 == "" {
		t.Skip("needs the test library from scripts/jellyfin-test-server.sh (Track 1–3)")
	}
	cfg := config.Default()
	cfg.Local.Enabled = false
	cfg.Jellyfin.PollInterval = config.Duration(time.Second)
	cfg.Jellyfin.Servers = []config.JellyfinServer{{Name: "it", URL: env.url}}
	cfg.Commands.RemoteVerifyTimeout = config.Duration(8 * time.Second)
	c := startService(t, cfg, nil)
	rpc[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "it", "username": env.user, "password": env.password})

	phone := newSimClient(t, env, "Finamp", "IT Phone Shuffle", []string{t1, t2, t3}, true)
	ps := waitSession(t, c, "phone", func(s model.Session) bool {
		return s.Player.DeviceID != nil && *s.Player.DeviceID == phone.deviceID && s.Has(model.CapPlayItemsShuffle)
	})

	// The album and artist ids of Track 1.
	var res struct {
		Items []struct {
			AlbumID     string `json:"AlbumId"`
			ArtistItems []struct {
				ID string `json:"Id"`
			}
		}
	}
	phone.do("GET", "/Items?ids="+t1+"&fields=ArtistItems", nil, &res)
	if len(res.Items) != 1 || res.Items[0].AlbumID == "" || len(res.Items[0].ArtistItems) == 0 {
		t.Fatalf("no album/artist for Track 1: %+v", res)
	}
	all := []string{t1, t2, t3}
	slices.Sort(all)
	for what, id := range map[string]string{"album": res.Items[0].AlbumID, "artist": res.Items[0].ArtistItems[0].ID} {
		r := command(t, c, ps.ID, map[string]any{"command": "play_items", "item_ids": []string{id}, "shuffle": true})
		got := phone.Received()
		last := got[len(got)-1]
		ids, ok := strings.CutPrefix(last, "Play:PlayNow:")
		sent := strings.Split(ids, ",")
		sorted := slices.Clone(sent)
		slices.Sort(sorted)
		if !ok || !slices.Equal(sorted, all) {
			t.Errorf("%s: phone received %q, want a permutation of Track 1–3", what, last)
		}
		t.Logf("%s %s: sent %v, outcome %s", what, id, sent, r.Outcome)
		if r.Outcome != model.OutcomeVerified {
			t.Errorf("%s: %+v", what, r)
		}
	}
}

// TestRealJellyfinSetShuffle checks set_shuffle against a real server: the
// GeneralCommand SetShuffleQueue reaches a client that declares it, and the
// PlaybackOrder it reports back verifies the command.
func TestRealJellyfinSetShuffle(t *testing.T) {
	env := jellyfinEnv(t)
	ids := audioItems(t, env)
	cfg := config.Default()
	cfg.Local.Enabled = false
	cfg.Jellyfin.PollInterval = config.Duration(time.Second)
	cfg.Jellyfin.Servers = []config.JellyfinServer{{Name: "it", URL: env.url}}
	cfg.Commands.RemoteVerifyTimeout = config.Duration(8 * time.Second)
	c := startService(t, cfg, nil)
	rpc[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "it", "username": env.user, "password": env.password})

	phone := newSimClient(t, env, "Finamp", "IT Phone SetShuffle", ids, true)
	ps := waitSession(t, c, "phone", func(s model.Session) bool {
		return s.Player.DeviceID != nil && *s.Player.DeviceID == phone.deviceID && s.Has(model.CapShuffle)
	})
	if ps.Playback.Shuffle == nil || *ps.Playback.Shuffle {
		t.Fatalf("initial shuffle %v", ps.Playback.Shuffle)
	}
	for _, want := range []bool{true, false} {
		r := command(t, c, ps.ID, map[string]any{"command": "set_shuffle", "shuffle": want})
		if r.Outcome != model.OutcomeVerified || r.Session == nil || *r.Session.Playback.Shuffle != want {
			t.Errorf("set_shuffle %v: %+v", want, r)
		}
	}
	got := phone.Received()
	if !slices.Equal(got[len(got)-2:], []string{"SetShuffleQueue(Shuffle)", "SetShuffleQueue(Sorted)"}) {
		t.Errorf("phone received %v", got)
	}
}
