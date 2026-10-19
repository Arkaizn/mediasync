// Package jellyfin implements a provider for playback sessions reported by a
// Jellyfin server (tested against Jellyfin 12.2).
//
// Verified server behaviour this provider relies on (see docs/JELLYFIN.md):
//   - GET /Sessions lists sessions with NowPlayingItem and PlayState; the
//     server extrapolates PositionTicks between client reports.
//   - SupportsRemoteControl is true only while the client holds a live
//     WebSocket; only then can the server deliver commands. The command
//     endpoints answer 204 even for sessions that cannot receive them, so
//     the provider gates commands on SupportsRemoteControl itself.
//   - Clients without a WebSocket keep their session (and an ever-increasing
//     extrapolated position) for several minutes after they stop reporting;
//     LastPlaybackCheckIn is used to detect such stale sessions.
//   - Images are served without authentication, so artwork URLs carry no
//     credentials.
package jellyfin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
	"github.com/arkaizn/mediasync/internal/redact"
	"github.com/arkaizn/mediasync/internal/secrets"
)

// Kind is the provider kind name.
const Kind = "jellyfin"

const ticksPerMS = 10_000

// Config configures one Jellyfin server.
type Config struct {
	// Name is the instance name (e.g. "home"); the provider instance is "jellyfin:<Name>".
	Name string
	URL  string

	DeviceID   string
	DeviceName string
	Version    string

	PollInterval   time.Duration
	RequestTimeout time.Duration
	// StaleAfter marks a playing session without a WebSocket as stale when
	// its last playback report is older than this.
	StaleAfter time.Duration
	// StalePausedAfter is the same for paused sessions (clients often stop
	// reporting while paused).
	StalePausedAfter time.Duration
	// RemoveDisconnectedAfter removes sessions after the server has been
	// unreachable for this long.
	RemoveDisconnectedAfter time.Duration
	// MaxBackoff bounds the retry interval while the server is unreachable.
	MaxBackoff time.Duration

	// AllUsers includes sessions of other users (admin accounts only).
	AllUsers      bool
	IgnoreClients []string
	IgnoreDevices []string

	CAFile     string
	HTTPClient *http.Client

	Credentials secrets.Store
}

func (c *Config) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Second
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 90 * time.Second
	}
	if c.StalePausedAfter <= 0 {
		c.StalePausedAfter = 15 * time.Minute
	}
	if c.RemoveDisconnectedAfter <= 0 {
		c.RemoveDisconnectedAfter = 5 * time.Minute
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Minute
	}
}

// Provider is the Jellyfin provider.
type Provider struct {
	cfg    Config
	log    *slog.Logger
	client *Client
	now    func() time.Time

	refresh chan struct{}
	reauth  chan struct{}

	mu         sync.Mutex
	sink       provider.Sink
	server     *PublicSystemInfo
	user       *User
	live       map[string]SessionInfo   // our session ID -> last raw info
	published  map[string]model.Session // our session ID -> last published
	lastOK     time.Time
	downSince  time.Time
	lastErr    error
	state      model.ProviderState
	message    string
	serverSkew time.Duration
	lastPoll   time.Time
	pollCount  uint64
}

