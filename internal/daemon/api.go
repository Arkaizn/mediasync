package daemon

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/engine"
	"github.com/arkaizn/mediasync/internal/ipc"
	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

func (s *Service) registerAPI() {
	h := s.ipc.Handle
	h("hello", s.apiHello)
	h("sessions.list", s.apiSessionsList)
	h("sessions.get", s.apiSessionsGet)
	h("sessions.selected", s.apiSessionsSelected)
	h("sessions.select", s.apiSessionsSelect)
	h("player.command", s.apiPlayerCommand)
	h("subscribe", s.apiSubscribe)
	h("unsubscribe", s.apiUnsubscribe)
	h("providers.status", s.apiProvidersStatus)
	h("settings.get", s.apiSettingsGet)
	h("settings.update", s.apiSettingsUpdate)
	h("diagnostics.get", s.apiDiagnostics)
	h("jellyfin.servers", s.apiJellyfinServers)
	h("jellyfin.login", s.apiJellyfinLogin)
	h("jellyfin.logout", s.apiJellyfinLogout)
	h("jellyfin.check", s.apiJellyfinCheck)
	h("service.reload", s.apiReload)
}

type helloParams struct {
	Client     string `json:"client"`
	APIVersion int    `json:"api_version"`
}

func (s *Service) apiHello(_ context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
	var p helloParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.APIVersion > APIVersion {
		return nil, model.Errorf(model.ErrInvalidArgument, "client requires API version %d but this service provides %d", p.APIVersion, APIVersion).
			WithData("api_version", APIVersion)
	}
	if p.Client != "" {
		c.SetName(p.Client)
	}
	methods := s.ipc.Methods()
	sort.Strings(methods)
	return map[string]any{
		"service":        "mediasync",
		"version":        s.opts.Version,
		"api_version":    APIVersion,
		"schema_version": model.SchemaVersion,
		"methods":        methods,
	}, nil
}

func decodeFilter(raw json.RawMessage) (engine.Filter, error) {
	var f engine.Filter
	if err := ipc.DecodeParams(raw, &f); err != nil {
		return f, err
	}
	return f, f.Validate()
}

func (s *Service) apiSessionsList(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	f, err := decodeFilter(raw)
	if err != nil {
		return nil, err
	}
	return s.engine.Sessions(f), nil
}

type sessionIDParams struct {
	SessionID string `json:"session_id"`
}

func (s *Service) apiSessionsGet(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p sessionIDParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.SessionID == "" {
		return nil, ipc.InvalidParams("session_id is required")
	}
	sess, ok := s.engine.Session(p.SessionID)
	if !ok {
		return nil, model.Errorf(model.ErrSessionNotFound, "session %q does not exist", p.SessionID)
	}
	return map[string]any{"session": sess}, nil
}

func (s *Service) apiSessionsSelected(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	sess, reason := s.engine.Selected()
	return map[string]any{"session": sess, "reason": reason}, nil
}

type selectParams struct {
	// SessionID null or "" returns to automatic selection.
	SessionID *string `json:"session_id"`
}

func (s *Service) apiSessionsSelect(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p selectParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	id := ""
	if p.SessionID != nil {
		id = *p.SessionID
	}
	if err := s.engine.Select(id); err != nil {
		return nil, err
	}
	sess, reason := s.engine.Selected()
	return map[string]any{"session": sess, "reason": reason}, nil
}

type commandParams struct {
	SessionID  string            `json:"session_id"`
	Command    model.CommandName `json:"command"`
	PositionMS *int64            `json:"position_ms"`
	OffsetMS   *int64            `json:"offset_ms"`
	Volume     *float64          `json:"volume"`
}

func (s *Service) apiPlayerCommand(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p commandParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Command == "" {
		return nil, ipc.InvalidParams("command is required")
	}
	return s.engine.Command(ctx, p.SessionID, model.Command{Name: p.Command, PositionMS: p.PositionMS, OffsetMS: p.OffsetMS, Volume: p.Volume})
}

// clientSub tracks one connection's subscription and the set of sessions
// that client currently knows, so that sessions entering or leaving the
// client's view are reported as added/removed.
type clientSub struct {
	mu     sync.Mutex
	filter engine.Filter
	known  map[string]bool
	sub    *engine.Subscription
}

func (cs *clientSub) translate(ev engine.Event) (engine.Event, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	switch ev.Type {
	case engine.EventSessionAdded, engine.EventSessionUpdated:
		visible := ev.Session != nil && cs.filter.Matches(ev.Session) && (cs.filter.IncludeHidden || ev.Session.Hidden == "")
		switch {
		case visible && cs.known[ev.SessionID]:
			ev.Type = engine.EventSessionUpdated
		case visible:
			ev.Type = engine.EventSessionAdded
			cs.known[ev.SessionID] = true
		case cs.known[ev.SessionID]:
			delete(cs.known, ev.SessionID)
			ev.Type = engine.EventSessionRemoved
		default:
			return ev, false
		}
	case engine.EventSessionRemoved:
		if !cs.known[ev.SessionID] {
			return ev, false
		}
		delete(cs.known, ev.SessionID)
	}
	return ev, true
}

