package jellyfin

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/providertest"
	"github.com/arkaizn/mediasync/internal/redact"
	"github.com/arkaizn/mediasync/internal/secrets"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

const wait = 5 * time.Second

type env struct {
	srv   *fakejellyfin.Server
	prov  *Provider
	sink  *providertest.Sink
	creds *secrets.Memory
	done  chan error
	stop  context.CancelFunc
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newEnv(t *testing.T, mutate func(*Config)) *env {
	t.Helper()
	srv := fakejellyfin.New("alice", "secret-pw")
	t.Cleanup(srv.Close)
	creds := secrets.NewMemory()
	cfg := Config{
		Name: "home", URL: srv.URL, DeviceID: "mediasync-test-device", DeviceName: "mydesk", Version: "0.1.0",
		PollInterval: 50 * time.Millisecond, RequestTimeout: time.Second, MaxBackoff: 200 * time.Millisecond,
		Credentials: creds,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	return &env{srv: srv, prov: p, creds: creds, sink: providertest.NewSink()}
}

func (e *env) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- e.prov.Run(ctx, e.sink) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(wait):
			t.Error("provider did not stop")
		}
	})
}

func (e *env) state(t *testing.T, want model.ProviderState) {
	t.Helper()
	e.sink.WaitFor(t, wait, "state "+string(want), func(s *providertest.Sink) bool { return s.Status().State == want })
}

func (e *env) login(t *testing.T) {
	t.Helper()
	res, err := e.prov.Login(context.Background(), "alice", "secret-pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.UserName != "alice" || res.ServerName != "fakefin" || res.Backend != "memory" {
		t.Errorf("login result %+v", res)
	}
}

func phone(srv *fakejellyfin.Server) *fakejellyfin.Client {
	return srv.AddClient(&fakejellyfin.Client{
		UserID: srv.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "Pixel 7", DeviceID: "phone-1",
		Controllable: true, ApplyCommands: true, SupportedCommands: []string{"SetVolume", "VolumeUp"},
		ItemID: "3b8df01c3a2a8ebe63ab3ea2d8fc79cd", Title: "Track 1", Album: "Album X", AlbumID: "deec99d6fc4ab8812411257e24696f5d",
		AlbumArtist: "Artist A", Artists: []string{"Artist A"}, RunTimeTicks: 1_800_000_000, ImageTag: "0b348f22",
		PositionTicks: 100_000_000, CanSeek: true, Volume: 70,
	})
}

func (e *env) sessionFor(t *testing.T, deviceID, client string, cond func(model.Session) bool) model.Session {
	t.Helper()
	id := e.prov.SessionID(e.srv.ServerID, deviceID, client)
	return e.sink.WaitSession(t, id, wait, "session "+deviceID, cond)
}

