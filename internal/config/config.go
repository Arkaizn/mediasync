// Package config loads the user configuration (TOML, never rewritten by the
// service) and the service state file (JSON, written by the service for
// settings changed over IPC and the stable device id).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/jellyfin"
)

// Duration is a time.Duration that decodes from strings like "2s".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q", string(b))
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the user configuration.
type Config struct {
	General      General      `toml:"general"`
	Presentation Presentation `toml:"presentation"`
	Local        Local        `toml:"local"`
	Remote       Remote       `toml:"remote"`
	Dedup        Dedup        `toml:"dedup"`
	Jellyfin     Jellyfin     `toml:"jellyfin"`
	Credentials  Credentials  `toml:"credentials"`
	IPC          IPC          `toml:"ipc"`
	Commands     Commands     `toml:"commands"`
}

type General struct {
	// LogLevel is debug, info, warn or error.
	LogLevel string `toml:"log_level"`
	// LogFormat is text or json.
	LogFormat string `toml:"log_format"`
}

type Presentation struct {
	Mode            string `toml:"mode"`
	PreferredPlayer string `toml:"preferred_player"`
	PreferOrigin    string `toml:"prefer_origin"`
	ShowViewOnly    bool   `toml:"show_view_only"`
	ShowStale       bool   `toml:"show_stale"`
}

type Local struct {
	Enabled        bool     `toml:"enabled"`
	Include        []string `toml:"include"`
	Ignore         []string `toml:"ignore"`
	PositionResync Duration `toml:"position_resync"`
	CallTimeout    Duration `toml:"call_timeout"`
}

type Remote struct {
	Enabled bool `toml:"enabled"`
}

type Dedup struct {
	Enabled bool               `toml:"enabled"`
	Links   []engine.DedupLink `toml:"links"`
	// UseAddress enables same-host detection from the client address the
	// server reports. Disable it when the server sees every client through
	// one local address (e.g. Docker port forwarding with userland proxy).
	UseAddress bool `toml:"use_address"`
}

type Jellyfin struct {
	PollInterval            Duration         `toml:"poll_interval"`
	RequestTimeout          Duration         `toml:"request_timeout"`
	StaleAfter              Duration         `toml:"stale_after"`
	StalePausedAfter        Duration         `toml:"stale_paused_after"`
	RemoveDisconnectedAfter Duration         `toml:"remove_disconnected_after"`
	Servers                 []JellyfinServer `toml:"servers"`
}

type JellyfinServer struct {
	Name          string   `toml:"name"`
	URL           string   `toml:"url"`
	CAFile        string   `toml:"ca_file"`
	AllUsers      bool     `toml:"all_users"`
	IgnoreClients []string `toml:"ignore_clients"`
	IgnoreDevices []string `toml:"ignore_devices"`
	Enabled       *bool    `toml:"enabled"`
}

// IsEnabled reports whether the server is enabled (default true).
func (s JellyfinServer) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

type Credentials struct {
	// Store is secret-service (default) or file.
	Store    string `toml:"store"`
	FilePath string `toml:"file_path"`
}

type IPC struct {
	SocketPath string `toml:"socket_path"`
	MaxClients int    `toml:"max_clients"`
}

type Commands struct {
	Timeout             Duration `toml:"timeout"`
	VerifyTimeout       Duration `toml:"verify_timeout"`
	RemoteVerifyTimeout Duration `toml:"remote_verify_timeout"`
}

// Paths are the resolved file locations.
type Paths struct {
	ConfigFile      string `json:"config_file"`
	StateFile       string `json:"state_file"`
	CredentialsFile string `json:"credentials_file"`
	Socket          string `json:"socket"`
}