// New creates a provider.
func New(cfg Config, log *slog.Logger) (*Provider, error) {
	cfg.defaults()
	if cfg.Name == "" {
		return nil, model.Errorf(model.ErrInvalidConfig, "jellyfin server needs a name")
	}
	if cfg.Credentials == nil {
		return nil, model.Errorf(model.ErrInvalidConfig, "jellyfin provider needs a credential store")
	}
	if cfg.DeviceID == "" {
		return nil, model.Errorf(model.ErrInvalidConfig, "jellyfin provider needs a device id")
	}
	client, err := NewClient(ClientOptions{
		BaseURL: cfg.URL, DeviceID: cfg.DeviceID, DeviceName: cfg.DeviceName, Version: cfg.Version,
		Timeout: cfg.RequestTimeout, CAFile: cfg.CAFile, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		cfg:       cfg,
		log:       log.With("provider", Kind, "server", cfg.Name),
		client:    client,
		now:       time.Now,
		refresh:   make(chan struct{}, 1),
		reauth:    make(chan struct{}, 1),
		live:      map[string]SessionInfo{},
		published: map[string]model.Session{},
		state:     model.ProviderStarting,
	}, nil
}

// Factory adapts New to provider.Factory. deps.Config must be a Config.
func Factory(deps provider.Deps) (provider.Provider, error) {
	cfg, ok := deps.Config.(Config)
	if !ok {
		return nil, fmt.Errorf("jellyfin: unexpected config type %T", deps.Config)
	}
	return New(cfg, deps.Logger)
}

func (p *Provider) Instance() string     { return Kind + ":" + p.cfg.Name }
func (p *Provider) Kind() string         { return Kind }
func (p *Provider) Origin() model.Origin { return model.OriginRemote }

// ServerURL returns the configured server URL.
func (p *Provider) ServerURL() string { return p.client.BaseURL() }

func (p *Provider) credAttrs(user string) secrets.Attributes {
	return secrets.Attributes{Kind: "jellyfin-token", Instance: p.cfg.Name, Server: p.client.BaseURL(), User: user}
}

// Refresh requests an immediate poll.
func (p *Provider) Refresh() {
	select {
	case p.refresh <- struct{}{}:
	default:
	}
}

func (p *Provider) signalReauth() {
	select {
	case p.reauth <- struct{}{}:
	default:
	}
}

func (p *Provider) setStatus(state model.ProviderState, msg string, err error) {
	p.mu.Lock()
	p.state, p.message = state, msg
	if err != nil {
		p.lastErr = err
	} else if state == model.ProviderRunning {
		p.lastErr = nil
	}
	st := p.statusLocked()
	sink := p.sink
	p.mu.Unlock()
	if sink != nil {
		sink.SetStatus(st)
	}
}

func (p *Provider) statusLocked() model.ProviderStatus {
	st := model.ProviderStatus{
		Instance: p.Instance(), Kind: Kind, Origin: model.OriginRemote,
		State: p.state, Message: p.message, Sessions: len(p.published),
		Details: map[string]any{"server_url": p.client.BaseURL()},
	}
	if p.lastErr != nil {
		e := model.AsError(p.lastErr)
		st.LastError = &model.ErrorInfo{Code: e.Code, Message: e.Message, At: p.now()}
	}
	if !p.lastOK.IsZero() {
		t := p.lastOK
		st.LastSuccess = &t
	}
	if p.server != nil {
		st.Details["server_id"] = p.server.ID
		st.Details["server_name"] = p.server.ServerName
		st.Details["server_version"] = p.server.Version
	}
	if p.user != nil {
		st.Details["user_id"] = p.user.ID
		st.Details["user_name"] = p.user.Name
	}
	if !p.lastPoll.IsZero() {
		st.Details["last_poll"] = p.lastPoll
	}
	st.Details["polls"] = p.pollCount
	return st
}

// Status returns the current status.
func (p *Provider) Status() model.ProviderStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked()
}