func TestUnauthenticatedThenLoginDiscoversSessions(t *testing.T) {
	e := newEnv(t, nil)
	ph := phone(e.srv)
	// View-only client (no control connection).
	e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), UserName: "alice", Client: "Jellyfin Web", DeviceName: "Firefox", DeviceID: "web-1",
		ItemID: "2d9afb28f2528231cce7646b8591accd", Title: "Track 2", Artists: []string{"Artist A"}, RunTimeTicks: 1_800_000_000,
	})
	// Idle client, other user's client and our own device must be excluded.
	e.srv.AddClient(&fakejellyfin.Client{UserID: e.srv.UserID("alice"), Client: "Finamp", DeviceName: "Tablet", DeviceID: "idle-1", Controllable: true})
	e.srv.AddClient(&fakejellyfin.Client{UserID: "someone-else", Client: "Finamp", DeviceName: "Bob's phone", DeviceID: "bob-1", ItemID: "x", Title: "Bob's song"})
	e.srv.AddClient(&fakejellyfin.Client{UserID: e.srv.UserID("alice"), Client: "mediasync", DeviceName: "mydesk", DeviceID: "mediasync-test-device", ItemID: "y", Title: "self"})

	e.run(t)
	e.state(t, model.ProviderUnauthenticated)
	if st := e.sink.Status(); st.LastError == nil || st.LastError.Code != model.ErrNotAuthenticated {
		t.Errorf("status %+v", st)
	}
	e.login(t)
	e.state(t, model.ProviderRunning)

	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	if s.ProviderSessionID != ph.SessionID || s.Origin != model.OriginRemote || !s.Controllable {
		t.Errorf("session %+v", s)
	}
	if s.Player.Name != "Pixel 7 (Finamp)" || *s.Player.Client != "Finamp" || *s.Player.Device != "Pixel 7" || *s.Player.DeviceID != "phone-1" {
		t.Errorf("player %+v", s.Player)
	}
	if *s.Server.ID != e.srv.ServerID || *s.Server.Name != "fakefin" || *s.Server.UserName != "alice" || s.Server.Instance != "home" {
		t.Errorf("server %+v", s.Server)
	}
	if *s.Track.Title != "Track 1" || *s.Track.Album != "Album X" || !slices.Equal(s.Track.Artists, []string{"Artist A"}) ||
		*s.Track.DurationMS != 180_000 || *s.Track.TrackID != "3b8df01c3a2a8ebe63ab3ea2d8fc79cd" {
		t.Errorf("track %+v", s.Track)
	}
	wantArt := e.srv.URL + "/Items/deec99d6fc4ab8812411257e24696f5d/Images/Primary?tag=0b348f22"
	if s.Track.ArtworkURL == nil || *s.Track.ArtworkURL != wantArt {
		t.Errorf("artwork %v", s.Track.ArtworkURL)
	}
	if s.Playback.State != model.StatePlaying || *s.Playback.Volume != 0.7 || *s.Playback.PositionMS < 10_000 {
		t.Errorf("playback %+v", s.Playback)
	}
	if !slices.Equal(s.Capabilities, model.AllCapabilities) {
		t.Errorf("caps %v", s.Capabilities)
	}

	web := e.sessionFor(t, "web-1", "Jellyfin Web", nil)
	if web.Controllable || len(web.Capabilities) != 0 || web.Track.ArtworkURL != nil {
		t.Errorf("view-only session %+v", web)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(e.sink.Sessions()); n != 2 {
		t.Errorf("expected 2 sessions, got %d: %v", n, e.sink.Sessions())
	}
	creds, err := e.creds.Get(context.Background(), e.prov.credAttrs(""))
	if err != nil || !strings.HasPrefix(creds.Reveal(), "tok") {
		t.Errorf("token not stored: %v", err)
	}
}

func TestStoredTokenIsUsedAndNeverInURLs(t *testing.T) {
	e := newEnv(t, nil)
	phone(e.srv)
	e.login(t) // stores token before Run
	e.run(t)
	e.state(t, model.ProviderRunning)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPause}); err != nil {
		t.Fatal(err)
	}
	tok, _ := e.creds.Get(context.Background(), e.prov.credAttrs(""))
	for _, r := range e.srv.Requests() {
		if strings.Contains(r, tok.Reveal()) || strings.Contains(strings.ToLower(r), "api_key") {
			t.Fatalf("token in URL: %s", r)
		}
	}
	for _, h := range e.srv.Headers() {
		if h.Get("X-Emby-Token") != "" {
			t.Fatal("legacy token header used")
		}
		if a := h.Get("Authorization"); !strings.HasPrefix(a, "MediaBrowser ") || !strings.Contains(a, `DeviceId="mediasync-test-device"`) {
			t.Fatalf("auth header %q", a)
		}
	}
}

