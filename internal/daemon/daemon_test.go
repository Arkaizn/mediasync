package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/client"
	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/dbustest"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
	"github.com/arkaizn/mediasync/internal/testutil/fakempris"
	"github.com/arkaizn/mediasync/internal/testutil/fakesecrets"
)

const wait = 5 * time.Second

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	t       *testing.T
	bus     *dbustest.Bus
	jf      *fakejellyfin.Server
	secrets *fakesecrets.Service
	paths   config.Paths
	cfg     config.Config
	svc     *Service
	logs    *syncBuffer
	cancel  context.CancelFunc
}

func newHarness(t *testing.T, withJellyfin bool, mutate func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, logs: &syncBuffer{}}
	h.bus = dbustest.Start(t)
	conn, err := h.bus.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	h.secrets, err = fakesecrets.Start(conn)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "msd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h.paths = config.Paths{
		ConfigFile:      filepath.Join(dir, "config.toml"),
		StateFile:       filepath.Join(dir, "state", "state.json"),
		CredentialsFile: filepath.Join(dir, "state", "credentials.json"),
		Socket:          filepath.Join(dir, "run", "mediasync.sock"),
	}
	h.cfg = config.Default()
	h.cfg.General.LogLevel = "debug"
	h.cfg.Jellyfin.PollInterval = config.Duration(500 * time.Millisecond)
	h.cfg.Local.PositionResync = config.Duration(time.Second)
	if withJellyfin {
		h.jf = fakejellyfin.New("alice", "correct horse battery staple")
		t.Cleanup(h.jf.Close)
		h.cfg.Jellyfin.Servers = []config.JellyfinServer{{Name: "home", URL: h.jf.URL}}
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	h.start()
	return h
}

