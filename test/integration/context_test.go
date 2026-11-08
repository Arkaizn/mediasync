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

// TestRealJellyfinQueueContextAndJumps checks against a real server:
//   - a playlist started by mediasync becomes the session's queue context
//     (also on 12.x, which exposes no queue during playback): the whole
//     playlist, with jumps that resend it with PlayNow and a start index;
//   - on servers that expose the reported queue (10.10), a short forward
//     queue_jump is sent as NextTrack commands.
func TestRealJellyfinQueueContextAndJumps(t *testing.T) {
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
	login := rpc[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "it", "username": env.user, "password": env.password})
	version := login["server_version"].(string)

	phone := newSimClient(t, env, "Finamp", "IT Phone Context", []string{t1, t2, t3}, true)
	ps := waitSession(t, c, "phone", func(s model.Session) bool {
		return s.Player.DeviceID != nil && *s.Player.DeviceID == phone.deviceID && s.Controllable
	})

	// Step jumps where the server exposes the client's queue (10.10).
	if strings.HasPrefix(version, "10.") {
		ps = waitSession(t, c, "queue_jump", func(s model.Session) bool { return s.ID == ps.ID && s.Has(model.CapQueueJump) })
		n := len(phone.Received())
		r := command(t, c, ps.ID, map[string]any{"command": "queue_jump", "index": 2})
		if got := phone.Received()[n:]; !slices.Equal(got, []string{"NextTrack", "NextTrack"}) {
			t.Errorf("forward jump sent %v", got)
		}
		if r.Outcome != model.OutcomeVerified || *r.Session.Track.TrackID != t3 {
			t.Errorf("step jump: %+v", r)
		}
	}

	// The playlist "Road Trip" (Track 3, Track 1) started from mediasync.
	pls := rpc[map[string][]model.Playlist](t, c, "library.playlists", map[string]any{"server": "it"})["playlists"]
	var road string
	for _, p := range pls {
		if p.Name == "Road Trip" {
			road = p.ID
		}
	}
	if road == "" {
		t.Fatal("no Road Trip playlist")
	}
	r := command(t, c, ps.ID, map[string]any{"command": "play_items", "item_ids": []string{road}})
	if r.Outcome != model.OutcomeVerified {
		t.Errorf("play_items: %+v", r)
	}
	ps = waitSession(t, c, "context queue", func(s model.Session) bool {
		return s.ID == ps.ID && s.Has(model.CapQueue) && s.Has(model.CapQueueJump) && *s.Track.TrackID == t3
	})
	var q model.Queue
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		q = rpc[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ps.ID})
		if q.CurrentIndex != nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if q.Context == nil || q.Context.Type != "playlist" || q.Context.Name != "Road Trip" || q.Context.Shuffled ||
		q.Source == nil || *q.Source != "context" || len(q.Items) != 2 || q.Items[0].ItemID != t3 || q.Items[1].ItemID != t1 ||
		q.CurrentIndex == nil || *q.CurrentIndex != 0 {
		t.Fatalf("context queue on %s: %+v", version, q)
	}

	// Jump inside the playlist: the whole playlist again with the start index.
	r = command(t, c, ps.ID, map[string]any{"command": "queue_jump", "index": 1})
	if got := phone.Received(); got[len(got)-1] != "Play:PlayNow:"+t3+","+t1 {
		t.Errorf("context jump sent %v", got[len(got)-1])
	}
	if r.Outcome != model.OutcomeVerified || *r.Session.Track.TrackID != t1 {
		t.Errorf("context jump: %+v", r)
	}
	if q = rpc[model.Queue](t, c, "sessions.queue", map[string]any{"session_id": ps.ID}); q.Context == nil || len(q.Items) != 2 {
		t.Errorf("the context must stay after a jump: %+v", q)
	}
}
