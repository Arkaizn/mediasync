package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
	pt "github.com/arkaizn/mediasync/internal/provider/providertest"
)

func newEngine(t *testing.T, mutate func(*Options)) *Engine {
	t.Helper()
	opts := Options{
		Settings:            DefaultSettings(),
		Host:                HostInfo{Hostname: "mydesk", Addresses: []string{"192.168.1.10"}},
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		VerifyTimeout:       300 * time.Millisecond,
		RemoteVerifyTimeout: 300 * time.Millisecond,
		CommandTimeout:      time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	e := New(opts)
	t.Cleanup(e.Close)
	return e
}

func addFake(t *testing.T, e *Engine, instance, kind string, origin model.Origin, sessions ...model.Session) *pt.Fake {
	t.Helper()
	f := pt.NewFake(instance, kind, origin)
	for _, s := range sessions {
		f.Set(s)
	}
	e.AddProvider(f)
	f.WaitReady()
	waitUntil(t, "provider sessions", func() bool {
		for _, s := range sessions {
			if _, ok := e.Session(s.ID); !ok {
				return false
			}
		}
		return true
	})
	return f
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ids(snap Snapshot) []string {
	out := []string{}
	for _, s := range snap.Sessions {
		out = append(out, s.ID)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func remoteSession(id, title, device string, state model.PlaybackState, caps ...model.Capability) model.Session {
	s := pt.Session(id, model.OriginRemote, title, state, caps...)
	s.Player.Device = model.Ptr(device)
	s.Player.Client = model.Ptr("Finamp")
	s.Player.DeviceID = model.Ptr("dev-" + device)
	return s
}

func TestModesAndFilters(t *testing.T) {
	e := newEngine(t, nil)
	addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:feishin", model.OriginLocal, "Song A", model.StatePlaying, pt.AllCaps...),
		pt.Session("mpris:vlc", model.OriginLocal, "Song B", model.StatePaused, pt.AllCaps...))
	addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote,
		remoteSession("jf:phone", "Song C", "Pixel 7", model.StatePlaying, model.CapPlay, model.CapPause),
		remoteSession("jf:tv", "Song D", "Living Room TV", model.StatePaused))

	all := e.Sessions(Filter{})
	if !eq(ids(all), []string{"mpris:feishin", "jf:phone", "mpris:vlc", "jf:tv"}) {
		t.Fatalf("combined order: %v", ids(all))
	}
	if *all.SelectedID != "mpris:feishin" || all.Reason != "auto" {
		t.Errorf("selected %v %s", *all.SelectedID, all.Reason)
	}
	// Kinds.
	kinds := map[string]model.SessionKind{}
	for _, s := range all.Sessions {
		kinds[s.ID] = s.Kind
	}
	if kinds["mpris:vlc"] != model.KindLocal || kinds["jf:phone"] != model.KindRemoteControllable || kinds["jf:tv"] != model.KindRemoteViewOnly {
		t.Errorf("kinds %v", kinds)
	}

	st := e.Settings()
	st.Mode = ModeLocalOnly
	if err := e.SetSettings(st); err != nil {
		t.Fatal(err)
	}
	if got := ids(e.Sessions(Filter{})); !eq(got, []string{"mpris:feishin", "mpris:vlc"}) {
		t.Errorf("local only: %v", got)
	}
	st.Mode = ModeRemoteOnly
	_ = e.SetSettings(st)
	snap := e.Sessions(Filter{})
	if got := ids(snap); !eq(got, []string{"jf:phone", "jf:tv"}) {
		t.Errorf("remote only: %v", got)
	}
	if *snap.SelectedID != "jf:phone" {
		t.Errorf("remote-only selection %v", *snap.SelectedID)
	}
	// Hidden sessions are available on request, annotated.
	h := e.Sessions(Filter{IncludeHidden: true, SessionIDs: []string{"mpris:vlc"}})
	if len(h.Sessions) != 1 || h.Sessions[0].Hidden != "mode" {
		t.Errorf("hidden: %+v", h.Sessions)
	}

	st.Mode = ModeCombined
	st.ShowViewOnly = false
	_ = e.SetSettings(st)
	if got := ids(e.Sessions(Filter{})); !eq(got, []string{"mpris:feishin", "jf:phone", "mpris:vlc"}) {
		t.Errorf("hide view-only: %v", got)
	}

	// Filters.
	if got := ids(e.Sessions(Filter{Providers: []string{"jellyfin"}})); !eq(got, []string{"jf:phone"}) {
		t.Errorf("provider filter: %v", got)
	}
	if got := ids(e.Sessions(Filter{Origins: []model.Origin{model.OriginLocal}})); !eq(got, []string{"mpris:feishin", "mpris:vlc"}) {
		t.Errorf("origin filter: %v", got)
	}
	if got := ids(e.Sessions(Filter{Devices: []string{"pixel 7"}})); !eq(got, []string{"jf:phone"}) {
		t.Errorf("device filter: %v", got)
	}
	if got := ids(e.Sessions(Filter{Players: []string{"mpris:vlc"}})); !eq(got, []string{"mpris:vlc"}) {
		t.Errorf("player filter: %v", got)
	}
	if got := ids(e.Sessions(Filter{SessionIDs: []string{"jf:phone", "nope"}})); !eq(got, []string{"jf:phone"}) {
		t.Errorf("session filter: %v", got)
	}
}

func TestSelectionRules(t *testing.T) {
	e := newEngine(t, func(o *Options) { o.SelectionGrace = 200 * time.Millisecond })
	local := addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:a", model.OriginLocal, "A", model.StatePaused, pt.AllCaps...))
	time.Sleep(2 * time.Millisecond)
	local.Set(pt.Session("mpris:b", model.OriginLocal, "B", model.StatePaused, pt.AllCaps...))
	remote := addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote,
		remoteSession("jf:phone", "C", "Pixel", model.StatePaused, model.CapPlay))

	sel := func() string {
		s, _ := e.Selected()
		if s == nil {
			return ""
		}
		return s.ID
	}
	// All paused, no observed activity, same origin: ID decides.
	if got := sel(); got != "mpris:a" {
		t.Errorf("tie-break: %s", got)
	}
	// Playing beats everything.
	s := pt.Session("mpris:a", model.OriginLocal, "A", model.StatePlaying, pt.AllCaps...)
	local.Set(s)
	waitUntil(t, "a selected", func() bool { return sel() == "mpris:a" })

	// Playback starting later on the phone takes over (most recent activity).
	time.Sleep(2 * time.Millisecond)
	r := remoteSession("jf:phone", "C", "Pixel", model.StatePlaying, model.CapPlay)
	remote.Set(r)
	waitUntil(t, "phone selected", func() bool { return sel() == "jf:phone" })
	st := e.Settings()
	// Preferred player wins among equally playing sessions.
	st.PreferredPlayer = "mpris:a"
	_ = e.SetSettings(st)
	if got := sel(); got != "mpris:a" {
		t.Errorf("preferred: %s", got)
	}

	// Explicit selection sticks even when another session is playing.
	if err := e.Select("mpris:b"); err != nil {
		t.Fatal(err)
	}
	if s, reason := e.Selected(); s.ID != "mpris:b" || reason != "user" {
		t.Errorf("explicit: %v %s", s.ID, reason)
	}
	if err := e.Select("nope"); model.CodeOf(err) != model.ErrSessionNotFound {
		t.Errorf("select missing: %v", err)
	}
	// Explicit session restarts within grace: selection returns to it.
	local.Delete("mpris:b")
	waitUntil(t, "fallback", func() bool { return sel() == "mpris:a" })
	local.Set(pt.Session("mpris:b", model.OriginLocal, "B", model.StatePaused, pt.AllCaps...))
	waitUntil(t, "explicit restored", func() bool { return sel() == "mpris:b" })
	// After the grace period the explicit choice is forgotten.
	local.Delete("mpris:b")
	time.Sleep(300 * time.Millisecond)
	e.Tick()
	local.Set(pt.Session("mpris:b", model.OriginLocal, "B", model.StatePaused, pt.AllCaps...))
	waitUntil(t, "auto after grace", func() bool { return sel() == "mpris:a" })
	if _, reason := e.Selected(); reason != "auto" {
		t.Errorf("reason %s", reason)
	}
	// Back to automatic.
	_ = e.Select("jf:phone")
	_ = e.Select("")
	if _, reason := e.Selected(); reason != "auto" {
		t.Errorf("reason after clear %s", reason)
	}
}

func TestDeterministicOrdering(t *testing.T) {
	// Repeated recomputation with identical inputs yields identical order.
	e := newEngine(t, nil)
	var sessions []model.Session
	for _, id := range []string{"mpris:z", "mpris:m", "mpris:a"} {
		sessions = append(sessions, pt.Session(id, model.OriginLocal, id, model.StatePlaying))
	}
	addFake(t, e, "mpris", "mpris", model.OriginLocal, sessions...)
	first := ids(e.Sessions(Filter{}))
	for i := 0; i < 20; i++ {
		if got := ids(e.Sessions(Filter{})); !eq(got, first) {
			t.Fatalf("order changed: %v vs %v", got, first)
		}
	}
}

func TestDedupSameHost(t *testing.T) {
	e := newEngine(t, nil)
	local := pt.Session("mpris:feishin", model.OriginLocal, "Track 1", model.StatePlaying, pt.AllCaps...)
	local.Track.Artists = []string{"Artist A"}
	local.Track.DurationMS = model.Ptr[int64](180_000)
	local.Track.Album = model.Ptr("Album X")
	addFake(t, e, "mpris", "mpris", model.OriginLocal, local)

	// Feishin's own Jellyfin session on this machine (device = hostname).
	same := remoteSession("jf:feishin", "track 1", "MyDesk", model.StatePlaying, model.CapPlay)
	same.Player.Client = model.Ptr("Feishin")
	same.Track.Artists = []string{"artist a"}
	same.Track.DurationMS = model.Ptr[int64](180_400)
	same.Track.Album = model.Ptr("Wrong album would block")
	same.Track.Album = nil
	// A phone playing the same track elsewhere must NOT be merged.
	phone := remoteSession("jf:phone", "Track 1", "Pixel 7", model.StatePlaying, model.CapPlay)
	phone.Track.Artists = []string{"Artist A"}
	// A session on this host from the loopback address, different track.
	other := remoteSession("jf:web", "Different", "browser", model.StatePlaying)
	other.Player.Address = model.Ptr("127.0.0.1")
	addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote, same, phone, other)

	snap := e.Sessions(Filter{})
	if got := ids(snap); !eq(got, []string{"mpris:feishin", "jf:phone", "jf:web"}) {
		t.Fatalf("visible: %v", got)
	}
	if l := snap.Sessions[0]; len(l.Linked) != 1 || l.Linked[0] != "jf:feishin" {
		t.Errorf("linked: %v", l.Linked)
	}
	// Local metadata untouched by the remote duplicate.
	if l := snap.Sessions[0]; *l.Track.Title != "Track 1" || l.Track.Artists[0] != "Artist A" || l.Origin != model.OriginLocal {
		t.Errorf("local overwritten: %+v", l.Track)
	}
	h := e.Sessions(Filter{IncludeHidden: true, SessionIDs: []string{"jf:feishin"}})
	if len(h.Sessions) != 1 || h.Sessions[0].Hidden != "duplicate" || *h.Sessions[0].DuplicateOf != "mpris:feishin" {
		t.Errorf("duplicate annotation: %+v", h.Sessions)
	}
	if e.Diagnostics()["duplicates"].(map[string]string)["jf:feishin"] != string(DupSameHost) {
		t.Errorf("reason: %v", e.Diagnostics()["duplicates"])
	}

	// Disabling dedup shows both.
	st := e.Settings()
	st.Dedup = false
	_ = e.SetSettings(st)
	if n := len(e.Sessions(Filter{}).Sessions); n != 4 {
		t.Errorf("dedup off: %d", n)
	}
}

