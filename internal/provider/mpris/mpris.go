// Package mpris implements a provider for local media players that expose
// the MPRIS D-Bus interface (https://specifications.freedesktop.org/mpris-spec/latest/)
// on the user's session bus.
//
// Players are discovered and tracked exclusively through D-Bus: bus name
// ownership (NameOwnerChanged), PropertiesChanged and Seeked signals, and
// the advertised Can* properties. No process inspection or
// application-specific behaviour is used.
package mpris

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
)

const (
	// Kind is the provider kind name.
	Kind = "mpris"

	objectPath  = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	ifaceRoot   = "org.mpris.MediaPlayer2"
	ifacePlayer = "org.mpris.MediaPlayer2.Player"
	ifaceProps  = "org.freedesktop.DBus.Properties"
	namePrefix  = model.MPRISPrefix
)

// Config configures the MPRIS provider.
type Config struct {
	// Include, when non-empty, limits discovery to matching players.
	Include []string
	// Ignore excludes matching players (see model.Player.Matches).
	Ignore []string
	// PositionResync is how often the position of playing players is re-read
	// to correct drift. 0 disables periodic resync (Seeked and state changes
	// still trigger reads).
	PositionResync time.Duration
	// CallTimeout bounds every D-Bus call.
	CallTimeout time.Duration
	// Connect returns a session bus connection. Defaults to the user's
	// session bus; tests inject a private bus.
	Connect func() (*dbus.Conn, error)
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Ignore:         []string{"playerctld"},
		PositionResync: 5 * time.Second,
		CallTimeout:    3 * time.Second,
	}
}

// player is the provider's view of one MPRIS player (one D-Bus connection).
type player struct {
	owner   string   // unique bus name, e.g. ":1.42"
	names   []string // well-known MPRIS names owned by this connection
	busName string   // the representative name (shortest)

	identity     string
	desktopEntry string

	rootLoaded   bool
	playerLoaded bool
	loadErr      error

	props map[string]dbus.Variant // Player interface properties

	volumeWritable bool

	positionUS  int64
	hasPosition bool
	positionAt  time.Time

	lastSeen time.Time
}

// Provider is the MPRIS provider.
type Provider struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	conn    *dbus.Conn
	players map[string]*player // keyed by owner unique name
	sink    provider.Sink
}

// New creates an MPRIS provider.
func New(cfg Config, log *slog.Logger) *Provider {
	def := DefaultConfig()
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = def.CallTimeout
	}
	if cfg.PositionResync < 0 {
		cfg.PositionResync = 0
	}
	if cfg.Connect == nil {
		cfg.Connect = func() (*dbus.Conn, error) { return dbus.ConnectSessionBus() }
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		cfg:     cfg,
		log:     log.With("provider", Kind),
		now:     time.Now,
		players: map[string]*player{},
	}
}

// Factory adapts New to provider.Factory. deps.Config must be a Config.
func Factory(deps provider.Deps) (provider.Provider, error) {
	cfg, ok := deps.Config.(Config)
	if !ok {
		return nil, fmt.Errorf("mpris: unexpected config type %T", deps.Config)
	}
	return New(cfg, deps.Logger), nil
}

func (p *Provider) Instance() string     { return Kind }
func (p *Provider) Kind() string         { return Kind }
func (p *Provider) Origin() model.Origin { return model.OriginLocal }

// SessionID returns the stable session ID for an MPRIS bus name.
func SessionID(busName string) string {
	return "mpris:" + strings.TrimPrefix(busName, namePrefix)
}

func (p *Provider) status(state model.ProviderState, msg string, err error) model.ProviderStatus {
	st := model.ProviderStatus{
		Instance: Kind, Kind: Kind, Origin: model.OriginLocal,
		State: state, Message: msg,
	}
	if err != nil {
		st.LastError = &model.ErrorInfo{Code: model.CodeOf(err), Message: model.AsError(err).Message, At: p.now()}
	}
	if state == model.ProviderRunning {
		t := p.now()
		st.LastSuccess = &t
	}
	p.mu.Lock()
	st.Sessions = 0
	for _, pl := range p.players {
		if p.visible(pl) {
			st.Sessions++
		}
	}
	p.mu.Unlock()
	return st
}