// Run drives the provider until ctx is cancelled.
func (p *Provider) Run(ctx context.Context, sink provider.Sink) error {
	p.mu.Lock()
	p.sink = sink
	p.mu.Unlock()
	defer func() {
		p.removeAll()
		p.mu.Lock()
		p.sink = nil
		p.mu.Unlock()
	}()
	backoff := p.cfg.PollInterval
	for {
		if ctx.Err() != nil {
			return nil
		}
		// 1. Credentials.
		if !p.client.HasToken() {
			p.setStatus(model.ProviderStarting, "loading credentials", nil)
			tok, err := p.cfg.Credentials.Get(ctx, p.credAttrs(""))
			switch {
			case errors.Is(err, secrets.ErrNotFound):
				p.setStatus(model.ProviderUnauthenticated, "not logged in; run `mediasyncctl jellyfin login "+p.cfg.Name+"`",
					model.Errorf(model.ErrNotAuthenticated, "no stored credentials for this server"))
				if !p.waitReauth(ctx, 0) {
					return nil
				}
				continue
			case err != nil:
				p.setStatus(model.ProviderError, "credential store unavailable", err)
				if !p.waitReauth(ctx, 30*time.Second) {
					return nil
				}
				continue
			}
			p.client.SetToken(tok)
		}

		// 2. Validate server and token.
		if err := p.validate(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if model.CodeOf(err) == model.ErrAuthFailed {
				p.client.SetToken("")
				p.removeAll()
				p.setStatus(model.ProviderUnauthenticated, "the stored token was rejected; log in again", err)
				// Re-check occasionally in case the server was misconfigured.
				if !p.waitReauth(ctx, 10*time.Minute) {
					return nil
				}
				continue
			}
			p.markDisconnected(err)
			if !p.sleep(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, p.cfg.MaxBackoff)
			continue
		}
		backoff = p.cfg.PollInterval

		// 3. Poll.
		err := p.pollLoop(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errReauth) {
			continue
		}
		if model.CodeOf(err) == model.ErrAuthFailed {
			continue // validate again → unauthenticated
		}
		p.markDisconnected(err)
		if !p.sleep(ctx, backoff) {
			return nil
		}
		backoff = min(backoff*2, p.cfg.MaxBackoff)
	}
}

var errReauth = errors.New("credentials changed")

func (p *Provider) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-p.reauth:
		return true
	case <-p.refresh:
		return true
	}
}

// waitReauth waits for a login (or the optional timeout). Returns false on cancellation.
func (p *Provider) waitReauth(ctx context.Context, timeout time.Duration) bool {
	var tc <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		tc = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-p.reauth:
		return true
	case <-tc:
		return true
	}
}

func (p *Provider) validate(ctx context.Context) error {
	info, err := p.client.PublicInfo(ctx)
	if err != nil {
		return err
	}
	user, err := p.client.Me(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.server != nil && p.server.ID != info.ID {
		p.log.Warn("server identity changed", "old", p.server.ID, "new", info.ID)
	}
	p.server = &info
	p.user = &user
	p.mu.Unlock()
	p.log.Info("connected", "server_name", info.ServerName, "version", info.Version, "user", user.Name)
	return nil
}

func (p *Provider) pollLoop(ctx context.Context) error {
	const minGap = 150 * time.Millisecond
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.reauth:
			return errReauth
		case <-timer.C:
		case <-p.refresh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if wait := minGap - time.Since(last); wait > 0 {
				time.Sleep(wait)
			}
		}
		last = time.Now()
		if err := p.poll(ctx); err != nil {
			return err
		}
		timer.Reset(p.cfg.PollInterval)
	}
}

// poll fetches sessions once and publishes changes.
func (p *Provider) poll(ctx context.Context) error {
	sessions, serverNow, err := p.fetchSessions(ctx)
	if err != nil {
		return err
	}
	now := p.now()
	p.mu.Lock()
	p.lastOK, p.lastPoll, p.downSince = now, now, time.Time{}
	p.pollCount++
	if !serverNow.IsZero() {
		p.serverSkew = serverNow.Sub(now)
	}
	user, server := p.user, p.server
	sink := p.sink
	seen := map[string]bool{}
	var upserts []model.Session
	for _, si := range sessions {
		if !p.include(si, user) {
			continue
		}
		s := p.mapSession(si, server, now)
		seen[s.ID] = true
		p.live[s.ID] = si
		p.published[s.ID] = s
		upserts = append(upserts, s)
	}
	var removals []string
	for id := range p.published {
		if !seen[id] {
			removals = append(removals, id)
			delete(p.published, id)
			delete(p.live, id)
		}
	}
	p.state, p.message, p.lastErr = model.ProviderRunning, "", nil
	st := p.statusLocked()
	p.mu.Unlock()
	if sink != nil {
		for _, s := range upserts {
			sink.Upsert(s)
		}
		for _, id := range removals {
			sink.Remove(id)
		}
		sink.SetStatus(st)
	}
	return nil
}