func TestDedupContradictingMetadataNotMerged(t *testing.T) {
	host := HostInfo{Hostname: "mydesk"}
	l := pt.Session("mpris:x", model.OriginLocal, "Intro", model.StatePlaying)
	l.Track.Artists = []string{"Band One"}
	r := remoteSession("jf:x", "Intro", "mydesk", model.StatePlaying)
	r.Track.Artists = []string{"Band Two"}
	if _, ok := isDuplicate(DefaultSettings(), host, &l, &r); ok {
		t.Error("different artists merged")
	}
	r.Track.Artists = []string{"band one"}
	l.Track.DurationMS, r.Track.DurationMS = model.Ptr[int64](100_000), model.Ptr[int64](200_000)
	if _, ok := isDuplicate(DefaultSettings(), host, &l, &r); ok {
		t.Error("different durations merged")
	}
	r.Track.DurationMS = model.Ptr[int64](101_000)
	if _, ok := isDuplicate(DefaultSettings(), host, &l, &r); !ok {
		t.Error("expected merge")
	}
	// Unknown titles are never merged.
	l.Track.Title, r.Track.Title = nil, nil
	if _, ok := isDuplicate(DefaultSettings(), host, &l, &r); ok {
		t.Error("unknown titles merged")
	}
}

func TestDedupByLinkAndItemID(t *testing.T) {
	host := HostInfo{Hostname: "mydesk", Addresses: []string{"10.0.0.5"}}
	l := pt.Session("mpris:Feishin", model.OriginLocal, "Song", model.StatePlaying)
	l.Player.BusName = model.Ptr("org.mpris.MediaPlayer2.Feishin")
	r := remoteSession("jf:f", "Song", "laptop-name-differs", model.StatePlaying)
	r.Player.Client = model.Ptr("Feishin")

	st := DefaultSettings()
	if _, ok := isDuplicate(st, host, &l, &r); ok {
		t.Fatal("merged without evidence")
	}
	st.DedupLinks = []DedupLink{{LocalPlayer: "feishin", RemoteClient: "feishin"}}
	if reason, ok := isDuplicate(st, host, &l, &r); !ok || reason != DupLink {
		t.Fatal("link not applied")
	}
	// Link + different track: not merged.
	r2 := r
	r2.Track.Title = model.Ptr("Other")
	if _, ok := isDuplicate(st, host, &l, &r2); ok {
		t.Error("link merged different tracks")
	}

	// Item id referenced in artwork URL + same-host address.
	l.Track.Title = model.Ptr("Title differs in formatting (Remastered)")
	l.Track.ArtworkURL = model.Ptr("https://jf.example/Items/3b8df01c3a2a8ebe63ab3ea2d8fc79cd/Images/Primary?tag=x")
	r.Track.TrackID = model.Ptr("3b8df01c-3a2a-8ebe-63ab-3ea2d8fc79cd")
	r.Player.Address = model.Ptr("10.0.0.5")
	if reason, ok := isDuplicate(DefaultSettings(), host, &l, &r); !ok || reason != DupItemID {
		t.Errorf("item id: %v %v", reason, ok)
	}
	// Same item on a different host is not merged.
	r.Player.Address = model.Ptr("10.0.0.99")
	if _, ok := isDuplicate(DefaultSettings(), host, &l, &r); ok {
		t.Error("item id merged across hosts")
	}
}

