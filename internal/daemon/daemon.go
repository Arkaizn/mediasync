// Package daemon wires configuration, credential storage, the engine,
// providers and the IPC server together.
package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/ipc"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/jellyfin"
	"github.com/arkaizn/mediasync/internal/provider/mpris"
	"github.com/arkaizn/mediasync/internal/secrets"
)

// APIVersion is the major version of the IPC API.
const APIVersion = 1

// Options configure the service.
type Options struct {
	Paths   config.Paths
	Config  config.Config
	Logger  *slog.Logger
	Version string

	// Test hooks.
	SessionBus  func() (*dbus.Conn, error)
	Credentials secrets.Store
	Host        *engine.HostInfo
	HTTPClient  *http.Client
}

// Service is the running bridge.
type Service struct {
	opts    Options
	log     *slog.Logger
	engine  *engine.Engine
	ipc     *ipc.Server
	creds   secrets.Store
	started time.Time

	mu            sync.Mutex
	cfg           config.Config
	state         config.State
	localEnabled  bool
	remoteEnabled bool
	jf            map[string]*jellyfin.Provider // by server name
	running       map[string]bool               // provider instances added to the engine
}

// New creates the service (nothing is started yet).
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	st, err := config.LoadState(opts.Paths.StateFile)
	if err != nil {
		return nil, err
	}
	host := engine.DetectHost()
	if opts.Host != nil {
		host = *opts.Host
	}
	host.IgnoreAddresses = !opts.Config.Dedup.UseAddress
	settings, localEnabled, remoteEnabled := st.Overrides.Apply(opts.Config.EngineSettings(), opts.Config.Local.Enabled, opts.Config.Remote.Enabled)
	if err := settings.Validate(); err != nil {
		opts.Logger.Warn("ignoring invalid runtime overrides from state file", "err", err)
		st.Overrides = config.Overrides{}
		settings, localEnabled, remoteEnabled = opts.Config.EngineSettings(), opts.Config.Local.Enabled, opts.Config.Remote.Enabled
	}
	eng := engine.New(engine.Options{
		Settings:            settings,
		Host:                host,
		Logger:              opts.Logger,
		CommandTimeout:      opts.Config.Commands.Timeout.D(),
		VerifyTimeout:       opts.Config.Commands.VerifyTimeout.D(),
		RemoteVerifyTimeout: opts.Config.Commands.RemoteVerifyTimeout.D(),
	})
	creds := opts.Credentials
	if creds == nil {
		creds = newCredentialStore(opts.Config, opts.Paths, opts.SessionBus)
	}
	s := &Service{
		opts: opts, log: opts.Logger, engine: eng, creds: creds, started: time.Now(),
		cfg: opts.Config, state: st, localEnabled: localEnabled, remoteEnabled: remoteEnabled,
		jf: map[string]*jellyfin.Provider{}, running: map[string]bool{},
	}
	s.ipc = ipc.NewServer(opts.Paths.Socket, opts.Config.IPC.MaxClients, opts.Logger)
	s.registerAPI()
	return s, nil
}

func newCredentialStore(cfg config.Config, paths config.Paths, bus func() (*dbus.Conn, error)) secrets.Store {
	if cfg.Credentials.Store == "file" {
		return secrets.NewFile(paths.CredentialsFile)
	}
	ss := secrets.NewSecretService()
	if bus != nil {
		ss.Connect = bus
	}
	return ss
}

// Engine exposes the engine (tests).
func (s *Service) Engine() *engine.Engine { return s.engine }

// Start opens the socket and starts providers.
func (s *Service) Start() error {
	if err := s.ipc.Listen(); err != nil {
		return err
	}
	jf, err := s.buildJellyfin(s.cfg, s.state.DeviceID)
	if err != nil {
		s.ipc.Close()
		return err
	}
	s.mu.Lock()
	s.jf = jf
	s.mu.Unlock()
	s.applyProviders()
	go func() {
		if err := s.ipc.Serve(); err != nil {
			s.log.Error("ipc server stopped", "err", err)
		}
	}()
	s.log.Info("mediasync started", "version", s.opts.Version, "socket", s.ipc.Path(),
		"local", s.localEnabled, "remote", s.remoteEnabled, "credential_store", s.creds.Backend())
	return nil
}