// Run connects to the session bus and tracks players until ctx is done or
// the bus connection is lost.
func (p *Provider) Run(ctx context.Context, sink provider.Sink) error {
	sink.SetStatus(p.status(model.ProviderStarting, "connecting to session bus", nil))
	conn, err := p.cfg.Connect()
	if err != nil {
		e := model.Wrap(model.ErrProviderUnavailable, err, "cannot connect to the D-Bus session bus")
		sink.SetStatus(p.status(model.ProviderError, e.Message, e))
		return e
	}
	defer conn.Close()

	signals := make(chan *dbus.Signal, 512)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)

	matches := [][]dbus.MatchOption{
		{
			dbus.WithMatchSender("org.freedesktop.DBus"),
			dbus.WithMatchInterface("org.freedesktop.DBus"),
			dbus.WithMatchMember("NameOwnerChanged"),
			dbus.WithMatchArg0Namespace("org.mpris.MediaPlayer2"),
		},
		{
			dbus.WithMatchInterface(ifaceProps),
			dbus.WithMatchMember("PropertiesChanged"),
			dbus.WithMatchObjectPath(objectPath),
		},
		{
			dbus.WithMatchInterface(ifacePlayer),
			dbus.WithMatchMember("Seeked"),
			dbus.WithMatchObjectPath(objectPath),
		},
	}
	for _, m := range matches {
		if err := conn.AddMatchSignalContext(ctx, m...); err != nil {
			e := model.Wrap(model.ErrProviderError, err, "cannot subscribe to D-Bus signals")
			sink.SetStatus(p.status(model.ProviderError, e.Message, e))
			return e
		}
	}

	p.mu.Lock()
	p.conn = conn
	p.sink = sink
	p.mu.Unlock()
	defer p.teardown()

	if err := p.scan(ctx); err != nil {
		e := model.Wrap(model.ErrProviderError, err, "cannot list D-Bus names")
		sink.SetStatus(p.status(model.ProviderError, e.Message, e))
		return e
	}
	sink.SetStatus(p.status(model.ProviderRunning, "", nil))

	var tick <-chan time.Time
	if p.cfg.PositionResync > 0 {
		t := time.NewTicker(p.cfg.PositionResync)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			sink.SetStatus(p.status(model.ProviderStopped, "", nil))
			return nil
		case <-conn.Context().Done():
			e := model.Errorf(model.ErrProviderUnavailable, "D-Bus session bus connection lost")
			sink.SetStatus(p.status(model.ProviderError, e.Message, e))
			return e
		case sig, ok := <-signals:
			if !ok {
				e := model.Errorf(model.ErrProviderUnavailable, "D-Bus session bus connection lost")
				sink.SetStatus(p.status(model.ProviderError, e.Message, e))
				return e
			}
			p.handleSignal(ctx, sig)
		case <-tick:
			p.resyncPositions(ctx)
		}
	}
}

// teardown removes all sessions after the connection ends.
func (p *Provider) teardown() {
	p.mu.Lock()
	sink := p.sink
	var ids []string
	for _, pl := range p.players {
		if pl.busName != "" {
			ids = append(ids, SessionID(pl.busName))
		}
	}
	p.players = map[string]*player{}
	p.conn = nil
	p.mu.Unlock()
	for _, id := range ids {
		sink.Remove(id)
	}
}

func (p *Provider) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.cfg.CallTimeout)
}

// scan lists existing MPRIS names on the bus.
func (p *Provider) scan(ctx context.Context) error {
	cctx, cancel := p.callCtx(ctx)
	defer cancel()
	var names []string
	if err := p.conn.BusObject().CallWithContext(cctx, "org.freedesktop.DBus.ListNames", 0).Store(&names); err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasPrefix(n, namePrefix) {
			continue
		}
		cctx, cancel := p.callCtx(ctx)
		var owner string
		err := p.conn.BusObject().CallWithContext(cctx, "org.freedesktop.DBus.GetNameOwner", 0, n).Store(&owner)
		cancel()
		if err != nil {
			// The name vanished between ListNames and GetNameOwner.
			continue
		}
		p.nameAcquired(ctx, n, owner)
	}
	return nil
}