func (p *Provider) fetchSessions(ctx context.Context) ([]SessionInfo, time.Time, error) {
	// Also read the Date header to correct for clock skew between this
	// machine and the server when judging staleness.
	var out []SessionInfo
	hdr, err := p.client.doHeader(ctx, http.MethodGet, "/Sessions", nil, nil, &out, model.ErrProviderError)
	var serverNow time.Time
	if hdr != nil {
		if d, perr := http.ParseTime(hdr.Get("Date")); perr == nil {
			serverNow = d
		}
	}
	return out, serverNow, err
}

// include filters out our own session, idle sessions, other users and
// ignored clients/devices.
func (p *Provider) include(si SessionInfo, user *User) bool {
	if si.NowPlayingItem == nil {
		return false // idle client: nothing is playing
	}
	if si.DeviceID == p.cfg.DeviceID {
		return false
	}
	if !p.cfg.AllUsers && user != nil && si.UserID != "" && !strings.EqualFold(si.UserID, user.ID) {
		return false
	}
	for _, c := range p.cfg.IgnoreClients {
		if strings.EqualFold(c, si.Client) {
			return false
		}
	}
	for _, d := range p.cfg.IgnoreDevices {
		if strings.EqualFold(d, si.DeviceName) || strings.EqualFold(d, si.DeviceID) {
			return false
		}
	}
	return true
}

// SessionID derives a stable session ID. Jellyfin's own session id is not
// used because it changes when a client reconnects with another version.
func (p *Provider) SessionID(serverID, deviceID, client string) string {
	h := sha256.Sum256([]byte(serverID + "\x00" + deviceID + "\x00" + client))
	return "jellyfin:" + p.cfg.Name + ":" + hex.EncodeToString(h[:8])
}