func (h *harness) start() {
	h.t.Helper()
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc, err := New(Options{
		Paths: h.paths, Config: h.cfg, Logger: log, Version: "test",
		SessionBus: h.bus.Connect,
		Host:       &engine.HostInfo{Hostname: "mydesk", Addresses: []string{"10.1.2.3"}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := svc.Start(); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go svc.Run(ctx)
	h.svc, h.cancel = svc, cancel
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.svc != nil {
		h.cancel()
		h.svc.Close()
		h.svc = nil
	}
}

func (h *harness) client() *client.Client {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := client.Dial(ctx, h.paths.Socket)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { c.Close() })
	return c
}

func (h *harness) player(opts fakempris.Options) *fakempris.Player {
	h.t.Helper()
	conn, err := h.bus.Connect()
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	p, err := fakempris.Start(conn, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func call[T any](t *testing.T, c *client.Client, method string, params any) T {
	t.Helper()
	var out T
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.Call(ctx, method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

func callErr(t *testing.T, c *client.Client, method string, params any) *client.Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := c.Call(ctx, method, params, nil)
	ce, ok := err.(*client.Error)
	if !ok {
		t.Fatalf("%s: expected service error, got %v", method, err)
	}
	return ce
}

type snap struct {
	Revision   uint64          `json:"revision"`
	Sessions   []model.Session `json:"sessions"`
	SelectedID *string         `json:"selected_id"`
}

func ids(s snap) []string {
	out := []string{}
	for _, x := range s.Sessions {
		out = append(out, x.ID)
	}
	return out
}

func waitList(t *testing.T, c *client.Client, params any, what string, cond func(snap) bool) snap {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		s := call[snap](t, c, "sessions.list", params)
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; sessions: %v", what, ids(s))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type event struct {
	Revision  uint64         `json:"revision"`
	Type      string         `json:"type"`
	SessionID string         `json:"session_id"`
	Session   *model.Session `json:"session"`
	Reason    string         `json:"reason"`
}

func nextEvent(t *testing.T, c *client.Client, what string, cond func(event) bool) event {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				t.Fatalf("connection closed while waiting for %s", what)
			}
			var ev event
			if err := json.Unmarshal(n.Params, &ev); err != nil {
				t.Fatal(err)
			}
			if cond(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event: %s", what)
		}
	}
}

func feishinOpts() fakempris.Options {
	o := fakempris.FullCaps("Feishin")
	o.Identity = "Feishin"
	o.Status = "Playing"
	o.Tracks = []fakempris.Track{
		{ID: "a", Title: "Track 1", Artists: []string{"Artist A"}, Album: "Album X", LengthUS: 180_000_000},
		{ID: "b", Title: "Track 2", Artists: []string{"Artist A"}, Album: "Album X", LengthUS: 180_000_000},
	}
	return o
}

func TestEndToEndCombinedSessions(t *testing.T) {
	h := newHarness(t, true, nil)
	feishin := h.player(feishinOpts())
	alice := h.jf.UserID("alice")
	phone := h.jf.AddClient(&fakejellyfin.Client{
		UserID: alice, UserName: "alice", Client: "Finamp", DeviceName: "Pixel 7", DeviceID: "phone-1",
		Controllable: true, ApplyCommands: true, SupportedCommands: []string{"SetVolume"},
		ItemID: "item-phone", Title: "Phone Song", Artists: []string{"Someone"}, RunTimeTicks: 2_000_000_000, CanSeek: true, Volume: 50,
		PositionTicks: 50_000_000,
	})
	// Feishin's own Jellyfin session on this machine, same track → duplicate.
	feishinJF := h.jf.AddClient(&fakejellyfin.Client{
		UserID: alice, UserName: "alice", Client: "Feishin", DeviceName: "mydesk", DeviceID: "feishin-desk",
		Controllable: true, ItemID: "item-1", Title: "Track 1", Artists: []string{"Artist A"}, RunTimeTicks: 1_800_000_000,
	})
	// The phone also plays "Track 1" on another device later; must not be merged.

	c := h.client()
	hello := call[map[string]any](t, c, "hello", map[string]any{"client": "test", "api_version": 1})
	if hello["api_version"].(float64) != 1 || hello["schema_version"].(float64) != 1 {
		t.Errorf("hello %v", hello)
	}
	if e := callErr(t, c, "hello", map[string]any{"api_version": 99}); e.Code != string(model.ErrInvalidArgument) {
		t.Errorf("version negotiation: %+v", e)
	}

	// Before login: local works, Jellyfin unauthenticated.
	waitList(t, c, nil, "local feishin", func(s snap) bool { return len(s.Sessions) == 1 && s.Sessions[0].ID == "mpris:Feishin" })
	provs := call[map[string][]model.ProviderStatus](t, c, "providers.status", nil)["providers"]
	if len(provs) != 2 || provs[0].Instance != "jellyfin:home" || provs[0].State != model.ProviderUnauthenticated {
		t.Errorf("providers before login: %+v", provs)
	}

	// Login over IPC.
	res := call[map[string]any](t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "correct horse battery staple"})
	if res["user_name"] != "alice" || res["credential_backend"] != "secret-service" {
		t.Errorf("login %v", res)
	}
	if len(h.secrets.Items()) != 1 {
		t.Errorf("secret not stored: %v", h.secrets.Items())
	}

	s := waitList(t, c, nil, "combined view", func(s snap) bool { return len(s.Sessions) == 2 })
	phoneID := ""
	for _, x := range s.Sessions {
		if x.Origin == model.OriginRemote {
			phoneID = x.ID
		}
	}
	if s.Sessions[0].ID != "mpris:Feishin" || *s.SelectedID != "mpris:Feishin" || phoneID == "" {
		t.Fatalf("combined: %v selected=%v", ids(s), *s.SelectedID)
	}
	if len(s.Sessions[0].Linked) != 1 {
		t.Errorf("feishin's Jellyfin session should be linked as duplicate: %+v", s.Sessions[0])
	}
	all := call[snap](t, c, "sessions.list", map[string]any{"include_hidden": true})
	if len(all.Sessions) != 3 {
		t.Errorf("include_hidden: %v", ids(all))
	}

	// Subscribe and route a command to the phone.
	sub := call[map[string]json.RawMessage](t, c, "subscribe", map[string]any{})
	var subSnap snap
	_ = json.Unmarshal(sub["snapshot"], &subSnap)
	if len(subSnap.Sessions) != 2 {
		t.Errorf("subscribe snapshot %v", ids(subSnap))
	}
	cmd := call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": phoneID, "command": "pause"})
	if cmd.Outcome != model.OutcomeVerified || cmd.SessionID != phoneID {
		t.Errorf("remote pause: %+v", cmd)
	}
	if got := h.jf.ClientState(phone.SessionID).Received; len(got) != 1 || got[0] != "Pause" {
		t.Errorf("phone received %v", got)
	}
	if calls := feishin.Calls(); len(calls) != 0 {
		t.Errorf("local player received remote command: %v", calls)
	}
	nextEvent(t, c, "phone paused", func(e event) bool {
		return e.SessionID == phoneID && e.Session != nil && e.Session.Playback.State == model.StatePaused
	})

	// Default target is the selected (local) session.
	cmd = call[model.CommandResult](t, c, "player.command", map[string]any{"command": "next"})
	if cmd.SessionID != "mpris:Feishin" || cmd.Outcome != model.OutcomeVerified {
		t.Errorf("local next: %+v", cmd)
	}
	if calls := feishin.Calls(); len(calls) != 1 || calls[0] != "Next" {
		t.Errorf("feishin calls %v", calls)
	}
	if got := h.jf.ClientState(phone.SessionID).Received; len(got) != 1 {
		t.Errorf("phone received local command: %v", got)
	}
	// Feishin reports the new track to Jellyfin as well; the duplicate stays merged.
	h.jf.Update(feishinJF.SessionID, func(c *fakejellyfin.Client) { c.Title, c.ItemID = "Track 2", "item-2" })
	waitList(t, c, map[string]any{"include_hidden": true}, "duplicate follows track change", func(s snap) bool {
		for _, x := range s.Sessions {
			if x.Player.Client != nil && *x.Player.Client == "Feishin" {
				return x.Track.Title != nil && *x.Track.Title == "Track 2" && x.Hidden == "duplicate"
			}
		}
		return false
	})
	// Volume and seek on the remote session.
	cmd = call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": phoneID, "command": "set_volume", "volume": 0.8})
	if cmd.Outcome != model.OutcomeVerified {
		t.Errorf("volume: %+v", cmd)
	}
	cmd = call[model.CommandResult](t, c, "player.command", map[string]any{"session_id": phoneID, "command": "seek", "position_ms": 30000})
	if cmd.Outcome != model.OutcomeVerified {
		t.Errorf("seek: %+v", cmd)
	}

	// Mode switch: remote_only → the local session leaves this client's view.
	call[map[string]any](t, c, "settings.update", map[string]any{"mode": "remote_only"})
	nextEvent(t, c, "local removed from view", func(e event) bool { return e.Type == "session.removed" && e.SessionID == "mpris:Feishin" })
	if l := call[snap](t, c, "sessions.list", nil); len(l.Sessions) != 1 || l.Sessions[0].ID != phoneID {
		t.Errorf("remote only: %v", ids(l))
	}
	call[map[string]any](t, c, "settings.update", map[string]any{"mode": "local_only"})
	if l := call[snap](t, c, "sessions.list", nil); len(l.Sessions) != 1 || l.Sessions[0].ID != "mpris:Feishin" {
		t.Errorf("local only: %v", ids(l))
	}
	if e := callErr(t, c, "settings.update", map[string]any{"mode": "everything"}); e.Code != string(model.ErrInvalidArgument) {
		t.Errorf("invalid mode: %+v", e)
	}
	if e := callErr(t, c, "settings.update", map[string]any{"token": "x"}); e.RPCCode != -32602 {
		t.Errorf("unknown setting: %+v", e)
	}
	// Settings persist in the state file (no secrets there).
	stateData, _ := os.ReadFile(h.paths.StateFile)
	if !strings.Contains(string(stateData), "local_only") {
		t.Errorf("state not persisted: %s", stateData)
	}

	// Disable remote providers at runtime.
	call[map[string]any](t, c, "settings.update", map[string]any{"mode": "combined", "remote_enabled": false})
	waitList(t, c, nil, "remote disabled", func(s snap) bool { return len(s.Sessions) == 1 })
	provs = call[map[string][]model.ProviderStatus](t, c, "providers.status", nil)["providers"]
	if len(provs) != 1 || provs[0].Instance != "mpris" {
		t.Errorf("providers after disabling remote: %+v", provs)
	}
	call[map[string]any](t, c, "settings.update", map[string]any{"remote_enabled": true})
	waitList(t, c, nil, "remote re-enabled", func(s snap) bool { return len(s.Sessions) == 2 })

	// Diagnostics contain no secrets.
	diag := call[map[string]any](t, c, "diagnostics.get", nil)
	db, _ := json.Marshal(diag)
	tok := ""
	for _, v := range h.secrets.Items() {
		tok = v
	}
	if strings.Contains(string(db), tok) || strings.Contains(string(db), "correct horse") {
		t.Error("diagnostics leak a secret")
	}
	if diag["credentials"].(map[string]any)["available"] != true {
		t.Errorf("credential health %v", diag["credentials"])
	}

	// Logout revokes and deletes.
	out := call[map[string]any](t, c, "jellyfin.logout", map[string]any{"server": "home"})
	if out["revoked"] != true || out["credential_deleted"] != true {
		t.Errorf("logout %v", out)
	}
	if len(h.secrets.Items()) != 0 || h.jf.Tokens() != 0 {
		t.Error("credential not removed")
	}
	waitList(t, c, nil, "remote gone after logout", func(s snap) bool { return len(s.Sessions) == 1 })

	// Logs never contain the password or token.
	logs := h.logs.String()
	if strings.Contains(logs, "correct horse") || (tok != "" && strings.Contains(logs, tok)) {
		t.Error("logs leak a secret")
	}
}

func TestLocalOnlyWithoutJellyfin(t *testing.T) {
	h := newHarness(t, false, nil)
	h.player(feishinOpts())
	c := h.client()
	s := waitList(t, c, nil, "feishin", func(s snap) bool { return len(s.Sessions) == 1 })
	if s.Sessions[0].Player.Name != "Feishin" {
		t.Errorf("%+v", s.Sessions[0].Player)
	}
	if e := callErr(t, c, "jellyfin.login", map[string]any{"server": "home", "username": "a", "password": "b"}); e.Code != string(model.ErrProviderNotFound) {
		t.Errorf("login without server: %+v", e)
	}
	if e := callErr(t, c, "player.command", map[string]any{"session_id": "jellyfin:x:y", "command": "play"}); e.Code != string(model.ErrSessionNotFound) {
		t.Errorf("missing session: %+v", e)
	}
	sel := call[map[string]any](t, c, "sessions.select", map[string]any{"session_id": "mpris:Feishin"})
	if sel["reason"] != "user" {
		t.Errorf("select %v", sel)
	}
}

func TestPlayerDisappearsWhileSubscribed(t *testing.T) {
	h := newHarness(t, false, nil)
	c := h.client()
	call[map[string]any](t, c, "subscribe", nil)
	p := h.player(feishinOpts())
	nextEvent(t, c, "added", func(e event) bool { return e.Type == "session.added" && e.SessionID == "mpris:Feishin" })
	nextEvent(t, c, "selected", func(e event) bool { return e.Type == "selection.changed" && e.SessionID == "mpris:Feishin" })
	p.SetStatus("Paused")
	nextEvent(t, c, "paused", func(e event) bool {
		return e.Type == "session.updated" && e.Session.Playback.State == model.StatePaused
	})
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	nextEvent(t, c, "removed", func(e event) bool { return e.Type == "session.removed" && e.SessionID == "mpris:Feishin" })
	nextEvent(t, c, "selection cleared", func(e event) bool { return e.Type == "selection.changed" && e.SessionID == "" })
	if e := callErr(t, c, "player.command", map[string]any{"command": "play"}); e.Code != string(model.ErrNoSessionSelected) {
		t.Errorf("no selection: %+v", e)
	}
}

func TestIPCClientDisconnectsAndLimits(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) { c.IPC.MaxClients = 3 })
	h.player(feishinOpts())
	// Abrupt disconnects of subscribed clients release their subscriptions.
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		c, err := client.Dial(ctx, h.paths.Socket)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		call[map[string]any](t, c, "subscribe", nil)
		c.Close()
	}
	deadline := time.Now().Add(wait)
	for h.svc.engine.Diagnostics()["subscribers"].(int) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscriptions leaked: %v", h.svc.engine.Diagnostics()["subscribers"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Client limit.
	var conns []*client.Client
	for i := 0; i < 3; i++ {
		conns = append(conns, h.client())
		call[map[string]any](t, conns[i], "hello", nil)
	}
	raw, err := net.Dial("unix", h.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(raw).ReadString('\n')
	raw.Close()
	if !strings.Contains(line, "too_many_clients") {
		t.Errorf("4th client not rejected: %q", line)
	}
	for _, c := range conns {
		c.Close()
	}
	// The service keeps working.
	time.Sleep(100 * time.Millisecond)
	c := h.client()
	waitList(t, c, nil, "still serving", func(s snap) bool { return len(s.Sessions) == 1 })
}