func TestCommandsReachTheExactClient(t *testing.T) {
	e := newEnv(t, nil)
	ph := phone(e.srv)
	other := e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), UserName: "alice", Client: "Finamp", DeviceName: "Tablet", DeviceID: "tab-1",
		Controllable: true, ApplyCommands: true, ItemID: "i2", Title: "Other", RunTimeTicks: 1_000_000_000, CanSeek: true,
	})
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	e.sessionFor(t, "tab-1", "Finamp", nil)
	ctx := context.Background()
	for _, c := range []model.Command{
		{Name: model.CmdPause}, {Name: model.CmdPlay}, {Name: model.CmdPlayPause}, {Name: model.CmdNext}, {Name: model.CmdPrevious},
		{Name: model.CmdSeek, PositionMS: model.Ptr[int64](42_000)},
		{Name: model.CmdSetVolume, Volume: model.Ptr(0.35)},
		{Name: model.CmdStop},
	} {
		if err := e.prov.Command(ctx, s.ID, c); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
	}
	got := e.srv.ClientState(ph.SessionID).Received
	want := []string{"Pause", "Unpause", "PlayPause", "NextTrack", "PreviousTrack", "Seek(420000000)", "SetVolume(35)", "Stop"}
	if !slices.Equal(got, want) {
		t.Errorf("received %v", got)
	}
	if r := e.srv.ClientState(other.SessionID).Received; len(r) != 0 {
		t.Errorf("other client received commands: %v", r)
	}
	// After Stop the session has nothing playing and disappears.
	e.sink.WaitFor(t, wait, "stopped session removed", func(sk *providertest.Sink) bool {
		_, ok := sk.Session(s.ID)
		return !ok
	})
}

func TestViewOnlyAndUnsupportedCommandsAreNotSent(t *testing.T) {
	e := newEnv(t, nil)
	web := e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), Client: "Jellyfin Web", DeviceName: "Firefox", DeviceID: "web-1",
		ItemID: "a", Title: "x", RunTimeTicks: 1_000_000_000,
	})
	lim := e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), Client: "Gelli", DeviceName: "Phone", DeviceID: "lim-1", Controllable: true,
		ItemID: "b", Title: "y", RunTimeTicks: 1_000_000_000, CanSeek: false,
	})
	e.login(t)
	e.run(t)
	sw := e.sessionFor(t, "web-1", "Jellyfin Web", nil)
	sl := e.sessionFor(t, "lim-1", "Gelli", nil)
	before := len(e.srv.Requests())
	if err := e.prov.Command(context.Background(), sw.ID, model.Command{Name: model.CmdPause}); model.CodeOf(err) != model.ErrNotControllable {
		t.Errorf("view-only: %v", err)
	}
	if sl.Has(model.CapSeek) || sl.Has(model.CapVolume) {
		t.Errorf("caps %v", sl.Capabilities)
	}
	if err := e.prov.Command(context.Background(), sl.ID, model.Command{Name: model.CmdSeek, PositionMS: model.Ptr[int64](1)}); model.CodeOf(err) != model.ErrUnsupported {
		t.Errorf("seek: %v", err)
	}
	if err := e.prov.Command(context.Background(), sl.ID, model.Command{Name: model.CmdSetVolume, Volume: model.Ptr(0.1)}); model.CodeOf(err) != model.ErrUnsupported {
		t.Errorf("volume: %v", err)
	}
	for _, r := range e.srv.Requests()[before:] {
		if strings.Contains(r, "/Playing/") || strings.Contains(r, "/Command") {
			t.Errorf("command sent despite rejection: %s", r)
		}
	}
	_ = web
	_ = lim
}

func TestSessionGoneBetweenPollAndCommand(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PollInterval = time.Hour })
	ph := phone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	e.srv.RemoveClient(ph.SessionID)
	err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPause})
	if model.CodeOf(err) != model.ErrSessionNotFound {
		t.Fatalf("want session_not_found, got %v", err)
	}
}

func TestStableIDAcrossJellyfinSessionChange(t *testing.T) {
	e := newEnv(t, nil)
	ph := phone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	// The client reconnects and gets a new Jellyfin session id.
	e.srv.RemoveClient(ph.SessionID)
	np := phone(e.srv)
	s2 := e.sessionFor(t, "phone-1", "Finamp", func(x model.Session) bool { return x.ProviderSessionID == np.SessionID })
	if s2.ID != s.ID {
		t.Errorf("ID changed: %s -> %s", s.ID, s2.ID)
	}
}