func (p *Provider) mapSession(si SessionInfo, server *PublicSystemInfo, now time.Time) model.Session {
	serverID := si.ServerID
	if serverID == "" && server != nil {
		serverID = server.ID
	}
	item := si.NowPlayingItem
	s := model.Session{
		ID:                p.SessionID(serverID, si.DeviceID, si.Client),
		Provider:          Kind,
		ProviderInstance:  p.Instance(),
		ProviderSessionID: si.ID,
		Origin:            model.OriginRemote,
		Status:            model.StatusConnected,
		LastSeen:          now,
	}
	name := si.DeviceName
	if name == "" {
		name = si.Client
	}
	if si.DeviceName != "" && si.Client != "" {
		name = fmt.Sprintf("%s (%s)", si.DeviceName, si.Client)
	}
	if name == "" {
		name = "Jellyfin session"
	}
	s.Player = model.Player{
		Name:          name,
		Identity:      model.StrPtr(si.Client),
		Client:        model.StrPtr(si.Client),
		ClientVersion: model.StrPtr(si.ApplicationVersion),
		Device:        model.StrPtr(si.DeviceName),
		DeviceID:      model.StrPtr(si.DeviceID),
		Address:       model.StrPtr(si.RemoteEndPoint),
	}
	s.Server = &model.Server{
		Instance: p.cfg.Name,
		ID:       model.StrPtr(serverID),
		UserID:   model.StrPtr(si.UserID),
		UserName: model.StrPtr(si.UserName),
	}
	if server != nil {
		s.Server.Name = model.StrPtr(server.ServerName)
		s.Server.Version = model.StrPtr(server.Version)
	}

	// Track.
	s.Track = model.Track{
		Title:     model.StrPtr(strings.TrimSpace(item.Name)),
		Album:     model.StrPtr(strings.TrimSpace(item.Album)),
		TrackID:   model.StrPtr(item.ID),
		MediaType: model.StrPtr(firstNonEmpty(item.MediaType, item.Type)),
	}
	if len(item.Artists) > 0 {
		s.Track.Artists = slices.Clone(item.Artists)
	}
	for _, a := range item.AlbumArtists {
		if a.Name != "" {
			s.Track.AlbumArtists = append(s.Track.AlbumArtists, a.Name)
		}
	}
	if len(s.Track.AlbumArtists) == 0 && item.AlbumArtist != "" {
		s.Track.AlbumArtists = []string{item.AlbumArtist}
	}
	if item.RunTimeTicks != nil && *item.RunTimeTicks > 0 {
		s.Track.DurationMS = model.Ptr(*item.RunTimeTicks / ticksPerMS)
	}
	switch {
	case item.ImageTags["Primary"] != "":
		s.Track.ArtworkURL = model.Ptr(p.client.ImageURL(item.ID, item.ImageTags["Primary"]))
	case item.AlbumID != "" && item.AlbumPrimaryImageTag != "":
		s.Track.ArtworkURL = model.Ptr(p.client.ImageURL(item.AlbumID, item.AlbumPrimaryImageTag))
	}

	// Playback.
	ps := si.PlayState
	s.Playback.State = model.StatePlaying
	if ps.IsPaused {
		s.Playback.State = model.StatePaused
	}
	if ps.PositionTicks != nil {
		at := now
		s.Playback.PositionMS = model.Ptr(max(*ps.PositionTicks/ticksPerMS, 0))
		s.Playback.PositionAt = &at
	}
	if ps.VolumeLevel != nil {
		s.Playback.Volume = model.Ptr(float64(*ps.VolumeLevel) / 100)
	}
	s.Playback.Muted = model.Ptr(ps.IsMuted)
	switch ps.RepeatMode {
	case "RepeatNone":
		s.Playback.Repeat = model.Ptr("none")
	case "RepeatAll":
		s.Playback.Repeat = model.Ptr("playlist")
	case "RepeatOne":
		s.Playback.Repeat = model.Ptr("track")
	}
	switch ps.PlaybackOrder {
	case "Shuffle":
		s.Playback.Shuffle = model.Ptr(true)
	case "Default":
		s.Playback.Shuffle = model.Ptr(false)
	}

	// Staleness: a session with a WebSocket is live by definition (the
	// server drops it when the socket closes); otherwise rely on the last
	// playback report.
	if !si.SupportsRemoteControl {
		checkin := si.LastPlaybackCheckIn
		if checkin.Year() < 2000 {
			checkin = si.LastActivityDate
		}
		serverNow := now.Add(p.serverSkew)
		limit := p.cfg.StaleAfter
		if ps.IsPaused {
			limit = p.cfg.StalePausedAfter
		}
		if !checkin.IsZero() && checkin.Year() >= 2000 && serverNow.Sub(checkin) > limit {
			s.Status = model.StatusStale
			// The server keeps extrapolating the position of a client that
			// stopped reporting; do not present that as fact.
			s.Playback.PositionMS, s.Playback.PositionAt = nil, nil
		}
	}

	// Capabilities: only sessions with a live remote-control channel.
	if si.SupportsRemoteControl {
		s.Controllable = true
		caps := []model.Capability{model.CapPlay, model.CapPause, model.CapPlayPause, model.CapStop, model.CapNext, model.CapPrevious}
		if ps.CanSeek && s.Playback.PositionMS != nil {
			caps = append(caps, model.CapSeek)
		}
		if slices.Contains(si.SupportedCommands, "SetVolume") {
			caps = append(caps, model.CapVolume)
		}
		s.Capabilities = model.SortCapabilities(caps)
	} else {
		s.Capabilities = []model.Capability{}
	}
	return s
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// markDisconnected flags all sessions as disconnected after a server failure
// and removes them once the outage exceeds RemoveDisconnectedAfter.
func (p *Provider) markDisconnected(err error) {
	now := p.now()
	p.mu.Lock()
	if p.downSince.IsZero() {
		p.downSince = now
		p.log.Warn("server unreachable", "err", redact.String(err.Error()))
	}
	remove := now.Sub(p.downSince) > p.cfg.RemoveDisconnectedAfter
	var upserts []model.Session
	var removals []string
	for id, s := range p.published {
		if remove {
			removals = append(removals, id)
			delete(p.published, id)
			delete(p.live, id)
			continue
		}
		if s.Status != model.StatusDisconnected {
			s = s.Clone()
			s.Status = model.StatusDisconnected
			s.Controllable = false
			s.Capabilities = []model.Capability{}
			s.Error = &model.ErrorInfo{Code: model.CodeOf(err), Message: model.AsError(err).Message, At: now}
			p.published[id] = s
			upserts = append(upserts, s)
		}
	}
	sink := p.sink
	p.mu.Unlock()
	if sink != nil {
		for _, s := range upserts {
			sink.Upsert(s)
		}
		for _, id := range removals {
			sink.Remove(id)
		}
	}
	p.setStatus(model.ProviderDegraded, "server unreachable; retrying", err)
}

func (p *Provider) removeAll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.published))
	for id := range p.published {
		ids = append(ids, id)
	}
	p.published = map[string]model.Session{}
	p.live = map[string]SessionInfo{}
	sink := p.sink
	p.mu.Unlock()
	if sink != nil {
		for _, id := range ids {
			sink.Remove(id)
		}
	}
}