func TestIPCProtocolErrors(t *testing.T) {
	h := newHarness(t, false, nil)
	raw, err := net.Dial("unix", h.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rd := bufio.NewReader(raw)
	send := func(s string) map[string]any {
		t.Helper()
		if _, err := raw.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
		_ = raw.SetReadDeadline(time.Now().Add(wait))
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(line), &m)
		return m
	}
	code := func(m map[string]any) float64 { return m["error"].(map[string]any)["code"].(float64) }
	if c := code(send(`{not json`)); c != -32700 {
		t.Errorf("parse error code %v", c)
	}
	if c := code(send(`{"jsonrpc":"1.0","id":1,"method":"hello"}`)); c != -32600 {
		t.Errorf("invalid request code %v", c)
	}
	if c := code(send(`{"jsonrpc":"2.0","id":2,"method":"nope"}`)); c != -32601 {
		t.Errorf("method not found code %v", c)
	}
	m := send(`{"jsonrpc":"2.0","id":3,"method":"sessions.list","params":{"bogus":1}}`)
	if c := code(m); c != -32602 {
		t.Errorf("invalid params code %v", c)
	}
	m = send(`{"jsonrpc":"2.0","id":"abc","method":"sessions.get","params":{"session_id":"x"}}`)
	if m["id"] != "abc" || m["error"].(map[string]any)["data"].(map[string]any)["code"] != "session_not_found" {
		t.Errorf("app error %v", m)
	}
	// Oversized message closes the connection; the service keeps running.
	big := `{"jsonrpc":"2.0","id":4,"method":"hello","params":{"client":"` + strings.Repeat("x", 2<<20) + `"}}`
	_, _ = raw.Write([]byte(big + "\n"))
	_ = raw.SetReadDeadline(time.Now().Add(wait))
	for {
		if _, err := rd.ReadString('\n'); err != nil {
			break
		}
	}
	c := h.client()
	call[map[string]any](t, c, "hello", nil)
}

