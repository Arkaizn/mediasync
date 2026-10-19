//go:build integration

// Package integration contains optional tests against real software:
//
//   - a real MPRIS player (mpv with the mpv-mpris plugin) on a private
//     session bus: requires `mpv` and the mpris.so script;
//   - a real Jellyfin server: set MEDIASYNC_IT_JELLYFIN_URL,
//     MEDIASYNC_IT_JELLYFIN_USER and MEDIASYNC_IT_JELLYFIN_PASSWORD
//     (scripts/jellyfin-test-server.sh provisions a disposable one);
//   - optionally, read-only discovery on your real session bus with
//     MEDIASYNC_IT_SESSION_BUS=1.
//
// Run with: go test -tags integration -v ./test/integration/
// Tests skip (with a reason) when their prerequisites are missing.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/client"
	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/daemon"
	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/mpris"
	"github.com/arkaizn/mediasync/internal/provider/providertest"
	"github.com/arkaizn/mediasync/internal/testutil/dbustest"
)

const wait = 15 * time.Second

// --- helpers ---------------------------------------------------------------

func startService(t *testing.T, cfg config.Config, bus func() (*dbus.Conn, error)) *client.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "msit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	paths := config.Paths{
		ConfigFile: filepath.Join(dir, "config.toml"), StateFile: filepath.Join(dir, "state.json"),
		CredentialsFile: filepath.Join(dir, "cred", "credentials.json"), Socket: filepath.Join(dir, "s.sock"),
	}
	cfg.Credentials.Store = "file"
	level := slog.LevelInfo
	if os.Getenv("MEDIASYNC_IT_DEBUG") != "" {
		level = slog.LevelDebug
	}
	svc, err := daemon.New(daemon.Options{
		Paths: paths, Config: cfg, Version: "it", SessionBus: bus,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
		Host:   &engine.HostInfo{Hostname: "mediasync-it-host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	t.Cleanup(func() { cancel(); svc.Close() })
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	c, err := client.Dial(dctx, paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func rpc[T any](t *testing.T, c *client.Client, method string, params any) T {
	t.Helper()
	var out T
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Call(ctx, method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

type snap struct {
	Sessions   []model.Session `json:"sessions"`
	SelectedID *string         `json:"selected_id"`
}

func waitSession(t *testing.T, c *client.Client, what string, pick func(model.Session) bool) model.Session {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		s := rpc[snap](t, c, "sessions.list", map[string]any{"include_hidden": true})
		for _, x := range s.Sessions {
			if pick(x) {
				return x
			}
		}
		if time.Now().After(deadline) {
			b, _ := json.MarshalIndent(s.Sessions, "", " ")
			t.Fatalf("timed out waiting for %s; sessions:\n%s", what, b)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func command(t *testing.T, c *client.Client, id string, cmd map[string]any) model.CommandResult {
	t.Helper()
	cmd["session_id"] = id
	res := rpc[model.CommandResult](t, c, "player.command", cmd)
	t.Logf("command %v → %s %s", cmd["command"], res.Outcome, res.Detail)
	return res
}

// writeTone writes a mono 16-bit WAV file with a sine tone.
func writeTone(t *testing.T, path string, freq float64, seconds int) {
	t.Helper()
	const rate = 8000
	n := rate * seconds
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+n*2))
	buf.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(n*2))
	for i := 0; i < n; i++ {
		_ = binary.Write(&buf, binary.LittleEndian, int16(3000*math.Sin(2*math.Pi*freq*float64(i)/rate)))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- real MPRIS player -----------------------------------------------------

func findMPRISScript() string {
	for _, p := range []string{os.Getenv("MEDIASYNC_IT_MPV_MPRIS"), "/usr/lib/mpv-mpris/mpris.so", "/etc/mpv/scripts/mpris.so",
		"/usr/lib/x86_64-linux-gnu/mpv-mpris/mpris.so", "/usr/share/mpv/scripts/mpris.so"} {
		if p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func TestRealMPRISPlayerMPV(t *testing.T) {
	mpv, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("mpv not installed")
	}
	script := findMPRISScript()
	if script == "" {
		t.Skip("mpv-mpris plugin (mpris.so) not found; set MEDIASYNC_IT_MPV_MPRIS")
	}
	bus := dbustest.Start(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "first-tone.wav"), filepath.Join(dir, "second-tone.wav")
	writeTone(t, a, 440, 120)
	writeTone(t, b, 660, 120)

	cmd := exec.Command(mpv, "--no-config", "--script="+script, "--ao=null", "--vo=null", "--no-video",
		"--idle=no", "--keep-open=yes", "--volume=50", "--msg-level=all=warn", a, b)
	cmd.Env = append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS="+bus.Address)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("mpv stderr: %s", stderr.String())
		}
	})

	cfg := config.Default()
	cfg.Remote.Enabled = false
	cfg.Local.PositionResync = config.Duration(time.Second)
	cfg.Commands.VerifyTimeout = config.Duration(3 * time.Second)
	c := startService(t, cfg, bus.Connect)

	s := waitSession(t, c, "mpv via MPRIS", func(s model.Session) bool {
		return s.Provider == "mpris" && s.Track.Title != nil && s.Playback.State == model.StatePlaying
	})
	t.Logf("discovered %s: player=%q title=%q duration=%v caps=%v", s.ID, s.Player.Name, *s.Track.Title, s.Track.DurationMS, s.Capabilities)
	if !strings.HasPrefix(s.ID, "mpris:mpv") || s.Origin != model.OriginLocal || !s.Controllable {
		t.Errorf("unexpected session %+v", s)
	}
	if !strings.Contains(*s.Track.Title, "first-tone") {
		t.Errorf("title %q", *s.Track.Title)
	}
	if s.Track.DurationMS == nil || *s.Track.DurationMS < 119_000 || *s.Track.DurationMS > 121_000 {
		t.Errorf("duration %v", s.Track.DurationMS)
	}
	for _, capab := range []model.Capability{model.CapPlay, model.CapPause, model.CapPlayPause, model.CapNext, model.CapSeek} {
		if !s.Has(capab) {
			t.Errorf("mpv should advertise %s (caps %v)", capab, s.Capabilities)
		}
	}

	if r := command(t, c, s.ID, map[string]any{"command": "pause"}); r.Outcome != model.OutcomeVerified {
		t.Errorf("pause: %+v", r)
	}
	if r := command(t, c, s.ID, map[string]any{"command": "play"}); r.Outcome != model.OutcomeVerified {
		t.Errorf("play: %+v", r)
	}
	if r := command(t, c, s.ID, map[string]any{"command": "seek", "position_ms": 60_000}); r.Outcome != model.OutcomeVerified {
		t.Errorf("seek: %+v", r)
	}
	if r := command(t, c, s.ID, map[string]any{"command": "seek", "offset_ms": -30_000}); r.Outcome != model.OutcomeVerified {
		t.Errorf("relative seek: %+v", r)
	}
	if s.Has(model.CapVolume) {
		if r := command(t, c, s.ID, map[string]any{"command": "set_volume", "volume": 0.3}); r.Outcome != model.OutcomeVerified {
			t.Errorf("volume: %+v", r)
		}
	} else {
		t.Log("mpv-mpris build does not expose a writable Volume; volume not tested")
	}
	if r := command(t, c, s.ID, map[string]any{"command": "next"}); r.Outcome != model.OutcomeVerified {
		t.Errorf("next: %+v", r)
	}
	s = waitSession(t, c, "second track", func(x model.Session) bool {
		return x.ID == s.ID && x.Track.Title != nil && strings.Contains(*x.Track.Title, "second-tone")
	})

	// The player disappears when mpv exits.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(wait)
	for {
		l := rpc[snap](t, c, "sessions.list", map[string]any{"include_hidden": true})
		if len(l.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mpv session not removed after exit")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestRealSessionBusDiscovery lists players on the user's real session bus.
// It is read-only and opt-in.
func TestRealSessionBusDiscovery(t *testing.T) {
	if os.Getenv("MEDIASYNC_IT_SESSION_BUS") == "" {
		t.Skip("set MEDIASYNC_IT_SESSION_BUS=1 to inspect players on your session bus")
	}
	p := mpris.New(mpris.DefaultConfig(), slog.Default())
	sink := providertest.NewSink()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, sink) }()
	sink.WaitFor(t, wait, "running", func(s *providertest.Sink) bool { return s.Status().State == model.ProviderRunning })
	time.Sleep(time.Second)
	for id, s := range sink.Sessions() {
		b, _ := json.MarshalIndent(s, "", "  ")
		t.Logf("%s:\n%s", id, b)
	}
	cancel()
	<-done
}

// --- real Jellyfin ---------------------------------------------------------

type jfEnv struct {
	url, user, password string
}

func jellyfinEnv(t *testing.T) jfEnv {
	e := jfEnv{os.Getenv("MEDIASYNC_IT_JELLYFIN_URL"), os.Getenv("MEDIASYNC_IT_JELLYFIN_USER"), os.Getenv("MEDIASYNC_IT_JELLYFIN_PASSWORD")}
	if e.url == "" || e.user == "" {
		t.Skip("set MEDIASYNC_IT_JELLYFIN_URL, MEDIASYNC_IT_JELLYFIN_USER and MEDIASYNC_IT_JELLYFIN_PASSWORD (see scripts/jellyfin-test-server.sh)")
	}
	e.url = strings.TrimRight(e.url, "/")
	return e
}

// simClient simulates a Jellyfin music client (e.g. a phone app) using the
// same API calls real clients make. With controllable=true it opens the
// WebSocket that makes the server deliver remote-control commands, applies
// received commands and reports its new state, like a real player would.
type simClient struct {
	t        *testing.T
	env      jfEnv
	deviceID string
	client   string
	device   string
	token    string
	ws       *wsConn

	mu       sync.Mutex
	items    []string
	idx      int
	posTicks int64
	paused   bool
	volume   int
	received []string
	stop     chan struct{}
	wg       sync.WaitGroup
}

func (s *simClient) header() string {
	h := fmt.Sprintf(`MediaBrowser Client="%s", Device="%s", DeviceId="%s", Version="1.0"`, s.client, s.device, s.deviceID)
	if s.token != "" {
		h += fmt.Sprintf(`, Token="%s"`, s.token)
	}
	return h
}

func (s *simClient) do(method, path string, body any, out any) int {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.env.url+path, rd)
	req.Header.Set("Authorization", s.header())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == 200 {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func newSimClient(t *testing.T, env jfEnv, client, device string, items []string, controllable bool) *simClient {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	s := &simClient{t: t, env: env, client: client, device: device, deviceID: "mediasync-it-" + hex.EncodeToString(b),
		items: items, posTicks: 100_000_000, volume: 50, stop: make(chan struct{})}
	var auth struct{ AccessToken string }
	if code := s.do("POST", "/Users/AuthenticateByName", map[string]string{"Username": env.user, "Pw": env.password}, &auth); code != 200 {
		t.Fatalf("sim client auth: HTTP %d", code)
	}
	s.token = auth.AccessToken
	if controllable {
		s.do("POST", "/Sessions/Capabilities/Full", map[string]any{
			"PlayableMediaTypes": []string{"Audio"}, "SupportedCommands": []string{"SetVolume", "VolumeUp", "VolumeDown"},
			"SupportsMediaControl": true,
		}, nil)
		wsURL := "ws" + strings.TrimPrefix(env.url, "http") + "/socket"
		ws, err := dialWS(wsURL, http.Header{"Authorization": {s.header()}})
		if err != nil {
			t.Fatalf("sim client websocket: %v", err)
		}
		s.ws = ws
		s.wg.Add(1)
		go s.readLoop()
	}
	s.report("/Sessions/Playing", "")
	s.wg.Add(1)
	go s.progressLoop()
	t.Cleanup(s.Close)
	return s
}

func (s *simClient) state() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"ItemId": s.items[s.idx], "PositionTicks": s.posTicks, "IsPaused": s.paused, "CanSeek": true,
		"VolumeLevel": s.volume, "PlayMethod": "DirectPlay", "PlaySessionId": s.deviceID,
	}
}

func (s *simClient) report(path, event string) {
	st := s.state()
	if event != "" {
		st["EventName"] = event
	}
	s.do("POST", path, st, nil)
}

func (s *simClient) progressLoop() {
	defer s.wg.Done()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			if !s.paused {
				s.posTicks += 50_000_000
			}
			s.mu.Unlock()
			s.report("/Sessions/Playing/Progress", "TimeUpdate")
		}
	}
}

func (s *simClient) readLoop() {
	defer s.wg.Done()
	for {
		msg, err := s.ws.ReadText()
		if err != nil {
			return
		}
		var m struct {
			MessageType string
			Data        json.RawMessage
		}
		if json.Unmarshal([]byte(msg), &m) != nil {
			continue
		}
		switch m.MessageType {
		case "ForceKeepAlive":
			_ = s.ws.WriteText(`{"MessageType":"KeepAlive"}`)
		case "Playstate":
			var d struct {
				Command           string
				SeekPositionTicks *int64
			}
			_ = json.Unmarshal(m.Data, &d)
			s.mu.Lock()
			s.received = append(s.received, d.Command)
			newItem := false
			switch d.Command {
			case "Pause":
				s.paused = true
			case "Unpause":
				s.paused = false
			case "PlayPause":
				s.paused = !s.paused
			case "Seek":
				if d.SeekPositionTicks != nil {
					s.posTicks = *d.SeekPositionTicks
				}
			case "NextTrack":
				if s.idx+1 < len(s.items) {
					s.idx++
					s.posTicks = 0
					newItem = true
				}
			case "PreviousTrack":
				if s.idx > 0 {
					s.idx--
				}
				s.posTicks = 0
				newItem = true
			}
			s.mu.Unlock()
			if newItem {
				s.report("/Sessions/Playing", "")
			} else {
				s.report("/Sessions/Playing/Progress", d.Command)
			}
		case "GeneralCommand":
			var d struct {
				Name      string
				Arguments map[string]string
			}
			_ = json.Unmarshal(m.Data, &d)
			s.mu.Lock()
			s.received = append(s.received, d.Name)
			if d.Name == "SetVolume" {
				fmt.Sscanf(d.Arguments["Volume"], "%d", &s.volume)
			}
			s.mu.Unlock()
			s.report("/Sessions/Playing/Progress", "VolumeChange")
		}
	}
}

func (s *simClient) Received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.received...)
}

// Disconnect closes the control WebSocket (like an app being killed).
func (s *simClient) Disconnect() {
	if s.ws != nil {
		s.ws.Close()
	}
}

func (s *simClient) Close() {
	select {
	case <-s.stop:
		return
	default:
	}
	close(s.stop)
	s.report("/Sessions/Playing/Stopped", "")
	s.Disconnect()
	s.do("POST", "/Sessions/Logout", nil, nil)
	s.wg.Wait()
}

func audioItems(t *testing.T, env jfEnv) []string {
	t.Helper()
	probe := &simClient{t: t, env: env, client: "mediasync-it", device: "probe", deviceID: "mediasync-it-probe"}
	var auth struct{ AccessToken string }
	if code := probe.do("POST", "/Users/AuthenticateByName", map[string]string{"Username": env.user, "Pw": env.password}, &auth); code != 200 {
		t.Fatalf("auth: HTTP %d", code)
	}
	probe.token = auth.AccessToken
	defer probe.do("POST", "/Sessions/Logout", nil, nil)
	var res struct {
		Items []struct{ Id string }
	}
	probe.do("GET", "/Items?recursive=true&includeItemTypes=Audio&limit=10", nil, &res)
	if len(res.Items) < 2 {
		t.Skip("the Jellyfin server needs at least two audio items (scripts/jellyfin-test-server.sh creates some)")
	}
	out := []string{}
	for _, it := range res.Items {
		out = append(out, it.Id)
	}
	return out
}

func TestRealJellyfin(t *testing.T) {
	env := jellyfinEnv(t)
	items := audioItems(t, env)

	cfg := config.Default()
	cfg.Local.Enabled = false
	cfg.Jellyfin.PollInterval = config.Duration(time.Second)
	cfg.Jellyfin.Servers = []config.JellyfinServer{{Name: "it", URL: env.url}}
	cfg.Commands.RemoteVerifyTimeout = config.Duration(8 * time.Second)
	c := startService(t, cfg, nil)

	// Wrong password.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := c.Call(ctx, "jellyfin.login", map[string]any{"server": "it", "username": env.user, "password": env.password + "-wrong"}, nil)
	cancel()
	if ce, ok := err.(*client.Error); !ok || ce.Code != string(model.ErrAuthFailed) {
		t.Errorf("wrong password: %v", err)
	}
	login := rpc[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "it", "username": env.user, "password": env.password})
	t.Logf("logged in: %v", login)

	phone := newSimClient(t, env, "Finamp", "IT Phone", items, true)
	viewOnly := newSimClient(t, env, "Jellyfin Web", "IT Browser", items[1:], false)

	ps := waitSession(t, c, "controllable phone", func(s model.Session) bool {
		return s.Player.DeviceID != nil && *s.Player.DeviceID == phone.deviceID && s.Controllable
	})
	vs := waitSession(t, c, "view-only client", func(s model.Session) bool {
		return s.Player.DeviceID != nil && *s.Player.DeviceID == viewOnly.deviceID
	})
	b, _ := json.MarshalIndent(ps, "", "  ")
	t.Logf("phone session from real Jellyfin:\n%s", b)
	if ps.Origin != model.OriginRemote || ps.Kind != model.KindRemoteControllable || ps.Track.Title == nil || ps.Track.TrackID == nil || *ps.Track.TrackID != items[0] {
		t.Errorf("phone session %+v", ps)
	}
	if !ps.Has(model.CapSeek) || !ps.Has(model.CapVolume) || !ps.Has(model.CapPause) {
		t.Errorf("phone caps %v", ps.Capabilities)
	}
	if ps.Server == nil || ps.Server.ID == nil || ps.Server.Version == nil {
		t.Errorf("server info %+v", ps.Server)
	} else {
		t.Logf("server version %s", *ps.Server.Version)
	}
	if vs.Controllable || len(vs.Capabilities) != 0 || vs.Kind != model.KindRemoteViewOnly {
		t.Errorf("view-only session %+v", vs)
	}

	// Commands reach exactly the phone and are verified from the server's state.
	for _, tc := range []struct {
		cmd  map[string]any
		want string
	}{
		{map[string]any{"command": "pause"}, "Pause"},
		{map[string]any{"command": "play"}, "Unpause"},
		{map[string]any{"command": "seek", "position_ms": 60_000}, "Seek"},
		{map[string]any{"command": "set_volume", "volume": 0.3}, "SetVolume"},
		{map[string]any{"command": "next"}, "NextTrack"},
	} {
		r := command(t, c, ps.ID, tc.cmd)
		if r.Outcome != model.OutcomeVerified {
			t.Errorf("%v: outcome %s (%s)", tc.cmd["command"], r.Outcome, r.Detail)
		}
		got := phone.Received()
		if len(got) == 0 || got[len(got)-1] != tc.want {
			t.Errorf("%v: phone received %v", tc.cmd["command"], got)
		}
	}
	if got := viewOnly.Received(); len(got) != 0 {
		t.Errorf("view-only client received %v", got)
	}

	// Commands to a view-only session are refused before reaching the server.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	err = c.Call(ctx, "player.command", map[string]any{"session_id": vs.ID, "command": "pause"}, nil)
	cancel()
	if ce, ok := err.(*client.Error); !ok || ce.Code != string(model.ErrNotControllable) {
		t.Errorf("view-only command: %v", err)
	}

	// Disconnecting the phone's control channel removes it (the server drops
	// the session when its WebSocket closes).
	phone.Disconnect()
	deadline := time.Now().Add(wait)
	for {
		l := rpc[snap](t, c, "sessions.list", map[string]any{"include_hidden": true})
		found := false
		for _, s := range l.Sessions {
			if s.ID == ps.ID && s.Controllable {
				found = true
			}
		}
		if !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("phone still controllable after its WebSocket closed")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Logout revokes the token on the server.
	out := rpc[map[string]any](t, c, "jellyfin.logout", map[string]any{"server": "it"})
	if out["revoked"] != true || out["credential_deleted"] != true {
		t.Errorf("logout %v", out)
	}
	provs := rpc[map[string][]model.ProviderStatus](t, c, "providers.status", nil)["providers"]
	if len(provs) != 1 || provs[0].State != model.ProviderUnauthenticated {
		t.Errorf("after logout %+v", provs)
	}
}
