package mpris

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/providertest"
	"github.com/arkaizn/mediasync/internal/testutil/dbustest"
	"github.com/arkaizn/mediasync/internal/testutil/fakempris"
)

const wait = 5 * time.Second

type env struct {
	bus  *dbustest.Bus
	prov *Provider
	sink *providertest.Sink
	done chan error
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T, cfg Config, bus *dbustest.Bus) *env {
	t.Helper()
	if bus == nil {
		bus = dbustest.Start(t)
	}
	cfg.Connect = bus.Connect
	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = 2 * time.Second
	}
	p := New(cfg, quietLogger())
	sink := providertest.NewSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sink) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			t.Error("provider did not stop")
		}
	})
	sink.WaitFor(t, wait, "provider running", func(s *providertest.Sink) bool {
		return s.Status().State == model.ProviderRunning
	})
	return &env{bus: bus, prov: p, sink: sink, done: done}
}

func newPlayer(t *testing.T, bus *dbustest.Bus, opts fakempris.Options) *fakempris.Player {
	t.Helper()
	conn, err := bus.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	pl, err := fakempris.Start(conn, opts)
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

func TestDiscoveryOfExistingAndNewPlayers(t *testing.T) {
	bus := dbustest.Start(t)
	opts := fakempris.FullCaps("feishin")
	opts.Identity = "Feishin"
	opts.DesktopEntry = "feishin"
	opts.Tracks[0].ArtURL = "https://jf.example/Items/abc/Images/Primary?tag=1&api_key=SECRET"
	newPlayer(t, bus, opts) // exists before the provider starts

	e := start(t, Config{}, bus)
	s := e.sink.WaitSession(t, "mpris:feishin", wait, "feishin", nil)

	if s.Player.Name != "Feishin" || *s.Player.Identity != "Feishin" || *s.Player.DesktopEntry != "feishin" {
		t.Errorf("player info: %+v", s.Player)
	}
	if *s.Player.BusName != "org.mpris.MediaPlayer2.feishin" {
		t.Errorf("bus name %v", *s.Player.BusName)
	}
	if s.Origin != model.OriginLocal || !s.Controllable || s.Status != model.StatusConnected {
		t.Errorf("origin/controllable/status: %+v", s)
	}
	if *s.Track.Title != "First Song" || !slices.Equal(s.Track.Artists, []string{"Artist One"}) || *s.Track.Album != "Album" {
		t.Errorf("track: %+v", s.Track)
	}
	if *s.Track.DurationMS != 200_000 {
		t.Errorf("duration %d", *s.Track.DurationMS)
	}
	if *s.Track.ArtworkURL != "https://jf.example/Items/abc/Images/Primary?tag=1" {
		t.Errorf("artwork not sanitised: %s", *s.Track.ArtworkURL)
	}
	if s.Playback.State != model.StatePaused {
		t.Errorf("state %s", s.Playback.State)
	}
	want := []model.Capability{model.CapPlay, model.CapPause, model.CapPlayPause, model.CapStop, model.CapNext, model.CapPrevious, model.CapSeek, model.CapVolume}
	if !slices.Equal(s.Capabilities, want) {
		t.Errorf("caps %v", s.Capabilities)
	}
	if s.Playback.Volume == nil || *s.Playback.Volume != 0.5 {
		t.Errorf("volume %v", s.Playback.Volume)
	}

	// A second player appearing later is discovered too.
	newPlayer(t, bus, fakempris.FullCaps("vlc"))
	e.sink.WaitSession(t, "mpris:vlc", wait, "vlc", nil)
	if n := len(e.sink.Sessions()); n != 2 {
		t.Errorf("want 2 sessions, got %d", n)
	}
	if e.sink.Status().State != model.ProviderRunning {
		t.Errorf("status %+v", e.sink.Status())
	}
}

func TestPlaybackAndMetadataUpdates(t *testing.T) {
	e := start(t, Config{}, nil)
	pl := newPlayer(t, e.bus, fakempris.FullCaps("p1"))
	e.sink.WaitSession(t, "mpris:p1", wait, "p1", nil)

	pl.SetStatus("Playing")
	e.sink.WaitSession(t, "mpris:p1", wait, "playing", func(s model.Session) bool { return s.Playback.State == model.StatePlaying })

	pl.SetTrack(2)
	s := e.sink.WaitSession(t, "mpris:p1", wait, "track change", func(s model.Session) bool {
		return s.Track.Title != nil && *s.Track.Title == "Third Song"
	})
	if s.Playback.PositionMS == nil || *s.Playback.PositionMS > 1000 {
		t.Errorf("position after track change: %v", s.Playback.PositionMS)
	}

	pl.SetProperty("CanGoNext", false)
	e.sink.WaitSession(t, "mpris:p1", wait, "caps change", func(s model.Session) bool { return !s.Has(model.CapNext) })

	pl.SetProperty("Volume", 0.8)
	e.sink.WaitSession(t, "mpris:p1", wait, "volume change", func(s model.Session) bool {
		return s.Playback.Volume != nil && *s.Playback.Volume == 0.8
	})
}

func TestDisappearAndRestartKeepsStableID(t *testing.T) {
	e := start(t, Config{}, nil)
	conn, err := e.bus.Connect()
	if err != nil {
		t.Fatal(err)
	}
	pl, err := fakempris.Start(conn, fakempris.FullCaps("feishin"))
	if err != nil {
		t.Fatal(err)
	}
	e.sink.WaitSession(t, "mpris:feishin", wait, "appear", nil)

	conn.Close() // player process exits
	e.sink.WaitFor(t, wait, "disappear", func(s *providertest.Sink) bool {
		_, ok := s.Session("mpris:feishin")
		return !ok
	})
	_ = pl

	// Restart: a new connection takes the same name; the session ID is the same.
	newPlayer(t, e.bus, fakempris.FullCaps("feishin"))
	s := e.sink.WaitSession(t, "mpris:feishin", wait, "reappear", nil)
	if s.ID != "mpris:feishin" {
		t.Fatal(s.ID)
	}

	// Releasing the name without closing the connection also removes it.
	pl2 := newPlayer(t, e.bus, fakempris.FullCaps("other"))
	e.sink.WaitSession(t, "mpris:other", wait, "other", nil)
	if err := pl2.Stop(); err != nil {
		t.Fatal(err)
	}
	e.sink.WaitFor(t, wait, "other gone", func(s *providertest.Sink) bool {
		_, ok := s.Session("mpris:other")
		return !ok
	})
}

func TestMissingMetadataAndCapabilities(t *testing.T) {
	e := start(t, Config{}, nil)
	newPlayer(t, e.bus, fakempris.Options{Name: "bare", Status: "Stopped", OmitMetadata: true})
	s := e.sink.WaitSession(t, "mpris:bare", wait, "bare", nil)
	if s.Track.Title != nil || s.Track.Artists != nil || s.Track.Album != nil || s.Track.DurationMS != nil || s.Track.ArtworkURL != nil {
		t.Errorf("expected unknown track fields, got %+v", s.Track)
	}
	if s.Player.Name != "bare" || s.Player.Identity != nil {
		t.Errorf("player %+v", s.Player)
	}
	if s.Controllable || len(s.Capabilities) != 0 {
		t.Errorf("expected no control: %+v", s.Capabilities)
	}
	if s.Playback.Volume != nil {
		t.Error("volume should be unknown")
	}
	err := e.prov.Command(context.Background(), "mpris:bare", model.Command{Name: model.CmdPlay})
	if model.CodeOf(err) != model.ErrUnsupported {
		t.Fatalf("want unsupported, got %v", err)
	}

	// Malformed metadata: wrong types are ignored, single-string artist tolerated.
	opts := fakempris.FullCaps("weird")
	opts.RawMetadata = map[string]dbus.Variant{
		"xesam:title":   dbus.MakeVariant(int32(5)),
		"xesam:artist":  dbus.MakeVariant("Solo Artist"),
		"mpris:length":  dbus.MakeVariant(uint64(1_000_000)),
		"mpris:trackid": dbus.MakeVariant("/not/an/objpath/but/string"),
	}
	newPlayer(t, e.bus, opts)
	s = e.sink.WaitSession(t, "mpris:weird", wait, "weird", nil)
	if s.Track.Title != nil || !slices.Equal(s.Track.Artists, []string{"Solo Artist"}) || *s.Track.DurationMS != 1000 {
		t.Errorf("weird track %+v", s.Track)
	}
}

func TestReadOnlyVolumeIsNotACapability(t *testing.T) {
	e := start(t, Config{}, nil)
	opts := fakempris.FullCaps("ro")
	opts.VolumeReadOnly = true
	newPlayer(t, e.bus, opts)
	s := e.sink.WaitSession(t, "mpris:ro", wait, "ro", nil)
	if s.Has(model.CapVolume) {
		t.Error("read-only volume advertised as capability")
	}
	if s.Playback.Volume == nil {
		t.Error("volume value should still be reported")
	}
}

func TestControls(t *testing.T) {
	e := start(t, Config{}, nil)
	pl := newPlayer(t, e.bus, fakempris.FullCaps("ctl"))
	e.sink.WaitSession(t, "mpris:ctl", wait, "ctl", nil)
	ctx := context.Background()
	id := "mpris:ctl"

	must := func(c model.Command) {
		t.Helper()
		if err := e.prov.Command(ctx, id, c); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
	}
	must(model.Command{Name: model.CmdPlay})
	e.sink.WaitSession(t, id, wait, "playing", func(s model.Session) bool { return s.Playback.State == model.StatePlaying })
	must(model.Command{Name: model.CmdPause})
	e.sink.WaitSession(t, id, wait, "paused", func(s model.Session) bool { return s.Playback.State == model.StatePaused })
	must(model.Command{Name: model.CmdPlayPause})
	e.sink.WaitSession(t, id, wait, "toggled", func(s model.Session) bool { return s.Playback.State == model.StatePlaying })
	must(model.Command{Name: model.CmdNext})
	e.sink.WaitSession(t, id, wait, "next", func(s model.Session) bool { return *s.Track.Title == "Second Song" })
	must(model.Command{Name: model.CmdPrevious})
	e.sink.WaitSession(t, id, wait, "prev", func(s model.Session) bool { return *s.Track.Title == "First Song" })

	must(model.Command{Name: model.CmdSeek, PositionMS: model.Ptr[int64](42_000)})
	e.sink.WaitSession(t, id, wait, "seek", func(s model.Session) bool {
		return s.Playback.PositionMS != nil && *s.Playback.PositionMS >= 42_000 && *s.Playback.PositionMS < 43_000
	})
	must(model.Command{Name: model.CmdSetVolume, Volume: model.Ptr(0.25)})
	e.sink.WaitSession(t, id, wait, "volume", func(s model.Session) bool {
		return s.Playback.Volume != nil && *s.Playback.Volume == 0.25
	})
	must(model.Command{Name: model.CmdStop})
	e.sink.WaitSession(t, id, wait, "stopped", func(s model.Session) bool { return s.Playback.State == model.StateStopped })

	calls := pl.Calls()
	wantPrefix := []string{"Play", "Pause", "PlayPause", "Next", "Previous", "SetPosition(/org/example/track/t1,42000000)"}
	if !slices.Equal(calls[:len(wantPrefix)], wantPrefix) {
		t.Errorf("calls %v", calls)
	}
}

func TestRelativeSeekAndSeekWithoutTrackID(t *testing.T) {
	e := start(t, Config{}, nil)
	opts := fakempris.FullCaps("rel")
	opts.PositionUS = 10_000_000
	opts.RawMetadata = map[string]dbus.Variant{"xesam:title": dbus.MakeVariant("No ID")}
	pl := newPlayer(t, e.bus, opts)
	e.sink.WaitSession(t, "mpris:rel", wait, "rel", nil)

	if err := e.prov.Command(context.Background(), "mpris:rel", model.Command{Name: model.CmdSeek, OffsetMS: model.Ptr[int64](5_000)}); err != nil {
		t.Fatal(err)
	}
	// Absolute seek without a track id falls back to a relative Seek.
	if err := e.prov.Command(context.Background(), "mpris:rel", model.Command{Name: model.CmdSeek, PositionMS: model.Ptr[int64](30_000)}); err != nil {
		t.Fatal(err)
	}
	calls := pl.Calls()
	if calls[0] != "Seek(5000000)" || calls[1] != "Seek(15000000)" {
		t.Errorf("calls %v", calls)
	}
}

func TestUnsupportedCommandsAreRejected(t *testing.T) {
	e := start(t, Config{}, nil)
	opts := fakempris.FullCaps("limited")
	opts.CanSeek, opts.CanGoNext, opts.Volume = false, false, nil
	pl := newPlayer(t, e.bus, opts)
	e.sink.WaitSession(t, "mpris:limited", wait, "limited", nil)
	for _, c := range []model.Command{
		{Name: model.CmdNext},
		{Name: model.CmdSeek, PositionMS: model.Ptr[int64](1)},
		{Name: model.CmdSetVolume, Volume: model.Ptr(0.1)},
	} {
		if err := e.prov.Command(context.Background(), "mpris:limited", c); model.CodeOf(err) != model.ErrUnsupported {
			t.Errorf("%s: want unsupported, got %v", c.Name, err)
		}
	}
	if len(pl.Calls()) != 0 {
		t.Errorf("player must not be called: %v", pl.Calls())
	}
	if err := e.prov.Command(context.Background(), "mpris:nope", model.Command{Name: model.CmdPlay}); model.CodeOf(err) != model.ErrSessionNotFound {
		t.Errorf("want not found, got %v", err)
	}
}

func TestCommandTimeout(t *testing.T) {
	bus := dbustest.Start(t)
	e := start(t, Config{CallTimeout: 300 * time.Millisecond}, bus)
	opts := fakempris.FullCaps("slow")
	newPlayer(t, bus, opts)
	e.sink.WaitSession(t, "mpris:slow", wait, "slow", nil)

	// Make the player slow only after discovery has finished.
	conn2, _ := bus.Connect()
	defer conn2.Close()
	slowOpts := fakempris.FullCaps("slow2")
	slowOpts.MethodDelay = 1500 * time.Millisecond
	if _, err := fakempris.Start(conn2, slowOpts); err != nil {
		t.Fatal(err)
	}
	e.sink.WaitSession(t, "mpris:slow2", wait, "slow2", nil)
	start := time.Now()
	err := e.prov.Command(context.Background(), "mpris:slow2", model.Command{Name: model.CmdPlay})
	if model.CodeOf(err) != model.ErrTimeout {
		t.Fatalf("want timeout, got %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("timeout not bounded: %v", d)
	}
}

func TestIncludeIgnoreFilters(t *testing.T) {
	bus := dbustest.Start(t)
	newPlayer(t, bus, fakempris.FullCaps("playerctld"))
	newPlayer(t, bus, fakempris.FullCaps("firefox.instance_1_2"))
	newPlayer(t, bus, fakempris.FullCaps("feishin"))
	e := start(t, Config{Ignore: []string{"playerctld", "firefox"}}, bus)
	e.sink.WaitSession(t, "mpris:feishin", wait, "feishin", nil)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.sink.Sessions()); n != 1 {
		t.Errorf("want only feishin, got %v", e.sink.Sessions())
	}

	bus2 := dbustest.Start(t)
	newPlayer(t, bus2, fakempris.FullCaps("vlc"))
	newPlayer(t, bus2, fakempris.FullCaps("feishin"))
	e2 := start(t, Config{Include: []string{"Feishin"}}, bus2)
	e2.sink.WaitSession(t, "mpris:feishin", wait, "feishin", nil)
	time.Sleep(200 * time.Millisecond)
	if n := len(e2.sink.Sessions()); n != 1 {
		t.Errorf("want only feishin, got %v", e2.sink.Sessions())
	}
}

func TestMultipleNamesOneConnectionAppearOnce(t *testing.T) {
	e := start(t, Config{}, nil)
	conn, _ := e.bus.Connect()
	defer conn.Close()
	if _, err := fakempris.Start(conn, fakempris.FullCaps("vlc")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RequestName("org.mpris.MediaPlayer2.vlc.instance77", dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	e.sink.WaitSession(t, "mpris:vlc", wait, "vlc", nil)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.sink.Sessions()); n != 1 {
		t.Errorf("want 1 session, got %v", e.sink.Sessions())
	}
}

func TestPositionResync(t *testing.T) {
	e := start(t, Config{PositionResync: 100 * time.Millisecond}, nil)
	opts := fakempris.FullCaps("pos")
	opts.Status = "Playing"
	opts.NoSeekedSignal = true
	pl := newPlayer(t, e.bus, opts)
	e.sink.WaitSession(t, "mpris:pos", wait, "pos", nil)
	// Jump the player's position without any signal; resync must pick it up.
	pl.SetProperty("Position", int64(90_000_000))
	e.sink.WaitSession(t, "mpris:pos", wait, "resync", func(s model.Session) bool {
		return s.Playback.PositionMS != nil && *s.Playback.PositionMS >= 90_000
	})
}

func TestBusLossEndsRunAndRemovesSessions(t *testing.T) {
	bus := dbustest.Start(t)
	newPlayer(t, bus, fakempris.FullCaps("a"))
	e := start(t, Config{}, bus)
	e.sink.WaitSession(t, "mpris:a", wait, "a", nil)
	bus.Stop()
	select {
	case err := <-e.done:
		if model.CodeOf(err) != model.ErrProviderUnavailable {
			t.Errorf("want provider_unavailable, got %v", err)
		}
		e.done <- err // let cleanup observe exit
	case <-time.After(wait):
		t.Fatal("Run did not return after bus loss")
	}
	if len(e.sink.Sessions()) != 0 {
		t.Error("sessions not removed after bus loss")
	}
}