// Command sends a playback command to the Jellyfin client behind sessionID.
func (p *Provider) Command(ctx context.Context, sessionID string, cmd model.Command) error {
	p.mu.Lock()
	si, ok := p.live[sessionID]
	pub := p.published[sessionID]
	p.mu.Unlock()
	if !ok {
		return model.Errorf(model.ErrSessionNotFound, "Jellyfin session %q is not present", sessionID)
	}
	if pub.Status == model.StatusDisconnected {
		return model.Errorf(model.ErrSessionUnavailable, "the Jellyfin server is unreachable")
	}
	if !si.SupportsRemoteControl {
		return model.Errorf(model.ErrNotControllable, "the Jellyfin client %q does not accept remote control (no active control connection)", si.Client)
	}
	if c, ok := cmd.RequiredCapability(); !ok || !pub.Has(c) {
		return model.Errorf(model.ErrUnsupported, "the Jellyfin client does not support %q", cmd.Name)
	}
	jfID := si.ID
	var err error
	switch cmd.Name {
	case model.CmdPlay:
		err = p.client.PlaystateCommand(ctx, jfID, "Unpause", nil)
	case model.CmdPause:
		err = p.client.PlaystateCommand(ctx, jfID, "Pause", nil)
	case model.CmdPlayPause:
		err = p.client.PlaystateCommand(ctx, jfID, "PlayPause", nil)
	case model.CmdStop:
		err = p.client.PlaystateCommand(ctx, jfID, "Stop", nil)
	case model.CmdNext:
		err = p.client.PlaystateCommand(ctx, jfID, "NextTrack", nil)
	case model.CmdPrevious:
		err = p.client.PlaystateCommand(ctx, jfID, "PreviousTrack", nil)
	case model.CmdSeek:
		var target int64
		if cmd.PositionMS != nil {
			target = *cmd.PositionMS
		} else {
			cur := pub.Playback.CurrentPosition(p.now())
			if cur == nil {
				return model.Errorf(model.ErrUnsupported, "cannot seek relatively: position unknown")
			}
			target = max(*cur+*cmd.OffsetMS, 0)
		}
		if pub.Track.DurationMS != nil && target > *pub.Track.DurationMS {
			target = *pub.Track.DurationMS
		}
		ticks := target * ticksPerMS
		err = p.client.PlaystateCommand(ctx, jfID, "Seek", &ticks)
	case model.CmdSetVolume:
		v := int(math.Round(*cmd.Volume * 100))
		v = min(max(v, 0), 100)
		err = p.client.GeneralCommand(ctx, jfID, "SetVolume", map[string]string{"Volume": strconv.Itoa(v)})
	default:
		return model.Errorf(model.ErrUnsupported, "unknown command %q", cmd.Name)
	}
	if err != nil {
		return err
	}
	p.Refresh()
	return nil
}