// Run blocks until ctx is cancelled, ticking the engine.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.engine.Tick()
		}
	}
}

// Close stops everything.
func (s *Service) Close() {
	s.ipc.Close()
	s.engine.Close()
	if c, ok := s.creds.(interface{ Close() }); ok {
		c.Close()
	}
	s.log.Info("mediasync stopped")
}

// buildJellyfin creates (but does not start) one provider per configured server.
func (s *Service) buildJellyfin(cfg config.Config, deviceID string) (map[string]*jellyfin.Provider, error) {
	out := map[string]*jellyfin.Provider{}
	host, _ := os.Hostname()
	for _, srv := range cfg.Jellyfin.Servers {
		p, err := jellyfin.New(jellyfin.Config{
			Name: srv.Name, URL: srv.URL, DeviceID: deviceID, DeviceName: host, Version: s.opts.Version,
			PollInterval: cfg.Jellyfin.PollInterval.D(), RequestTimeout: cfg.Jellyfin.RequestTimeout.D(),
			StaleAfter: cfg.Jellyfin.StaleAfter.D(), StalePausedAfter: cfg.Jellyfin.StalePausedAfter.D(),
			RemoveDisconnectedAfter: cfg.Jellyfin.RemoveDisconnectedAfter.D(),
			AllUsers:                srv.AllUsers, IgnoreClients: srv.IgnoreClients, IgnoreDevices: srv.IgnoreDevices,
			CAFile: srv.CAFile, HTTPClient: s.opts.HTTPClient, Credentials: s.creds,
		}, s.log)
		if err != nil {
			return nil, model.Errorf(model.ErrInvalidConfig, "jellyfin server %q: %s", srv.Name, model.AsError(err).Message)
		}
		out[srv.Name] = p
	}
	return out, nil
}

// applyProviders starts/stops providers to match the enablement flags.
func (s *Service) applyProviders() {
	s.mu.Lock()
	want := map[string]func(){}
	if s.localEnabled {
		cfg := mpris.Config{
			Include: s.cfg.Local.Include, Ignore: s.cfg.Local.Ignore,
			PositionResync: s.cfg.Local.PositionResync.D(), CallTimeout: s.cfg.Local.CallTimeout.D(),
			Connect: s.opts.SessionBus,
		}
		want[mpris.Kind] = func() { s.engine.AddProvider(mpris.New(cfg, s.log)) }
	}
	if s.remoteEnabled {
		for _, srv := range s.cfg.Jellyfin.Servers {
			if !srv.IsEnabled() {
				continue
			}
			p := s.jf[srv.Name]
			want[p.Instance()] = func() { s.engine.AddProvider(p) }
		}
	}
	var stop []string
	for inst := range s.running {
		if _, ok := want[inst]; !ok {
			stop = append(stop, inst)
		}
	}
	var start []func()
	for inst, f := range want {
		if !s.running[inst] {
			start = append(start, f)
			s.running[inst] = true
		}
	}
	for _, inst := range stop {
		delete(s.running, inst)
	}
	s.mu.Unlock()
	for _, inst := range stop {
		s.log.Info("stopping provider", "provider", inst)
		s.engine.RemoveProvider(inst)
	}
	for _, f := range start {
		f()
	}
}

