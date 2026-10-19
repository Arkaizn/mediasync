package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/model"
)

func TestDefaultsAndMissingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Local.Enabled || !cfg.Remote.Enabled || cfg.Presentation.Mode != "combined" || cfg.Credentials.Store != "secret-service" {
		t.Errorf("defaults %+v", cfg)
	}
	if len(cfg.Jellyfin.Servers) != 0 {
		t.Error("no servers expected by default")
	}
}

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../examples/config.toml")
	if err != nil {
		t.Skip("example config not present yet")
	}
	cfg, err := Parse(data, "example")
	if err != nil {
		t.Fatalf("example config invalid: %v", err)
	}
	if len(cfg.Jellyfin.Servers) == 0 {
		t.Error("example should show a server")
	}
}

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(`
[general]
log_level = "debug"
[presentation]
mode = "local_only"
preferred_player = "feishin"
prefer_origin = "remote"
[local]
ignore = ["playerctld", "firefox"]
position_resync = "2s"
[dedup]
use_address = false
[[dedup.links]]
local_player = "feishin"
remote_client = "Feishin"
[jellyfin]
poll_interval = "3s"
[[jellyfin.servers]]
name = "home"
url = "https://jellyfin.example.org"
ignore_clients = ["Jellyfin Web"]
[credentials]
store = "file"
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Presentation.Mode != "local_only" || cfg.Local.PositionResync.D() != 2*time.Second || cfg.Jellyfin.PollInterval.D() != 3*time.Second {
		t.Errorf("%+v", cfg)
	}
	s := cfg.EngineSettings()
	if s.Mode != engine.ModeLocalOnly || s.PreferOrigin != engine.PreferRemote || len(s.DedupLinks) != 1 || s.DedupLinks[0].RemoteClient != "Feishin" {
		t.Errorf("settings %+v", s)
	}
	if len(cfg.Local.Ignore) != 2 || cfg.Dedup.UseAddress {
		t.Errorf("local %+v dedup %+v", cfg.Local, cfg.Dedup)
	}
	if !cfg.Jellyfin.Servers[0].IsEnabled() {
		t.Error("server should default to enabled")
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"syntax":         "[general\nlog_level=1",
		"unknown key":    "[general]\nverbose = true",
		"bad mode":       "[presentation]\nmode = \"everything\"",
		"bad duration":   "[jellyfin]\npoll_interval = \"soon\"",
		"too fast poll":  "[jellyfin]\npoll_interval = \"10ms\"",
		"bad url":        "[[jellyfin.servers]]\nname = \"a\"\nurl = \"ftp://x\"",
		"url with creds": "[[jellyfin.servers]]\nname = \"a\"\nurl = \"https://u:p@x\"",
		"dup names":      "[[jellyfin.servers]]\nname = \"a\"\nurl = \"https://x\"\n[[jellyfin.servers]]\nname = \"a\"\nurl = \"https://y\"",
		"bad name":       "[[jellyfin.servers]]\nname = \"a b\"\nurl = \"https://x\"",
		"bad store":      "[credentials]\nstore = \"plaintext\"",
		"relative path":  "[ipc]\nsocket_path = \"x.sock\"",
		"bad link":       "[[dedup.links]]\nlocal_player = \"x\"",
		"bad log level":  "[general]\nlog_level = \"loud\"",
	}
	for name, txt := range cases {
		if _, err := Parse([]byte(txt), "t"); model.CodeOf(err) != model.ErrInvalidConfig {
			t.Errorf("%s: want invalid_config, got %v", name, err)
		}
	}
}

func TestCredentialsInConfigRejected(t *testing.T) {
	for _, txt := range []string{
		"[[jellyfin.servers]]\nname = \"a\"\nurl = \"https://x\"\ntoken = \"abc123\"",
		"[[jellyfin.servers]]\nname = \"a\"\nurl = \"https://x\"\npassword = \"hunter2\"",
		"[jellyfin]\napi_key = \"zzz\"",
	} {
		_, err := Parse([]byte(txt), "t")
		if model.CodeOf(err) != model.ErrInvalidConfig || !strings.Contains(err.Error(), "credentials must not be stored") {
			t.Errorf("want credential rejection, got %v", err)
		}
		for _, secret := range []string{"abc123", "hunter2", "zzz"} {
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("error echoes secret: %v", err)
			}
		}
	}
}

func TestStateRoundTripAndOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state", "state.json")
	st, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.DeviceID, "mediasync-") {
		t.Errorf("device id %q", st.DeviceID)
	}
	st2, _ := LoadState(p)
	if st2.DeviceID != st.DeviceID {
		t.Error("device id not stable")
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %o", fi.Mode().Perm())
	}
	mode := "remote_only"
	off := false
	st.Overrides = st.Overrides.Merge(Overrides{Mode: &mode, LocalEnabled: &off})
	if err := SaveState(p, st); err != nil {
		t.Fatal(err)
	}
	st3, _ := LoadState(p)
	s, local, remote := st3.Overrides.Apply(Default().EngineSettings(), true, true)
	if s.Mode != engine.ModeRemoteOnly || local || !remote {
		t.Errorf("apply: %+v %v %v", s, local, remote)
	}
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p); model.CodeOf(err) != model.ErrInvalidConfig {
		t.Errorf("corrupt state: %v", err)
	}
}

func TestPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	t.Setenv("XDG_STATE_HOME", "/st")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	p := DefaultPaths()
	if p.ConfigFile != "/cfg/mediasync/config.toml" || p.StateFile != "/st/mediasync/state.json" || p.Socket != "/run/user/1000/mediasync/mediasync.sock" {
		t.Errorf("%+v", p)
	}
	cfg := Default()
	cfg.IPC.SocketPath = "/tmp/x.sock"
	if cfg.ResolvePaths(p).Socket != "/tmp/x.sock" {
		t.Error("override")
	}
}