func (p *Provider) handleSignal(ctx context.Context, sig *dbus.Signal) {
	switch sig.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(sig.Body) != 3 {
			return
		}
		name, _ := sig.Body[0].(string)
		oldOwner, _ := sig.Body[1].(string)
		newOwner, _ := sig.Body[2].(string)
		if !strings.HasPrefix(name, namePrefix) {
			return
		}
		if oldOwner != "" {
			p.nameLost(name, oldOwner)
		}
		if newOwner != "" {
			p.nameAcquired(ctx, name, newOwner)
		}
	case ifaceProps + ".PropertiesChanged":
		if sig.Path != objectPath || len(sig.Body) < 2 {
			return
		}
		iface, _ := sig.Body[0].(string)
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		var invalidated []string
		if len(sig.Body) >= 3 {
			invalidated, _ = sig.Body[2].([]string)
		}
		p.propertiesChanged(ctx, sig.Sender, iface, changed, invalidated)
	case ifacePlayer + ".Seeked":
		if sig.Path != objectPath || len(sig.Body) < 1 {
			return
		}
		pos, ok := variantInt64(dbus.MakeVariant(sig.Body[0]))
		if !ok {
			return
		}
		p.mu.Lock()
		pl := p.players[sig.Sender]
		if pl != nil {
			pl.positionUS, pl.hasPosition, pl.positionAt = pos, true, p.now()
		}
		p.mu.Unlock()
		if pl != nil {
			p.publish(sig.Sender)
		}
	}
}

func (p *Provider) nameAcquired(ctx context.Context, name, owner string) {
	p.mu.Lock()
	pl, exists := p.players[owner]
	if !exists {
		pl = &player{owner: owner, props: map[string]dbus.Variant{}}
		p.players[owner] = pl
	}
	oldBus := pl.busName
	if !containsStr(pl.names, name) {
		pl.names = append(pl.names, name)
	}
	pl.busName = representative(pl.names)
	p.mu.Unlock()

	if oldBus != "" && oldBus != pl.busName {
		p.sink.Remove(SessionID(oldBus))
	}
	if !exists {
		p.log.Info("player appeared", "bus_name", name, "owner", owner)
		p.load(ctx, owner)
	}
	p.publish(owner)
}

func (p *Provider) nameLost(name, owner string) {
	p.mu.Lock()
	pl := p.players[owner]
	if pl == nil {
		p.mu.Unlock()
		return
	}
	oldBus := pl.busName
	pl.names = removeStr(pl.names, name)
	if len(pl.names) == 0 {
		delete(p.players, owner)
		p.mu.Unlock()
		p.log.Info("player disappeared", "bus_name", name, "owner", owner)
		p.sink.Remove(SessionID(oldBus))
		return
	}
	pl.busName = representative(pl.names)
	p.mu.Unlock()
	if oldBus != pl.busName {
		p.sink.Remove(SessionID(oldBus))
		p.publish(owner)
	}
}

// representative picks the shortest (then lexicographically first) name so
// a player that owns e.g. "org.mpris.MediaPlayer2.vlc" and
// "org.mpris.MediaPlayer2.vlc.instance42" appears once, under a stable ID.
func representative(names []string) string {
	if len(names) == 0 {
		return ""
	}
	best := names[0]
	for _, n := range names[1:] {
		if len(n) < len(best) || (len(n) == len(best) && n < best) {
			best = n
		}
	}
	return best
}