func TestProviderCannotOverwriteForeignSession(t *testing.T) {
	e := newEngine(t, nil)
	addFake(t, e, "mpris", "mpris", model.OriginLocal, pt.Session("shared-id", model.OriginLocal, "Local", model.StatePlaying, pt.AllCaps...))
	remote := addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote)
	remote.Set(remoteSession("shared-id", "Remote", "phone", model.StatePlaying))
	time.Sleep(50 * time.Millisecond)
	s, _ := e.Session("shared-id")
	if *s.Track.Title != "Local" || s.ProviderInstance != "mpris" || s.Origin != model.OriginLocal {
		t.Fatalf("overwritten: %+v", s)
	}
}

func TestCommandRouting(t *testing.T) {
	e := newEngine(t, nil)
	local := addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:a", model.OriginLocal, "A", model.StatePaused, pt.AllCaps...))
	remote := addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote,
		remoteSession("jf:phone", "C", "Pixel", model.StatePlaying, model.CapPlay, model.CapPause, model.CapNext),
		remoteSession("jf:tv", "D", "TV", model.StatePlaying))
	ctx := context.Background()

	// Explicit target goes to the remote provider only.
	res, err := e.Command(ctx, "jf:phone", model.Command{Name: model.CmdPause})
	if err != nil || res.Outcome != model.OutcomeVerified || res.SessionID != "jf:phone" {
		t.Fatalf("remote pause: %+v %v", res, err)
	}
	if len(local.Calls()) != 0 || len(remote.Calls()) != 1 || remote.Calls()[0].SessionID != "jf:phone" {
		t.Fatalf("misrouted: local=%v remote=%v", local.Calls(), remote.Calls())
	}
	if remote.Refreshes() == 0 {
		t.Error("expected refresh for verification")
	}

	// Without a target the selected session is used and reported.
	_ = e.Select("mpris:a")
	res, err = e.Command(ctx, "", model.Command{Name: model.CmdPlay})
	if err != nil || res.SessionID != "mpris:a" || res.Outcome != model.OutcomeVerified {
		t.Fatalf("selected: %+v %v", res, err)
	}
	if len(local.Calls()) != 1 || len(remote.Calls()) != 1 {
		t.Fatal("misrouted selected command")
	}

	// Errors.
	cases := []struct {
		id   string
		cmd  model.Command
		code model.ErrorCode
	}{
		{"nope", model.Command{Name: model.CmdPlay}, model.ErrSessionNotFound},
		{"jf:tv", model.Command{Name: model.CmdPlay}, model.ErrNotControllable},
		{"jf:phone", model.Command{Name: model.CmdSeek, PositionMS: model.Ptr[int64](1)}, model.ErrUnsupported},
		{"jf:phone", model.Command{Name: "explode"}, model.ErrInvalidArgument},
		{"jf:phone", model.Command{Name: model.CmdSeek}, model.ErrInvalidArgument},
		{"mpris:a", model.Command{Name: model.CmdSetVolume, Volume: model.Ptr(7.0)}, model.ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, err := e.Command(ctx, c.id, c.cmd); model.CodeOf(err) != c.code {
			t.Errorf("%s %s: want %s got %v", c.id, c.cmd.Name, c.code, err)
		}
	}
	if len(remote.Calls()) != 1 {
		t.Errorf("rejected commands reached the provider: %v", remote.Calls())
	}

	// Disconnected session.
	d := remoteSession("jf:gone", "E", "Old", model.StatePlaying, model.CapPlay)
	d.Status = model.StatusDisconnected
	remote.Set(d)
	waitUntil(t, "gone", func() bool { _, ok := e.Session("jf:gone"); return ok })
	if _, err := e.Command(ctx, "jf:gone", model.Command{Name: model.CmdPlay}); model.CodeOf(err) != model.ErrSessionUnavailable {
		t.Errorf("disconnected: %v", err)
	}

	// No selection.
	e2 := newEngine(t, nil)
	if _, err := e2.Command(ctx, "", model.Command{Name: model.CmdPlay}); model.CodeOf(err) != model.ErrNoSessionSelected {
		t.Errorf("no selection: %v", err)
	}
}