// Reload re-reads the configuration file and restarts providers. If the
// new configuration is invalid, the running configuration is kept.
func (s *Service) Reload() error {
	cfg, err := config.Load(s.opts.Paths.ConfigFile)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if cfg.Credentials != s.cfg.Credentials || cfg.IPC.SocketPath != s.cfg.IPC.SocketPath || cfg.IPC.MaxClients != s.cfg.IPC.MaxClients {
		s.log.Warn("credential store and IPC changes take effect after a restart")
	}
	cfg.Credentials, cfg.IPC = s.cfg.Credentials, s.cfg.IPC
	deviceID := s.state.DeviceID
	settings, local, remote := s.state.Overrides.Apply(cfg.EngineSettings(), cfg.Local.Enabled, cfg.Remote.Enabled)
	s.mu.Unlock()
	if err := settings.Validate(); err != nil {
		return err
	}
	jf, err := s.buildJellyfin(cfg, deviceID)
	if err != nil {
		return err
	}
	// The new configuration is valid: swap.
	s.mu.Lock()
	s.cfg = cfg
	s.localEnabled, s.remoteEnabled = local, remote
	instances := make([]string, 0, len(s.running))
	for inst := range s.running {
		instances = append(instances, inst)
	}
	s.running = map[string]bool{}
	s.jf = jf
	s.mu.Unlock()
	for _, inst := range instances {
		s.engine.RemoveProvider(inst)
	}
	if err := s.engine.SetSettings(settings); err != nil {
		return err
	}
	s.applyProviders()
	s.log.Info("configuration reloaded")
	return nil
}

// UpdateSettings applies and persists runtime overrides.
func (s *Service) UpdateSettings(u config.Overrides) error {
	s.mu.Lock()
	merged := s.state.Overrides.Merge(u)
	settings, local, remote := merged.Apply(s.cfg.EngineSettings(), s.cfg.Local.Enabled, s.cfg.Remote.Enabled)
	s.mu.Unlock()
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := s.engine.SetSettings(settings); err != nil {
		return err
	}
	s.mu.Lock()
	s.state.Overrides = merged
	s.localEnabled, s.remoteEnabled = local, remote
	st := s.state
	s.mu.Unlock()
	if err := config.SaveState(s.opts.Paths.StateFile, st); err != nil {
		s.log.Error("cannot persist settings", "err", err)
		return err
	}
	s.applyProviders()
	return nil
}

func (s *Service) jellyfinProvider(name string) (*jellyfin.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" && len(s.jf) == 1 {
		for _, p := range s.jf {
			return p, nil
		}
	}
	p, ok := s.jf[name]
	if !ok {
		names := make([]string, 0, len(s.jf))
		for n := range s.jf {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, model.Errorf(model.ErrProviderNotFound, "no Jellyfin server named %q is configured", name).WithData("configured", names)
	}
	return p, nil
}

// credentialHealth reports whether the credential backend is usable.
func (s *Service) credentialHealth(ctx context.Context) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := map[string]any{"backend": s.creds.Backend()}
	if err := s.creds.Check(ctx); err != nil {
		e := model.AsError(err)
		out["available"] = false
		out["error"] = map[string]any{"code": e.Code, "message": e.Message}
	} else {
		out["available"] = true
	}
	return out
}

func (s *Service) diagnostics(ctx context.Context) map[string]any {
	s.mu.Lock()
	cfg := s.cfg
	local, remote := s.localEnabled, s.remoteEnabled
	servers := make([]map[string]any, 0, len(cfg.Jellyfin.Servers))
	for _, srv := range cfg.Jellyfin.Servers {
		servers = append(servers, map[string]any{"name": srv.Name, "url": srv.URL, "enabled": srv.IsEnabled()})
	}
	s.mu.Unlock()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return map[string]any{
		"service":        "mediasync",
		"version":        s.opts.Version,
		"api_version":    APIVersion,
		"schema_version": model.SchemaVersion,
		"go_version":     runtime.Version(),
		"pid":            os.Getpid(),
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"paths":          s.opts.Paths,
		"local_enabled":  local,
		"remote_enabled": remote,
		"jellyfin":       servers,
		"credentials":    s.credentialHealth(ctx),
		"engine":         s.engine.Diagnostics(),
		"ipc":            s.ipc.Stats(),
		"providers":      s.engine.ProviderStatuses(),
		"runtime": map[string]any{
			"goroutines":     runtime.NumGoroutine(),
			"heap_alloc_kib": mem.HeapAlloc / 1024,
			"sys_kib":        mem.Sys / 1024,
		},
	}
}