// load reads all properties of a newly discovered player.
func (p *Provider) load(ctx context.Context, owner string) {
	obj := p.conn.Object(owner, objectPath)

	cctx, cancel := p.callCtx(ctx)
	var root map[string]dbus.Variant
	rootErr := obj.CallWithContext(cctx, ifaceProps+".GetAll", 0, ifaceRoot).Store(&root)
	cancel()

	cctx, cancel = p.callCtx(ctx)
	var props map[string]dbus.Variant
	playerErr := obj.CallWithContext(cctx, ifaceProps+".GetAll", 0, ifacePlayer).Store(&props)
	cancel()

	volumeWritable := false
	if playerErr == nil {
		if _, ok := props["Volume"]; ok {
			volumeWritable = p.volumeWritable(ctx, obj)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	pl := p.players[owner]
	if pl == nil {
		return
	}
	pl.lastSeen = p.now()
	if rootErr == nil {
		pl.rootLoaded = true
		if v, ok := root["Identity"]; ok {
			pl.identity, _ = variantString(v)
		}
		if v, ok := root["DesktopEntry"]; ok {
			pl.desktopEntry, _ = variantString(v)
		}
	} else {
		p.log.Warn("cannot read MPRIS root properties", "bus_name", pl.busName, "err", rootErr)
	}
	if playerErr == nil {
		pl.playerLoaded = true
		pl.props = props
		pl.volumeWritable = volumeWritable
		if v, ok := props["Position"]; ok {
			if us, ok := variantInt64(v); ok {
				pl.positionUS, pl.hasPosition, pl.positionAt = us, true, p.now()
			}
		}
		pl.loadErr = nil
	} else {
		pl.loadErr = playerErr
		p.log.Warn("cannot read MPRIS player properties", "bus_name", pl.busName, "err", playerErr)
	}
}

// volumeWritable checks via introspection whether Volume is writable. The
// spec mandates readwrite, but some players expose it read-only. If
// introspection is unavailable, the spec is trusted.
func (p *Provider) volumeWritable(ctx context.Context, obj dbus.BusObject) bool {
	cctx, cancel := p.callCtx(ctx)
	defer cancel()
	var xml string
	if err := obj.CallWithContext(cctx, "org.freedesktop.DBus.Introspectable.Introspect", 0).Store(&xml); err != nil {
		return true
	}
	var node introspect.Node
	if err := xmlUnmarshal(xml, &node); err != nil {
		return true
	}
	for _, iface := range node.Interfaces {
		if iface.Name != ifacePlayer {
			continue
		}
		for _, prop := range iface.Properties {
			if prop.Name == "Volume" {
				return prop.Access == "readwrite" || prop.Access == "write"
			}
		}
		return false
	}
	return true
}

func (p *Provider) propertiesChanged(ctx context.Context, sender, iface string, changed map[string]dbus.Variant, invalidated []string) {
	p.mu.Lock()
	pl := p.players[sender]
	p.mu.Unlock()
	if pl == nil {
		return
	}
	switch iface {
	case ifaceRoot:
		p.mu.Lock()
		if v, ok := changed["Identity"]; ok {
			pl.identity, _ = variantString(v)
		}
		if v, ok := changed["DesktopEntry"]; ok {
			pl.desktopEntry, _ = variantString(v)
		}
		p.mu.Unlock()
	case ifacePlayer:
		// Read invalidated properties explicitly.
		fetched := map[string]dbus.Variant{}
		for _, name := range invalidated {
			cctx, cancel := p.callCtx(ctx)
			var v dbus.Variant
			err := p.conn.Object(sender, objectPath).CallWithContext(cctx, ifaceProps+".Get", 0, ifacePlayer, name).Store(&v)
			cancel()
			if err == nil {
				fetched[name] = v
			}
		}
		_, statusChanged := changed["PlaybackStatus"]
		_, metaChanged := changed["Metadata"]
		_, rateChanged := changed["Rate"]
		p.mu.Lock()
		if !pl.playerLoaded {
			pl.playerLoaded = true
		}
		for k, v := range changed {
			pl.props[k] = v
		}
		for _, name := range invalidated {
			if v, ok := fetched[name]; ok {
				pl.props[name] = v
			} else {
				delete(pl.props, name)
			}
		}
		pl.lastSeen = p.now()
		p.mu.Unlock()
		// Position is not signalled via PropertiesChanged (per spec), so re-read
		// it whenever the playback context changes.
		if statusChanged || metaChanged || rateChanged {
			p.readPosition(ctx, sender)
		}
	default:
		return
	}
	p.publish(sender)
}

// readPosition fetches the current position of a player.
func (p *Provider) readPosition(ctx context.Context, owner string) bool {
	cctx, cancel := p.callCtx(ctx)
	defer cancel()
	var v dbus.Variant
	err := p.conn.Object(owner, objectPath).CallWithContext(cctx, ifaceProps+".Get", 0, ifacePlayer, "Position").Store(&v)
	p.mu.Lock()
	defer p.mu.Unlock()
	pl := p.players[owner]
	if pl == nil {
		return false
	}
	if err != nil {
		// Position is optional in practice; treat as unknown.
		pl.hasPosition = false
		return false
	}
	us, ok := variantInt64(v)
	if !ok {
		pl.hasPosition = false
		return false
	}
	pl.positionUS, pl.hasPosition, pl.positionAt = us, true, p.now()
	return true
}

// resyncPositions re-reads positions of playing players (bounded: one Get
// per playing player per interval).
func (p *Provider) resyncPositions(ctx context.Context) {
	p.mu.Lock()
	var owners []string
	for o, pl := range p.players {
		if s, _ := variantString(pl.props["PlaybackStatus"]); s == "Playing" {
			owners = append(owners, o)
		}
	}
	p.mu.Unlock()
	for _, o := range owners {
		p.readPosition(ctx, o)
		p.publish(o)
	}
}

func (p *Provider) visible(pl *player) bool {
	if pl.busName == "" {
		return false
	}
	pi := p.playerInfo(pl)
	if len(p.cfg.Include) > 0 {
		found := false
		for _, pat := range p.cfg.Include {
			if pi.Matches(pat) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, pat := range p.cfg.Ignore {
		if pi.Matches(pat) {
			return false
		}
	}
	return true
}

func (p *Provider) playerInfo(pl *player) model.Player {
	name := pl.identity
	if name == "" {
		name = strings.TrimPrefix(pl.busName, namePrefix)
	}
	return model.Player{
		Name:         name,
		Identity:     model.StrPtr(pl.identity),
		DesktopEntry: model.StrPtr(pl.desktopEntry),
		BusName:      model.StrPtr(pl.busName),
	}
}

// publish builds the session for a player and sends it to the sink.
func (p *Provider) publish(owner string) {
	p.mu.Lock()
	pl := p.players[owner]
	if pl == nil || p.sink == nil {
		p.mu.Unlock()
		return
	}
	if !p.visible(pl) {
		id := SessionID(pl.busName)
		p.mu.Unlock()
		p.sink.Remove(id)
		return
	}
	s := p.buildSession(pl)
	sink := p.sink
	p.mu.Unlock()
	sink.Upsert(s)
}

func (p *Provider) buildSession(pl *player) model.Session {
	props := pl.props
	getBool := func(k string) bool {
		v, ok := props[k]
		if !ok {
			return false
		}
		b, _ := variantBool(v)
		return b
	}
	s := model.Session{
		ID:                SessionID(pl.busName),
		Provider:          Kind,
		ProviderInstance:  Kind,
		ProviderSessionID: pl.busName,
		Origin:            model.OriginLocal,
		Player:            p.playerInfo(pl),
		Status:            model.StatusConnected,
		LastSeen:          pl.lastSeen,
	}
	if md, ok := props["Metadata"]; ok {
		if m, ok := md.Value().(map[string]dbus.Variant); ok {
			s.Track = trackFromMetadata(m)
		}
	}
	st := model.StateUnknown
	if v, ok := props["PlaybackStatus"]; ok {
		if str, ok := variantString(v); ok {
			st = mapPlaybackStatus(str)
		}
	}
	s.Playback.State = st
	if pl.hasPosition {
		ms := pl.positionUS / 1000
		if ms < 0 {
			ms = 0
		}
		at := pl.positionAt
		s.Playback.PositionMS = &ms
		s.Playback.PositionAt = &at
	}
	if v, ok := props["Rate"]; ok {
		if r, ok := variantFloat(v); ok && r > 0 {
			s.Playback.Rate = &r
		}
	}
	if v, ok := props["Volume"]; ok {
		if vol, ok := variantFloat(v); ok && vol >= 0 {
			s.Playback.Volume = &vol
		}
	}
	if v, ok := props["Shuffle"]; ok {
		if b, ok := variantBool(v); ok {
			s.Playback.Shuffle = &b
		}
	}
	if v, ok := props["LoopStatus"]; ok {
		if str, ok := variantString(v); ok {
			if l, ok := mapLoopStatus(str); ok {
				s.Playback.Repeat = &l
			}
		}
	}

	if getBool("CanControl") {
		var caps []model.Capability
		canPlay, canPause := getBool("CanPlay"), getBool("CanPause")
		if canPlay {
			caps = append(caps, model.CapPlay)
		}
		if canPause {
			caps = append(caps, model.CapPause)
		}
		if canPlay && canPause {
			caps = append(caps, model.CapPlayPause)
		}
		// Stop has no Can* property; it is valid whenever CanControl is true.
		caps = append(caps, model.CapStop)
		if getBool("CanGoNext") {
			caps = append(caps, model.CapNext)
		}
		if getBool("CanGoPrevious") {
			caps = append(caps, model.CapPrevious)
		}
		if getBool("CanSeek") {
			caps = append(caps, model.CapSeek)
		}
		if _, ok := props["Volume"]; ok && pl.volumeWritable {
			caps = append(caps, model.CapVolume)
		}
		s.Capabilities = model.SortCapabilities(caps)
		s.Controllable = true
	} else {
		s.Capabilities = []model.Capability{}
	}
	if pl.loadErr != nil {
		s.Error = &model.ErrorInfo{Code: model.ErrProviderError, Message: "player did not answer property queries", At: pl.lastSeen}
	}
	return s
}

// Command executes a playback command on the player behind sessionID.
func (p *Provider) Command(ctx context.Context, sessionID string, cmd model.Command) error {
	p.mu.Lock()
	conn := p.conn
	var pl *player
	for _, cand := range p.players {
		if cand.busName != "" && SessionID(cand.busName) == sessionID {
			pl = cand
			break
		}
	}
	if pl == nil || conn == nil {
		p.mu.Unlock()
		return model.Errorf(model.ErrSessionNotFound, "MPRIS session %q is not present", sessionID)
	}
	// Address the exact connection (unique name) the session was built
	// from, so a command can never reach a different process that took the
	// name over in the meantime.
	owner := pl.owner
	sess := p.buildSession(pl)
	p.mu.Unlock()

	if c, ok := cmd.RequiredCapability(); !ok || !sess.Has(c) {
		return model.Errorf(model.ErrUnsupported, "player does not support %q", cmd.Name)
	}
	obj := conn.Object(owner, objectPath)
	cctx, cancel := p.callCtx(ctx)
	defer cancel()

	var call *dbus.Call
	switch cmd.Name {
	case model.CmdPlay:
		call = obj.CallWithContext(cctx, ifacePlayer+".Play", 0)
	case model.CmdPause:
		call = obj.CallWithContext(cctx, ifacePlayer+".Pause", 0)
	case model.CmdPlayPause:
		call = obj.CallWithContext(cctx, ifacePlayer+".PlayPause", 0)
	case model.CmdStop:
		call = obj.CallWithContext(cctx, ifacePlayer+".Stop", 0)
	case model.CmdNext:
		call = obj.CallWithContext(cctx, ifacePlayer+".Next", 0)
	case model.CmdPrevious:
		call = obj.CallWithContext(cctx, ifacePlayer+".Previous", 0)
	case model.CmdSeek:
		call = p.seekCall(cctx, obj, sess, cmd)
		if call == nil {
			return model.Errorf(model.ErrUnsupported, "cannot seek: current position is unknown")
		}
	case model.CmdSetVolume:
		call = obj.CallWithContext(cctx, ifaceProps+".Set", 0, ifacePlayer, "Volume", dbus.MakeVariant(*cmd.Volume))
	default:
		return model.Errorf(model.ErrUnsupported, "unknown command %q", cmd.Name)
	}
	if call.Err != nil {
		return mapCallError(call.Err, cmd.Name)
	}
	if cmd.Name == model.CmdSeek {
		// Some players do not emit Seeked; re-read so state reflects the result.
		p.readPosition(ctx, owner)
		p.publish(owner)
	}
	return nil
}

func (p *Provider) seekCall(ctx context.Context, obj dbus.BusObject, sess model.Session, cmd model.Command) *dbus.Call {
	if cmd.OffsetMS != nil {
		return obj.CallWithContext(ctx, ifacePlayer+".Seek", 0, *cmd.OffsetMS*1000)
	}
	target := *cmd.PositionMS
	if sess.Track.DurationMS != nil && target > *sess.Track.DurationMS {
		target = *sess.Track.DurationMS
	}
	// SetPosition is absolute but needs a valid track id.
	if sess.Track.TrackID != nil && strings.HasPrefix(*sess.Track.TrackID, "/") && dbus.ObjectPath(*sess.Track.TrackID).IsValid() {
		return obj.CallWithContext(ctx, ifacePlayer+".SetPosition", 0, dbus.ObjectPath(*sess.Track.TrackID), target*1000)
	}
	cur := sess.Playback.CurrentPosition(p.now())
	if cur == nil {
		return nil
	}
	return obj.CallWithContext(ctx, ifacePlayer+".Seek", 0, (target-*cur)*1000)
}

func mapCallError(err error, name model.CommandName) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return model.Wrap(model.ErrTimeout, err, fmt.Sprintf("player did not answer %q in time", name))
	}
	var derr dbus.Error
	if errors.As(err, &derr) {
		switch derr.Name {
		case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
			return model.Wrap(model.ErrSessionNotFound, err, "player is no longer on the bus")
		case "org.freedesktop.DBus.Error.UnknownMethod", "org.freedesktop.DBus.Error.NotSupported",
			"org.freedesktop.DBus.Error.PropertyReadOnly", "org.freedesktop.DBus.Error.UnknownProperty":
			return model.Wrap(model.ErrUnsupported, err, fmt.Sprintf("player rejected %q as unsupported", name))
		case "org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Timeout":
			return model.Wrap(model.ErrTimeout, err, fmt.Sprintf("player did not answer %q in time", name))
		}
		return model.Wrap(model.ErrProviderError, err, fmt.Sprintf("player returned error %s", derr.Name))
	}
	return model.Wrap(model.ErrProviderError, err, fmt.Sprintf("D-Bus call for %q failed", name))
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func removeStr(xs []string, s string) []string {
	out := xs[:0]
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