func TestCommandOutcomesAndFailures(t *testing.T) {
	e := newEngine(t, func(o *Options) { o.CommandTimeout = 200 * time.Millisecond })
	f := addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:a", model.OriginLocal, "A", model.StatePaused, pt.AllCaps...))
	ctx := context.Background()

	// Provider accepts but nothing changes → acknowledged, not verified.
	f.ApplyEffects = false
	res, err := e.Command(ctx, "mpris:a", model.Command{Name: model.CmdPlay})
	if err != nil || res.Outcome != model.OutcomeAcknowledged || res.Detail == "" {
		t.Fatalf("ack: %+v %v", res, err)
	}

	// Delayed effect within window → verified.
	f.ApplyEffects = true
	f.EffectDelay = 100 * time.Millisecond
	res, err = e.Command(ctx, "mpris:a", model.Command{Name: model.CmdNext})
	if err != nil || res.Outcome != model.OutcomeVerified {
		t.Fatalf("delayed: %+v %v", res, err)
	}
	f.EffectDelay = 0

	// Seek is verified by position.
	res, err = e.Command(ctx, "mpris:a", model.Command{Name: model.CmdSeek, PositionMS: model.Ptr[int64](60_000)})
	if err != nil || res.Outcome != model.OutcomeVerified {
		t.Fatalf("seek: %+v %v", res, err)
	}
	res, err = e.Command(ctx, "mpris:a", model.Command{Name: model.CmdSetVolume, Volume: model.Ptr(0.3)})
	if err != nil || res.Outcome != model.OutcomeVerified {
		t.Fatalf("volume: %+v %v", res, err)
	}

	// Provider error is passed through with its code.
	f.CommandErr = model.Errorf(model.ErrProviderError, "boom")
	if _, err := e.Command(ctx, "mpris:a", model.Command{Name: model.CmdPause}); model.CodeOf(err) != model.ErrProviderError {
		t.Errorf("provider error: %v", err)
	}
	// Provider hang → timeout.
	f.CommandErr = nil
	f.CommandDelay = time.Second
	start := time.Now()
	if _, err := e.Command(ctx, "mpris:a", model.Command{Name: model.CmdPause}); model.CodeOf(err) != model.ErrTimeout {
		t.Errorf("timeout: %v", err)
	}
	if time.Since(start) > 600*time.Millisecond {
		t.Error("timeout not bounded")
	}
}