func TestStaleSessionWithoutControlChannel(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.StaleAfter = time.Minute })
	c := e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), Client: "Finamp", DeviceName: "Old phone", DeviceID: "old-1",
		ItemID: "a", Title: "x", RunTimeTicks: 1_800_000_000, PositionTicks: 10_000_000,
		LastCheckIn: time.Now().UTC().Add(-5 * time.Minute),
	})
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "old-1", "Finamp", nil)
	if s.Status != model.StatusStale || s.Playback.PositionMS != nil || s.Controllable {
		t.Errorf("stale session %+v", s)
	}
	// A fresh report makes it current again.
	e.srv.Update(c.SessionID, func(c *fakejellyfin.Client) { c.LastCheckIn = time.Now().UTC() })
	e.sessionFor(t, "old-1", "Finamp", func(x model.Session) bool { return x.Status == model.StatusConnected && x.Playback.PositionMS != nil })
	// Paused sessions use the longer threshold.
	e.srv.Update(c.SessionID, func(c *fakejellyfin.Client) {
		c.LastCheckIn = time.Now().UTC().Add(-5 * time.Minute)
		c.Paused = true
	})
	time.Sleep(200 * time.Millisecond)
	if x, _ := e.sink.Session(s.ID); x.Status != model.StatusConnected {
		t.Errorf("paused session marked stale too early: %s", x.Status)
	}
}

func TestServerOutageAndRecovery(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.RemoveDisconnectedAfter = 800 * time.Millisecond })
	phone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)

	e.srv.SetDown(true)
	d := e.sessionFor(t, "phone-1", "Finamp", func(x model.Session) bool { return x.Status == model.StatusDisconnected })
	if d.Controllable || len(d.Capabilities) != 0 || d.Error == nil || d.Error.Code != model.ErrServerUnreachable {
		t.Errorf("disconnected session %+v", d)
	}
	e.state(t, model.ProviderDegraded)
	if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPause}); model.CodeOf(err) != model.ErrSessionUnavailable {
		t.Errorf("command while down: %v", err)
	}
	e.srv.SetDown(false)
	e.sessionFor(t, "phone-1", "Finamp", func(x model.Session) bool { return x.Status == model.StatusConnected && x.Controllable })
	e.state(t, model.ProviderRunning)

	// A long outage removes sessions; they come back after recovery.
	e.srv.SetDown(true)
	e.sink.WaitFor(t, wait, "removal after long outage", func(sk *providertest.Sink) bool {
		_, ok := sk.Session(s.ID)
		return !ok
	})
	e.srv.SetDown(false)
	e.sessionFor(t, "phone-1", "Finamp", nil)

	// HTTP 500 is treated like an outage, not as an auth problem.
	e.srv.SetFail500(true)
	e.sessionFor(t, "phone-1", "Finamp", func(x model.Session) bool { return x.Status == model.StatusDisconnected })
	e.srv.SetFail500(false)
	e.sessionFor(t, "phone-1", "Finamp", func(x model.Session) bool { return x.Status == model.StatusConnected })
}

func TestRevokedTokenBecomesUnauthenticated(t *testing.T) {
	e := newEnv(t, nil)
	phone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "phone-1", "Finamp", nil)
	// Revoke the token server-side (e.g. via the Jellyfin dashboard).
	tok, _ := e.creds.Get(context.Background(), e.prov.credAttrs(""))
	other, _ := NewClient(ClientOptions{BaseURL: e.srv.URL, DeviceID: "x", Timeout: time.Second})
	other.SetToken(tok)
	if err := other.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.state(t, model.ProviderUnauthenticated)
	e.sink.WaitFor(t, wait, "sessions removed", func(sk *providertest.Sink) bool { _, ok := sk.Session(s.ID); return !ok })
	if st := e.sink.Status(); st.LastError == nil || st.LastError.Code != model.ErrAuthFailed {
		t.Errorf("status %+v", st)
	}
	e.login(t)
	e.state(t, model.ProviderRunning)
	e.sessionFor(t, "phone-1", "Finamp", nil)
}