// Default returns the default configuration.
func Default() Config {
	return Config{
		General:      General{LogLevel: "info", LogFormat: "text"},
		Presentation: Presentation{Mode: "combined", PreferOrigin: "local", ShowViewOnly: true, ShowStale: true},
		Local: Local{
			Enabled: true, Ignore: []string{"playerctld"},
			PositionResync: Duration(5 * time.Second), CallTimeout: Duration(3 * time.Second),
		},
		Remote: Remote{Enabled: true},
		Dedup:  Dedup{Enabled: true, UseAddress: true},
		Jellyfin: Jellyfin{
			PollInterval: Duration(2 * time.Second), RequestTimeout: Duration(10 * time.Second),
			StaleAfter: Duration(90 * time.Second), StalePausedAfter: Duration(15 * time.Minute),
			RemoveDisconnectedAfter: Duration(5 * time.Minute),
		},
		Credentials: Credentials{Store: "secret-service"},
		IPC:         IPC{MaxClients: 32},
		Commands: Commands{
			Timeout: Duration(5 * time.Second), VerifyTimeout: Duration(1500 * time.Millisecond),
			RemoteVerifyTimeout: Duration(5 * time.Second),
		},
	}
}

// DefaultPaths resolves XDG locations.
func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	xdg := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, fallback)
	}
	cfgDir := filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "mediasync")
	stateDir := filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "mediasync")
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" || !filepath.IsAbs(runtime) {
		runtime = filepath.Join(os.TempDir(), fmt.Sprintf("mediasync-%d", os.Getuid()))
	} else {
		runtime = filepath.Join(runtime, "mediasync")
	}
	return Paths{
		ConfigFile:      filepath.Join(cfgDir, "config.toml"),
		StateFile:       filepath.Join(stateDir, "state.json"),
		CredentialsFile: filepath.Join(stateDir, "credentials.json"),
		Socket:          filepath.Join(runtime, "mediasync.sock"),
	}
}

// forbiddenKey matches keys that would hold credentials.
var forbiddenKey = regexp.MustCompile(`(?i)(^|\.)(token|access_token|api_key|apikey|password|pw|secret)$`)

// Load reads the config file. A missing file yields the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, cfg.Validate()
	}
	if err != nil {
		return cfg, model.Wrap(model.ErrInvalidConfig, err, "cannot read "+path)
	}
	return Parse(data, path)
}