func TestEventsAndSnapshot(t *testing.T) {
	e := newEngine(t, nil)
	f := addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:a", model.OriginLocal, "A", model.StatePaused, pt.AllCaps...))
	snap, sub := e.Subscribe(Filter{})
	defer sub.Close()
	if len(snap.Sessions) != 1 || snap.Revision == 0 {
		t.Fatalf("snapshot %+v", snap)
	}
	next := func(want EventType) Event {
		t.Helper()
		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					t.Fatal("closed")
				}
				if ev.Type == want {
					return ev
				}
				if ev.Type == EventProviderStatus {
					continue
				}
				t.Fatalf("want %s got %s", want, ev.Type)
			case <-time.After(2 * time.Second):
				t.Fatalf("no %s event", want)
			}
		}
	}
	f.Set(pt.Session("mpris:b", model.OriginLocal, "B", model.StatePlaying, pt.AllCaps...))
	ev := next(EventSessionAdded)
	if ev.SessionID != "mpris:b" || ev.Revision <= snap.Revision {
		t.Errorf("added %+v", ev)
	}
	ev = next(EventSelectionChanged)
	if ev.SessionID != "mpris:b" || ev.Reason != "auto" {
		t.Errorf("selection %+v", ev)
	}

	// A position update that agrees with extrapolation emits nothing.
	s, _ := e.Session("mpris:b")
	now := time.Now()
	pos := *s.Playback.CurrentPosition(now)
	s2 := pt.Session("mpris:b", model.OriginLocal, "B", model.StatePlaying, pt.AllCaps...)
	s2.Playback.PositionMS, s2.Playback.PositionAt = &pos, &now
	f.Set(s2)
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
	// A seek (position jump) emits an update.
	jump := pos + 60_000
	s2.Playback.PositionMS = &jump
	f.Set(s2)
	next(EventSessionUpdated)

	f.Delete("mpris:b")
	next(EventSessionRemoved)
	next(EventSelectionChanged)
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	e := newEngine(t, func(o *Options) { o.SubscriberBuffer = 2 })
	f := addFake(t, e, "mpris", "mpris", model.OriginLocal)
	_, sub := e.Subscribe(Filter{})
	for i := 0; i < 10; i++ {
		f.Set(pt.Session("mpris:x", model.OriginLocal, string(rune('a'+i)), model.StatePaused))
	}
	waitUntil(t, "closed", func() bool {
		for {
			select {
			case _, ok := <-sub.Events():
				if !ok {
					return true
				}
			default:
				return false
			}
		}
	})
	if sub.Reason() != CloseOverflow {
		t.Errorf("reason %s", sub.Reason())
	}
	// A fresh subscription receives a full snapshot.
	snap, sub2 := e.Subscribe(Filter{})
	defer sub2.Close()
	if len(snap.Sessions) != 1 {
		t.Errorf("snapshot after overflow: %v", ids(snap))
	}
}

