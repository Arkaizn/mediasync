package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/secrets"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func entries(ids []string) []fakejellyfin.QueueEntry {
	var out []fakejellyfin.QueueEntry
	for _, id := range ids {
		out = append(out, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
	}
	return out
}

// TestQueueContextSurvivesFinampSwitch replays what Finamp 1.x on Jellyfin
// 12.4.0 reports after play_items "now" of a playlist:
//  1. the old track, with a changed queue snapshot (controlled mode);
//  2. the new track, with the old pre-switch snapshot (12.x keeps it until
//     the next stop report);
//  3. a fresh snapshot in playlist order.
//
// The context must survive all of it and end only on a real departure.
func TestQueueContextSurvivesFinampSwitch(t *testing.T) {
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true) // expose the reported snapshots as the server keeps them
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 10)
	var old []string // the phone's previous 100-track queue
	for i := range 100 {
		id := fmt.Sprintf("%032x", 0xe000+i)
		e.srv.AddItem(fakejellyfin.Item{ID: id, Name: id, Type: "Audio", RunTimeTicks: 1_800_000_000})
		old = append(old, id)
	}
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.ItemID, c.Title, c.ApplyCommands = t1, "In My Room", false // we drive the reports
		c.Queue = entries(old)
	})
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) })
	ctx := context.Background()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
		t.Fatal(err)
	}
	brief := func(q model.Queue) string {
		cur := "null"
		if q.CurrentIndex != nil {
			cur = fmt.Sprint(*q.CurrentIndex)
		}
		return fmt.Sprintf("context=%+v current=%s items=%d", q.Context, cur, len(q.Items))
	}
	queueNow := func() model.Queue {
		q, err := e.prov.Queue(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	report := func(f func(c *fakejellyfin.Client)) {
		t.Helper()
		e.srv.Update(ph.SessionID, f)
		// Two polls, so the provider has certainly seen the report.
		for range 2 {
			e.prov.Refresh()
			time.Sleep(120 * time.Millisecond)
		}
	}

	// 1. Old track, changed snapshot: still pending, context shown.
	report(func(c *fakejellyfin.Client) { c.Queue = entries(old[3:50]) })
	if q := queueNow(); q.Context == nil || q.Context.ID != plID || q.CurrentIndex != nil || len(q.Items) != 10 || *q.Source != "context" {
		t.Fatalf("1: context lost while the client still reports the old track: %s", brief(q))
	}
	// 2. New track, but the snapshot from before the switch.
	report(func(c *fakejellyfin.Client) {
		c.ItemID, c.Title, c.Queue = tracks[0], "Song 000", entries(old[3:50])
	})
	if q := queueNow(); q.Context == nil || q.CurrentIndex == nil || *q.CurrentIndex != 0 {
		t.Fatalf("2: the stale snapshot ended the context: %s", brief(q))
	}
	// 3. Next track with a fresh snapshot in playlist order.
	report(func(c *fakejellyfin.Client) {
		c.ItemID, c.Title, c.Queue = tracks[1], "Song 001", entries(tracks[2:])
	})
	if q := queueNow(); q.Context == nil || *q.CurrentIndex != 1 {
		t.Fatalf("3: %s", brief(q))
	}
	// 4. A real departure: a fresh snapshot that does not follow the order.
	report(func(c *fakejellyfin.Client) {
		c.Queue = entries(append([]string{t2}, tracks[2:]...))
	})
	if q := queueNow(); q.Context != nil {
		t.Errorf("4: a post-switch snapshot out of order must end the context: %+v", q.Context)
	}
}

func TestQueueContextEndsOnRealDeparture(t *testing.T) {
	e := stepEnv(t, 15)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 5)
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.ApplyCommands = false })
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		steps []func(c *fakejellyfin.Client)
	}{
		// While pending: a track that is neither the old one nor in the list.
		{"pending, elsewhere", []func(c *fakejellyfin.Client){func(c *fakejellyfin.Client) { c.ItemID = t2 }}},
		// Confirmed, then a track outside the list.
		{"confirmed, elsewhere", []func(c *fakejellyfin.Client){
			func(c *fakejellyfin.Client) { c.ItemID = tracks[0] },
			func(c *fakejellyfin.Client) { c.ItemID = t2 },
		}},
	} {
		e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.ItemID = t1 })
		e.prov.Refresh()
		s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == t1 })
		if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
			t.Fatal(err)
		}
		for i, step := range tc.steps {
			e.srv.Update(ph.SessionID, step)
			e.prov.Refresh()
			time.Sleep(150 * time.Millisecond)
			if q, _ := e.prov.Queue(ctx, s.ID); i < len(tc.steps)-1 && q.Context == nil {
				t.Fatalf("%s: ended too early at step %d", tc.name, i)
			}
		}
		e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
			q, _ := e.prov.Queue(ctx, x.ID)
			return q.Context == nil
		})
	}
}