// LoginResult describes a successful login (no secrets).
type LoginResult struct {
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	Version    string `json:"server_version"`
	UserID     string `json:"user_id"`
	UserName   string `json:"user_name"`
	Backend    string `json:"credential_backend"`
}

// Login authenticates with user name and password and stores the token.
// The password is not stored.
func (p *Provider) Login(ctx context.Context, username string, password redact.Secret) (LoginResult, error) {
	if strings.TrimSpace(username) == "" {
		return LoginResult{}, model.Errorf(model.ErrInvalidArgument, "username is required")
	}
	info, err := p.client.PublicInfo(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	user, token, err := p.client.AuthenticateByName(ctx, username, password)
	if err != nil {
		if model.CodeOf(err) == model.ErrAuthFailed {
			return LoginResult{}, model.Errorf(model.ErrAuthFailed, "Jellyfin rejected the user name or password")
		}
		return LoginResult{}, err
	}
	if err := p.cfg.Credentials.Set(ctx, p.credAttrs(user.Name), "mediasync: Jellyfin token for "+p.cfg.Name+" ("+p.client.BaseURL()+")", token); err != nil {
		// Do not leave an orphaned token on the server.
		tmp, cerr := NewClient(ClientOptions{BaseURL: p.cfg.URL, DeviceID: p.cfg.DeviceID, DeviceName: p.cfg.DeviceName, Version: p.cfg.Version, Timeout: p.cfg.RequestTimeout, CAFile: p.cfg.CAFile, HTTPClient: p.cfg.HTTPClient})
		if cerr == nil {
			tmp.SetToken(token)
			_ = tmp.Logout(ctx)
		}
		return LoginResult{}, err
	}
	p.client.SetToken(token)
	p.signalReauth()
	p.log.Info("logged in", "user", user.Name)
	return LoginResult{ServerID: info.ID, ServerName: info.ServerName, Version: info.Version, UserID: user.ID, UserName: user.Name, Backend: p.cfg.Credentials.Backend()}, nil
}

// LogoutResult describes the outcome of a logout.
type LogoutResult struct {
	// Revoked is true if the server confirmed the token was revoked.
	Revoked bool `json:"revoked"`
	// CredentialDeleted is true if the stored token was deleted.
	CredentialDeleted bool   `json:"credential_deleted"`
	Detail            string `json:"detail,omitempty"`
}

// Logout revokes the token on the server (best effort), deletes the stored
// credential and stops polling.
func (p *Provider) Logout(ctx context.Context) (LogoutResult, error) {
	var res LogoutResult
	if !p.client.HasToken() {
		if tok, err := p.cfg.Credentials.Get(ctx, p.credAttrs("")); err == nil {
			p.client.SetToken(tok)
		}
	}
	if p.client.HasToken() {
		if err := p.client.Logout(ctx); err != nil {
			res.Detail = "the server could not be told to revoke the token: " + model.AsError(err).Message
		} else {
			res.Revoked = true
		}
	}
	p.client.SetToken("")
	if err := p.cfg.Credentials.Delete(ctx, p.credAttrs("")); err != nil {
		return res, err
	}
	res.CredentialDeleted = true
	p.mu.Lock()
	p.user = nil
	p.mu.Unlock()
	p.removeAll()
	p.signalReauth()
	return res, nil
}