// crashing provider: Run fails after publishing a session.
type crashing struct {
	runs atomic.Int32
}

func (c *crashing) Instance() string     { return "crash" }
func (c *crashing) Kind() string         { return "crash" }
func (c *crashing) Origin() model.Origin { return model.OriginLocal }
func (c *crashing) Command(context.Context, string, model.Command) error {
	return nil
}
func (c *crashing) Run(ctx context.Context, sink provider.Sink) error {
	n := c.runs.Add(1)
	sink.Upsert(pt.Session("crash:1", model.OriginLocal, "x", model.StatePlaying))
	if n == 1 {
		time.Sleep(50 * time.Millisecond)
		return errors.New("crashed")
	}
	<-ctx.Done()
	return nil
}

func TestProviderRestartRemovesSessionsAndRecovers(t *testing.T) {
	e := newEngine(t, nil)
	_, sub := e.Subscribe(Filter{})
	defer sub.Close()
	c := &crashing{}
	e.AddProvider(c)
	removed := false
	deadline := time.After(5 * time.Second)
	for c.runs.Load() < 2 || !removed {
		select {
		case ev := <-sub.Events():
			if ev.Type == EventSessionRemoved && ev.SessionID == "crash:1" {
				removed = true
			}
		case <-deadline:
			t.Fatalf("runs=%d removed=%v", c.runs.Load(), removed)
		}
	}
	waitUntil(t, "session back", func() bool { _, ok := e.Session("crash:1"); return ok })

	e.RemoveProvider("crash")
	if _, ok := e.Session("crash:1"); ok {
		t.Error("session survived provider removal")
	}
	if len(e.ProviderStatuses()) != 0 {
		t.Error("provider still listed")
	}
}