// syncBuf is a goroutine-safe log buffer.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// The pending state is time-boxed; endings are logged at info level with
// the reason.
func TestQueueContextPendingTimeoutAndLogging(t *testing.T) {
	logs := &syncBuf{}
	p, err := New(Config{Name: "home", URL: "http://127.0.0.1:1", DeviceID: "d", Credentials: secrets.NewMemory()},
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	old := SessionInfo{NowPlayingItem: &BaseItem{ID: "old"}}
	start := func(ids ...string) {
		p.startContextLocked("s", old, model.QueueContext{Type: "playlist", ID: "pl", Name: "Electronic"}, ids)
	}

	// The old track for nine minutes: still waiting.
	start("a", "b")
	now = now.Add(9 * time.Minute)
	p.updateContextLocked("s", old)
	if c := p.contexts["s"]; c == nil || !c.pending {
		t.Fatal("ended before the time box")
	}
	// Past the time box: ended, with the reason in the log.
	now = now.Add(2 * time.Minute)
	p.updateContextLocked("s", old)
	if p.contexts["s"] != nil {
		t.Fatal("pending context outlived the time box")
	}
	if l := logs.String(); !strings.Contains(l, "level=INFO") || !strings.Contains(l, `msg="queue context ended"`) ||
		!strings.Contains(l, "context=Electronic") || !strings.Contains(l, "reason=\"timeout") {
		t.Errorf("log: %s", l)
	}

	// If the old track is itself part of the context, waiting out the time
	// box confirms it instead.
	start("old", "b")
	now = now.Add(11 * time.Minute)
	p.updateContextLocked("s", old)
	if c := p.contexts["s"]; c == nil || c.pending || c.current != 0 {
		t.Errorf("old track in the context: %+v", c)
	}
	for _, reason := range []string{"track outside the context"} {
		p.updateContextLocked("s", SessionInfo{NowPlayingItem: &BaseItem{ID: "zzz"}})
		if !strings.Contains(logs.String(), reason) {
			t.Errorf("no %q in the log", reason)
		}
	}
}

// After a step jump, a stale-reporting client may report a changed queue
// snapshot with the old track: the remembered position no longer matches
// the list, so the next jump must not count steps from either position.
func TestStepJumpWithChangedSnapshotUsesPlayNow(t *testing.T) {
	e := stepEnv(t, 15)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	_, tracks := bigPlaylist(e.srv, 12)
	ph := longQueuePhone(e, tracks, true)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })
	jump(t, e, s.ID, 2) // two NextTrack
	// Same (old) track, different snapshot.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Queue = entries(tracks[4:]) })
	e.prov.Refresh()
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
		q, _ := e.prov.Queue(context.Background(), x.ID)
		return len(q.Items) > 1 && q.Items[1].ItemID == tracks[4]
	})
	jump(t, e, s.ID, 3)
	got := received(e, ph)
	if len(got) != 3 || !strings.HasPrefix(got[2], "Play(PlayNow:") || !strings.HasSuffix(got[2], "@3)") {
		t.Errorf("want two NextTrack, then PlayNow: %v", got)
	}
}