// Parse parses configuration text.
func Parse(data []byte, source string) (Config, error) {
	cfg := Default()
	// Decoding into a fresh slice: TOML arrays replace defaults entirely.
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		var perr toml.ParseError
		if errors.As(err, &perr) {
			return cfg, model.Errorf(model.ErrInvalidConfig, "%s: line %d: %s", source, perr.Position.Line, perr.Message)
		}
		return cfg, model.Errorf(model.ErrInvalidConfig, "%s: %s", source, err.Error())
	}
	var bad, secret []string
	for _, k := range md.Undecoded() {
		if forbiddenKey.MatchString(k.String()) {
			secret = append(secret, k.String())
		} else {
			bad = append(bad, k.String())
		}
	}
	for _, k := range md.Keys() {
		if forbiddenKey.MatchString(k.String()) {
			secret = append(secret, k.String())
		}
	}
	if len(secret) > 0 {
		sort.Strings(secret)
		return cfg, model.Errorf(model.ErrInvalidConfig,
			"%s: credentials must not be stored in the configuration file (found %s); use `mediasyncctl jellyfin login`", source, strings.Join(secret, ", "))
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return cfg, model.Errorf(model.ErrInvalidConfig, "%s: unknown keys: %s", source, strings.Join(bad, ", "))
	}
	if err := cfg.Validate(); err != nil {
		return cfg, model.Errorf(model.ErrInvalidConfig, "%s: %s", source, model.AsError(err).Message)
	}
	return cfg, nil
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// Validate checks the configuration.
func (c Config) Validate() error {
	switch c.General.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return model.Errorf(model.ErrInvalidConfig, "general.log_level must be debug, info, warn or error")
	}
	switch c.General.LogFormat {
	case "text", "json":
	default:
		return model.Errorf(model.ErrInvalidConfig, "general.log_format must be text or json")
	}
	if err := c.EngineSettings().Validate(); err != nil {
		return model.Errorf(model.ErrInvalidConfig, "presentation/dedup: %s", model.AsError(err).Message)
	}
	if c.Local.PositionResync.D() != 0 && c.Local.PositionResync.D() < 500*time.Millisecond {
		return model.Errorf(model.ErrInvalidConfig, "local.position_resync must be 0 (off) or at least 500ms")
	}
	if err := within("local.call_timeout", c.Local.CallTimeout.D(), 100*time.Millisecond, time.Minute); err != nil {
		return err
	}
	j := c.Jellyfin
	for _, chk := range []struct {
		name     string
		v        time.Duration
		min, max time.Duration
	}{
		{"jellyfin.poll_interval", j.PollInterval.D(), 500 * time.Millisecond, 5 * time.Minute},
		{"jellyfin.request_timeout", j.RequestTimeout.D(), time.Second, 2 * time.Minute},
		{"jellyfin.stale_after", j.StaleAfter.D(), 10 * time.Second, 24 * time.Hour},
		{"jellyfin.stale_paused_after", j.StalePausedAfter.D(), 10 * time.Second, 24 * time.Hour},
		{"jellyfin.remove_disconnected_after", j.RemoveDisconnectedAfter.D(), 5 * time.Second, 24 * time.Hour},
		{"commands.timeout", c.Commands.Timeout.D(), 100 * time.Millisecond, time.Minute},
		{"commands.verify_timeout", c.Commands.VerifyTimeout.D(), 0, time.Minute},
		{"commands.remote_verify_timeout", c.Commands.RemoteVerifyTimeout.D(), 0, time.Minute},
	} {
		if err := within(chk.name, chk.v, chk.min, chk.max); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	for i, s := range j.Servers {
		if !serverName.MatchString(s.Name) {
			return model.Errorf(model.ErrInvalidConfig, "jellyfin.servers[%d].name must match %s", i, serverName)
		}
		if names[s.Name] {
			return model.Errorf(model.ErrInvalidConfig, "duplicate jellyfin server name %q", s.Name)
		}
		names[s.Name] = true
		if _, err := jellyfin.ParseServerURL(s.URL); err != nil {
			return model.Errorf(model.ErrInvalidConfig, "jellyfin.servers[%d] (%s): %s", i, s.Name, model.AsError(err).Message)
		}
		if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
			return model.Errorf(model.ErrInvalidConfig, "jellyfin.servers[%d].ca_file must be an absolute path", i)
		}
	}
	switch c.Credentials.Store {
	case "secret-service", "file":
	default:
		return model.Errorf(model.ErrInvalidConfig, "credentials.store must be secret-service or file")
	}
	if c.Credentials.FilePath != "" && !filepath.IsAbs(c.Credentials.FilePath) {
		return model.Errorf(model.ErrInvalidConfig, "credentials.file_path must be an absolute path")
	}
	if c.IPC.SocketPath != "" && !filepath.IsAbs(c.IPC.SocketPath) {
		return model.Errorf(model.ErrInvalidConfig, "ipc.socket_path must be an absolute path")
	}
	if c.IPC.MaxClients < 1 || c.IPC.MaxClients > 1024 {
		return model.Errorf(model.ErrInvalidConfig, "ipc.max_clients must be between 1 and 1024")
	}
	return nil
}

func within(name string, v, lo, hi time.Duration) error {
	if v < lo || v > hi {
		return model.Errorf(model.ErrInvalidConfig, "%s must be between %s and %s (got %s)", name, lo, hi, v)
	}
	return nil
}

// EngineSettings converts presentation settings.
func (c Config) EngineSettings() engine.Settings {
	return engine.Settings{
		Mode:            engine.Mode(c.Presentation.Mode),
		PreferredPlayer: c.Presentation.PreferredPlayer,
		PreferOrigin:    engine.OriginPreference(c.Presentation.PreferOrigin),
		ShowViewOnly:    c.Presentation.ShowViewOnly,
		ShowStale:       c.Presentation.ShowStale,
		Dedup:           c.Dedup.Enabled,
		DedupLinks:      c.Dedup.Links,
	}
}

// ResolvePaths applies config overrides to default paths.
func (c Config) ResolvePaths(p Paths) Paths {
	if c.IPC.SocketPath != "" {
		p.Socket = c.IPC.SocketPath
	}
	if c.Credentials.FilePath != "" {
		p.CredentialsFile = c.Credentials.FilePath
	}
	return p
}