func TestSocketSecurity(t *testing.T) {
	h := newHarness(t, false, nil)
	fi, err := os.Stat(h.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %o", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(h.paths.Socket))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("socket dir mode %o", di.Mode().Perm())
	}
	// A second instance must not steal the socket.
	svc2, err := New(Options{Paths: h.paths, Config: h.cfg, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)), SessionBus: h.bus.Connect})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.Start(); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("second instance: %v", err)
	}
}

func TestServiceRestartClientResnapshots(t *testing.T) {
	h := newHarness(t, false, nil)
	h.player(feishinOpts())
	c := h.client()
	call[map[string]any](t, c, "subscribe", nil)
	h.stop()
	select {
	case <-c.Done():
	case <-time.After(wait):
		t.Fatal("client not notified of service stop")
	}
	// A stale socket file must not prevent a restart.
	h.start()
	c2 := h.client()
	res := call[map[string]json.RawMessage](t, c2, "subscribe", nil)
	var s snap
	_ = json.Unmarshal(res["snapshot"], &s)
	if len(s.Sessions) == 0 {
		// Discovery may not have finished; the event stream completes the picture.
		nextEvent(t, c2, "added after restart", func(e event) bool { return e.Type == "session.added" })
	}
}

func TestReloadKeepsRunningOnInvalidConfig(t *testing.T) {
	h := newHarness(t, false, nil)
	h.player(feishinOpts())
	c := h.client()
	waitList(t, c, nil, "feishin", func(s snap) bool { return len(s.Sessions) == 1 })
	if err := os.WriteFile(h.paths.ConfigFile, []byte("[presentation]\nmode = \"bogus\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := callErr(t, c, "service.reload", nil); e.Code != string(model.ErrInvalidConfig) {
		t.Errorf("reload invalid: %+v", e)
	}
	waitList(t, c, nil, "still running", func(s snap) bool { return len(s.Sessions) == 1 })
	if err := os.WriteFile(h.paths.ConfigFile, []byte("[local]\nignore = [\"feishin\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	call[map[string]any](t, c, "service.reload", nil)
	waitList(t, c, nil, "feishin ignored after reload", func(s snap) bool { return len(s.Sessions) == 0 })
}

func TestCredentialStoreUnavailable(t *testing.T) {
	bus := dbustest.Start(t) // no secret service on this bus
	jf := fakejellyfin.New("alice", "pw")
	defer jf.Close()
	dir, _ := os.MkdirTemp("", "msd")
	defer os.RemoveAll(dir)
	cfg := config.Default()
	cfg.Jellyfin.Servers = []config.JellyfinServer{{Name: "home", URL: jf.URL}}
	paths := config.Paths{StateFile: filepath.Join(dir, "s.json"), Socket: filepath.Join(dir, "m.sock"), CredentialsFile: filepath.Join(dir, "c.json")}
	svc, err := New(Options{Paths: paths, Config: cfg, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)), SessionBus: bus.Connect})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, err := client.Dial(ctx, paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	e := callErr(t, c, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "pw"})
	if e.Code != string(model.ErrCredentialStoreUnavailable) || !strings.Contains(e.Message, "file") {
		t.Errorf("login with no keyring: %+v", e)
	}
	if jf.Tokens() != 0 {
		t.Error("orphaned token on server")
	}
	diag := call[map[string]any](t, c, "diagnostics.get", nil)
	if diag["credentials"].(map[string]any)["available"] != false {
		t.Errorf("diag %v", diag["credentials"])
	}
	// The local provider keeps working without the secret service.
	provs := call[map[string][]model.ProviderStatus](t, c, "providers.status", nil)["providers"]
	found := false
	for _, p := range provs {
		if p.Instance == "mpris" {
			found = true
		}
	}
	if !found {
		t.Error("mpris provider missing")
	}

	// File fallback works when explicitly configured.
	cfg.Credentials.Store = "file"
	paths.Socket = filepath.Join(dir, "m2.sock")
	svc2, err := New(Options{Paths: paths, Config: cfg, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)), SessionBus: bus.Connect})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.Start(); err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	c2, err := client.Dial(ctx, paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	res := call[map[string]any](t, c2, "jellyfin.login", map[string]any{"server": "home", "username": "alice", "password": "pw"})
	if res["credential_backend"] != "file" {
		t.Errorf("file backend: %v", res)
	}
	fi, err := os.Stat(paths.CredentialsFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials file: %v", err)
	}
	_ = dbus.ObjectPath("")
}