func TestPausingKeepsSelectionAndInitialSyncIsNotActivity(t *testing.T) {
	e := newEngine(t, nil)
	// Both found playing during the initial sync: no recency information,
	// so the origin preference (local) decides.
	local := addFake(t, e, "mpris", "mpris", model.OriginLocal,
		pt.Session("mpris:a", model.OriginLocal, "A", model.StatePlaying, pt.AllCaps...))
	addFake(t, e, "jellyfin:home", "jellyfin", model.OriginRemote,
		remoteSession("jf:phone", "C", "Pixel", model.StatePlaying, pt.AllCaps...))
	sel := func() string {
		s, _ := e.Selected()
		if s == nil {
			return ""
		}
		return s.ID
	}
	if got := sel(); got != "mpris:a" {
		t.Fatalf("initial: %s", got)
	}
	// Pausing the local player through mediasync keeps it selected even
	// though the phone is still playing, so "toggle" resumes it.
	res, err := e.Command(context.Background(), "", model.Command{Name: model.CmdPause})
	if err != nil || res.SessionID != "mpris:a" {
		t.Fatalf("pause: %+v %v", res, err)
	}
	if got := sel(); got != "mpris:a" {
		t.Fatalf("after pause: %s", got)
	}
	res, err = e.Command(context.Background(), "", model.Command{Name: model.CmdPlayPause})
	if err != nil || res.SessionID != "mpris:a" {
		t.Fatalf("toggle went to %+v %v", res, err)
	}
	// Pausing in the player itself (not via mediasync) is activity too.
	time.Sleep(2 * time.Millisecond)
	local.Set(pt.Session("mpris:a", model.OriginLocal, "A", model.StatePaused, pt.AllCaps...))
	waitUntil(t, "still selected", func() bool { return sel() == "mpris:a" })
}