const subKey = "subscription"

func (s *Service) apiSubscribe(_ context.Context, c *ipc.Conn, raw json.RawMessage) (any, error) {
	f, err := decodeFilter(raw)
	if err != nil {
		return nil, err
	}
	if old, ok := c.Value(subKey).(*clientSub); ok {
		old.sub.Close()
	}
	snap, sub := s.engine.Subscribe(f)
	cs := &clientSub{filter: f, known: map[string]bool{}, sub: sub}
	for _, sess := range snap.Sessions {
		cs.known[sess.ID] = true
	}
	c.SetValue(subKey, cs)
	c.OnClose(sub.Close)
	pump := func() {
		for ev := range sub.Events() {
			if out, ok := cs.translate(ev); ok {
				c.Notify(ipc.NotificationEvent, out)
			}
		}
		// The engine closed the subscription.
		if r := sub.Reason(); r == engine.CloseOverflow || r == engine.CloseShutdown {
			c.Notify(ipc.NotificationEvent, map[string]any{"type": "subscription.closed", "reason": r})
		}
	}
	return ipc.Deferred{
		Result: map[string]any{"snapshot": snap, "api_version": APIVersion, "schema_version": model.SchemaVersion},
		After:  func() { go pump() },
	}, nil
}

func (s *Service) apiUnsubscribe(_ context.Context, c *ipc.Conn, _ json.RawMessage) (any, error) {
	if cs, ok := c.Value(subKey).(*clientSub); ok {
		cs.sub.Close()
		c.SetValue(subKey, nil)
	}
	return map[string]any{"unsubscribed": true}, nil
}

func (s *Service) apiProvidersStatus(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	return map[string]any{"providers": s.engine.ProviderStatuses()}, nil
}

func (s *Service) settingsView() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"settings":       s.engine.Settings(),
		"local_enabled":  s.localEnabled,
		"remote_enabled": s.remoteEnabled,
		"overrides":      s.state.Overrides,
	}
}

func (s *Service) apiSettingsGet(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	return s.settingsView(), nil
}

// settingsUpdateParams lists the settings a client may change at runtime.
type settingsUpdateParams struct {
	config.Overrides
	// Reset clears all runtime overrides (back to the config file values).
	Reset bool `json:"reset"`
}

func (s *Service) apiSettingsUpdate(_ context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p settingsUpdateParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if p.Reset {
		s.mu.Lock()
		s.state.Overrides = config.Overrides{}
		s.mu.Unlock()
	}
	if err := s.UpdateSettings(p.Overrides); err != nil {
		return nil, err
	}
	return s.settingsView(), nil
}

func (s *Service) apiDiagnostics(ctx context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	return s.diagnostics(ctx), nil
}

func (s *Service) apiJellyfinServers(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	s.mu.Lock()
	cfg := s.cfg
	remote := s.remoteEnabled
	running := map[string]bool{}
	for k, v := range s.running {
		running[k] = v
	}
	jf := s.jf
	s.mu.Unlock()
	out := []map[string]any{}
	for _, srv := range cfg.Jellyfin.Servers {
		p := jf[srv.Name]
		out = append(out, map[string]any{
			"name": srv.Name, "url": p.ServerURL(), "enabled": srv.IsEnabled() && remote,
			"instance": p.Instance(), "running": running[p.Instance()], "status": p.Status(),
		})
	}
	return map[string]any{"servers": out}, nil
}

type loginParams struct {
	Server   string `json:"server"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Service) apiJellyfinLogin(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p loginParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	pw := redact.Secret(p.Password)
	p.Password = ""
	prov, err := s.jellyfinProvider(p.Server)
	if err != nil {
		return nil, err
	}
	return prov.Login(ctx, p.Username, pw)
}

type serverParams struct {
	Server string `json:"server"`
}

func (s *Service) apiJellyfinLogout(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p serverParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	prov, err := s.jellyfinProvider(p.Server)
	if err != nil {
		return nil, err
	}
	return prov.Logout(ctx)
}

func (s *Service) apiJellyfinCheck(ctx context.Context, _ *ipc.Conn, raw json.RawMessage) (any, error) {
	var p serverParams
	if err := ipc.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	prov, err := s.jellyfinProvider(p.Server)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return prov.Check(ctx), nil
}

func (s *Service) apiReload(_ context.Context, _ *ipc.Conn, _ json.RawMessage) (any, error) {
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return map[string]any{"reloaded": true}, nil
}