func TestLogoutRevokesAndDeletes(t *testing.T) {
	e := newEnv(t, nil)
	phone(e.srv)
	e.login(t)
	e.run(t)
	e.sessionFor(t, "phone-1", "Finamp", nil)
	if e.srv.Tokens() != 1 {
		t.Fatalf("tokens %d", e.srv.Tokens())
	}
	res, err := e.prov.Logout(context.Background())
	if err != nil || !res.Revoked || !res.CredentialDeleted {
		t.Fatalf("logout %+v %v", res, err)
	}
	if e.srv.Tokens() != 0 {
		t.Error("token not revoked on server")
	}
	if _, err := e.creds.Get(context.Background(), e.prov.credAttrs("")); !errors.Is(err, secrets.ErrNotFound) {
		t.Error("credential not deleted")
	}
	e.state(t, model.ProviderUnauthenticated)
	if len(e.sink.Sessions()) != 0 {
		t.Error("sessions remain after logout")
	}
	// Logout while the server is down still deletes the local credential.
	e.login(t)
	e.srv.SetDown(true)
	res, err = e.prov.Logout(context.Background())
	if err != nil || res.Revoked || !res.CredentialDeleted || res.Detail == "" {
		t.Errorf("offline logout %+v %v", res, err)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.prov.Login(context.Background(), "alice", "wrong"); model.CodeOf(err) != model.ErrAuthFailed {
		t.Errorf("bad password: %v", err)
	}
	if _, err := e.prov.Login(context.Background(), "", "x"); model.CodeOf(err) != model.ErrInvalidArgument {
		t.Errorf("empty user: %v", err)
	}
	// Credential store failure: login fails and the new token is revoked.
	e.creds.Fail = model.Errorf(model.ErrCredentialStoreUnavailable, "no keyring")
	if _, err := e.prov.Login(context.Background(), "alice", "secret-pw"); model.CodeOf(err) != model.ErrCredentialStoreUnavailable {
		t.Errorf("store failure: %v", err)
	}
	if e.srv.Tokens() != 0 {
		t.Error("orphaned token left on server")
	}
	// Provider reports the credential store error.
	e.run(t)
	e.state(t, model.ProviderError)
	if st := e.sink.Status(); st.LastError.Code != model.ErrCredentialStoreUnavailable {
		t.Errorf("status %+v", st)
	}
	// Unreachable server.
	e.srv.SetDown(true)
	if _, err := e.prov.Login(context.Background(), "alice", "secret-pw"); model.CodeOf(err) != model.ErrServerUnreachable {
		t.Errorf("down: %v", err)
	}
}

func TestErrorsAndStatusNeverContainSecrets(t *testing.T) {
	e := newEnv(t, nil)
	phone(e.srv)
	e.login(t)
	e.run(t)
	e.sessionFor(t, "phone-1", "Finamp", nil)
	tok, _ := e.creds.Get(context.Background(), e.prov.credAttrs(""))
	e.srv.SetDown(true)
	e.state(t, model.ProviderDegraded)
	for _, txt := range []string{
		strings.Join(func() []string {
			var out []string
			for _, s := range e.sink.Sessions() {
				if s.Error != nil {
					out = append(out, s.Error.Message)
				}
			}
			return out
		}(), "\n"),
		e.sink.Status().LastError.Message,
	} {
		if strings.Contains(txt, tok.Reveal()) || strings.Contains(txt, "secret-pw") {
			t.Fatalf("secret leaked: %s", txt)
		}
	}
}

func TestTLSVerification(t *testing.T) {
	srv := fakejellyfin.NewTLS("alice", "pw")
	defer srv.Close()
	creds := secrets.NewMemory()
	p, err := New(Config{Name: "tls", URL: srv.URL, DeviceID: "d", Credentials: creds, RequestTimeout: time.Second}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Login(context.Background(), "alice", "pw"); model.CodeOf(err) != model.ErrTLS {
		t.Fatalf("self-signed must fail verification, got %v", err)
	}
	// Trusting the CA explicitly works.
	ca := filepath.Join(t.TempDir(), "ca.pem")
	cert := srv.Certificate()
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := New(Config{Name: "tls", URL: srv.URL, DeviceID: "d", Credentials: creds, RequestTimeout: time.Second, CAFile: ca}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Login(context.Background(), "alice", "pw"); err != nil {
		t.Fatalf("with CA: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	creds := secrets.NewMemory()
	for _, u := range []string{"", "ftp://x", "https://user:pw@host", "https://h/?api_key=x", "not a url"} {
		if _, err := New(Config{Name: "x", URL: u, DeviceID: "d", Credentials: creds}, quiet()); model.CodeOf(err) != model.ErrInvalidConfig {
			t.Errorf("%q: %v", u, err)
		}
	}
	if _, err := New(Config{URL: "https://h", DeviceID: "d", Credentials: creds}, quiet()); err == nil {
		t.Error("missing name accepted")
	}
	p, err := New(Config{Name: "x", URL: "https://jf.example/base/", DeviceID: "d", Credentials: creds}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if p.ServerURL() != "https://jf.example/base" || p.Instance() != "jellyfin:x" {
		t.Errorf("url %s instance %s", p.ServerURL(), p.Instance())
	}
}

func TestPasswordNotRetained(t *testing.T) {
	e := newEnv(t, nil)
	e.login(t)
	tok, _ := e.creds.Get(context.Background(), e.prov.credAttrs(""))
	if strings.Contains(tok.Reveal(), "secret-pw") {
		t.Fatal("password stored")
	}
	_ = redact.Secret("")
}

func TestCheckDiagnosesRemoteControl(t *testing.T) {
	e := newEnv(t, nil)
	phone(e.srv)
	// Finamp announced remote control but its WebSocket never got through.
	e.srv.AddClient(&fakejellyfin.Client{
		UserID: e.srv.UserID("alice"), Client: "Finamp", DeviceName: "iPhone", DeviceID: "iphone-1",
		SupportedCommands: []string{"SetVolume", "PlayState"},
		ItemID:            "x", Title: "Song", RunTimeTicks: 1_000_000_000,
	})
	ctx := context.Background()
	if res := e.prov.Check(ctx); res.Error == nil || res.Error.Code != model.ErrNotAuthenticated {
		t.Fatalf("check before login: %+v", res)
	}
	e.login(t)

	res := e.prov.Check(ctx)
	if res.Error != nil || !res.WebSocket.OK || res.WebSocket.HTTPStatus != 101 || res.ServerVersion != "12.2.0" || res.UserName != "alice" {
		t.Fatalf("check: %+v", res)
	}
	byDevice := map[string]ClientCheck{}
	for _, c := range res.Clients {
		byDevice[c.Device] = c
	}
	if c := byDevice["Pixel 7"]; !c.SupportsRemoteControl || !strings.HasPrefix(c.Explanation, "controllable") {
		t.Errorf("phone: %+v", c)
	}
	if c := byDevice["iPhone"]; c.SupportsRemoteControl || !strings.Contains(c.Explanation, "no live control") {
		t.Errorf("iphone: %+v", c)
	}

	// A proxy that does not forward WebSocket upgrades is detected.
	e.srv.SetNoWebSocket(true)
	res = e.prov.Check(ctx)
	if res.WebSocket.OK || res.WebSocket.HTTPStatus != 404 || !strings.Contains(res.WebSocket.Detail, "reverse proxy") {
		t.Errorf("broken proxy: %+v", res.WebSocket)
	}
	// The token must not appear in the socket URL.
	tok, _ := e.creds.Get(ctx, e.prov.credAttrs(""))
	for _, r := range e.srv.Requests() {
		if strings.Contains(r, tok.Reveal()) {
			t.Fatalf("token in URL: %s", r)
		}
	}
}