func TestQueueContextsSaveAndRestore(t *testing.T) {
	var saved []string
	var mu sync.Mutex
	mk := func(state []byte) *Provider {
		p, err := New(Config{Name: "home", URL: "http://127.0.0.1:1", DeviceID: "d", Credentials: secrets.NewMemory(),
			SavedState: state, SaveState: func(b json.RawMessage) { mu.Lock(); saved = append(saved, string(b)); mu.Unlock() }}, quiet())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := mk(nil)
	old := SessionInfo{NowPlayingItem: &BaseItem{ID: "old"}}
	p.mu.Lock()
	p.startContextLocked("waiting", old, model.QueueContext{Type: "playlist", ID: "pl1", Name: "Electronic"}, []string{"a", "b", "c"})
	p.startContextLocked("playing", old, model.QueueContext{Type: "album", ID: "al1", Name: "Album X"}, []string{"x", "y"})
	p.updateContextLocked("playing", SessionInfo{NowPlayingItem: &BaseItem{ID: "y"}})
	p.startContextLocked("gone", old, model.QueueContext{Type: "playlist", ID: "pl2", Name: "Gone"}, []string{"g"})
	p.mu.Unlock()
	p.persistContexts()
	p.persistContexts() // unchanged: not saved again
	if len(saved) != 1 || !strings.Contains(saved[0], `"Electronic"`) || !strings.Contains(saved[0], `"base_track":"old"`) {
		t.Fatalf("saved %v", saved)
	}

	// After a restart.
	q := mk([]byte(saved[0]))
	if len(q.contexts) != 3 {
		t.Fatalf("restored %d contexts", len(q.contexts))
	}
	for id, c := range q.contexts {
		if !c.pending {
			t.Errorf("%s: a restored context must be re-validated (pending)", id)
		}
	}
	if c := q.contexts["playing"]; c.current != 1 || c.baseTrack != "" {
		t.Errorf("confirmed context restored as %+v", c)
	}
	// The waiting one keeps waiting while the old track is reported.
	q.updateContextLocked("waiting", old)
	if c := q.contexts["waiting"]; c == nil || !c.pending {
		t.Error("restored waiting context ended on the old track")
	}
	// The confirmed one: confirmed again by a context track …
	q.updateContextLocked("playing", SessionInfo{NowPlayingItem: &BaseItem{ID: "y"}})
	if c := q.contexts["playing"]; c == nil || c.pending || c.current != 1 {
		t.Errorf("restored context not confirmed: %+v", c)
	}
	// … and a restored confirmed context ends on any other track.
	r := mk([]byte(saved[0]))
	r.updateContextLocked("playing", SessionInfo{NowPlayingItem: &BaseItem{ID: "old"}})
	if r.contexts["playing"] != nil {
		t.Error("a restored confirmed context survived a track outside it")
	}
	// Garbage is ignored, not fatal.
	if g := mk([]byte(`{not json`)); len(g.contexts) != 0 {
		t.Error("garbage restored")
	}
}

func TestRestoredContextOfAbsentSessionIsDropped(t *testing.T) {
	e := stepEnv(t, 15)
	library(e.srv)
	queuePhone(e.srv)
	id := e.prov.SessionID(e.srv.ServerID, "iphone-q", "Finamp")
	state := fmt.Sprintf(`{"queue_contexts":{%q:{"type":"playlist","id":%q,"name":"Road Trip","item_ids":[%q,%q],"current":0},
		"jellyfin:home:0000000000000000":{"type":"playlist","id":"x","name":"Gone","item_ids":["a"],"current":0}}}`, id, pl, t3, t1)
	p, err := New(Config{Name: "home", URL: e.srv.URL, DeviceID: "mediasync-test-device", DeviceName: "mydesk", Version: "0.1.0",
		PollInterval: 50 * time.Millisecond, RequestTimeout: time.Second, MaxBackoff: 200 * time.Millisecond,
		Credentials: e.creds, JumpMaxSteps: 15, SavedState: json.RawMessage(state)}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	e.prov = p
	e.login(t)
	e.run(t)
	// The phone plays t1 (the playlist's second track): confirmed at index 1.
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) })
	q, _ := e.prov.Queue(context.Background(), s.ID)
	if q.Context == nil || q.Context.Name != "Road Trip" || q.CurrentIndex == nil || *q.CurrentIndex != 1 {
		t.Errorf("restored context %+v current %v", q.Context, q.CurrentIndex)
	}
	e.prov.mu.Lock()
	defer e.prov.mu.Unlock()
	if len(e.prov.contexts) != 1 {
		t.Errorf("the context of the absent session was not dropped: %d contexts", len(e.prov.contexts))
	}
}
