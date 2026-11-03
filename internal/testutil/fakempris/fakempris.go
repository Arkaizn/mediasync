// Package fakempris implements a configurable fake MPRIS player on a real
// D-Bus connection, for tests and manual experiments.
package fakempris

import (
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

const (
	path        = dbus.ObjectPath("/org/mpris/MediaPlayer2")
	ifaceRoot   = "org.mpris.MediaPlayer2"
	ifacePlayer = "org.mpris.MediaPlayer2.Player"
)

// Track is a fake track.
type Track struct {
	ID       string // object path suffix, e.g. "t1"
	Title    string
	Artists  []string
	Album    string
	ArtURL   string
	URL      string
	LengthUS int64
	// Lyrics is published as xesam:asText (Feishin publishes LRC there).
	Lyrics string
}

// Options configure a fake player.
type Options struct {
	// Name is the bus name suffix ("feishin" → org.mpris.MediaPlayer2.feishin).
	Name         string
	Identity     string
	DesktopEntry string
	Tracks       []Track
	Status       string // "Playing", "Paused", "Stopped"
	PositionUS   int64

	CanControl, CanPlay, CanPause, CanGoNext, CanGoPrevious, CanSeek bool

	// Volume, if non-nil, exposes the Volume property.
	Volume         *float64
	VolumeReadOnly bool
	// OmitMetadata leaves out the Metadata property entirely.
	OmitMetadata bool
	// RawMetadata, if set, replaces generated metadata (to test malformed data).
	RawMetadata map[string]dbus.Variant
	// NoSeekedSignal suppresses Seeked signals.
	NoSeekedSignal bool
	// MethodDelay delays every method reply.
	MethodDelay time.Duration
	// IgnoreCommands accepts commands without changing state.
	IgnoreCommands bool
}

// FullCaps returns options with every capability enabled.
func FullCaps(name string) Options {
	v := 0.5
	return Options{
		Name: name, Identity: name, Status: "Paused",
		CanControl: true, CanPlay: true, CanPause: true, CanGoNext: true, CanGoPrevious: true, CanSeek: true,
		Volume: &v,
		Tracks: []Track{
			{ID: "t1", Title: "First Song", Artists: []string{"Artist One"}, Album: "Album", LengthUS: 200_000_000},
			{ID: "t2", Title: "Second Song", Artists: []string{"Artist Two"}, Album: "Album", LengthUS: 180_000_000},
			{ID: "t3", Title: "Third Song", Artists: []string{"Artist Three"}, Album: "Album", LengthUS: 240_000_000},
		},
	}
}

// Player is a running fake player.
type Player struct {
	Conn  *dbus.Conn
	opts  Options
	props *propStore

	mu       sync.Mutex
	calls    []string
	trackIdx int
	posUS    int64
	posAt    time.Time
	status   string
}

// BusName returns the full well-known name.
func (p *Player) BusName() string { return "org.mpris.MediaPlayer2." + p.opts.Name }

// Start exports a fake player on conn and requests its bus name.
func Start(conn *dbus.Conn, opts Options) (*Player, error) {
	p := &Player{Conn: conn, opts: opts, posUS: opts.PositionUS, posAt: time.Now(), status: opts.Status}
	if p.status == "" {
		p.status = "Stopped"
	}
	root := &rootHandler{p}
	if err := conn.Export(root, path, ifaceRoot); err != nil {
		return nil, err
	}
	if err := conn.ExportWithMap(&playerHandler{p}, map[string]string{"SeekBy": "Seek"}, path, ifacePlayer); err != nil {
		return nil, err
	}
	playerProps := map[string]*fprop{
		"PlaybackStatus": {Value: p.status, Emit: emitTrue},
		"Rate":           {Value: 1.0, Emit: emitTrue},
		"Position":       {Value: opts.PositionUS, Emit: emitFalse},
		"CanControl":     {Value: opts.CanControl, Emit: emitConst},
		"CanPlay":        {Value: opts.CanPlay, Emit: emitTrue},
		"CanPause":       {Value: opts.CanPause, Emit: emitTrue},
		"CanGoNext":      {Value: opts.CanGoNext, Emit: emitTrue},
		"CanGoPrevious":  {Value: opts.CanGoPrevious, Emit: emitTrue},
		"CanSeek":        {Value: opts.CanSeek, Emit: emitTrue},
		"LoopStatus":     {Value: "None", Writable: true, Emit: emitTrue},
		"Shuffle":        {Value: false, Writable: true, Emit: emitTrue},
	}
	if !opts.OmitMetadata {
		playerProps["Metadata"] = &fprop{Value: p.metadataLocked(), Emit: emitTrue}
	}
	if opts.Volume != nil {
		playerProps["Volume"] = &fprop{Value: *opts.Volume, Writable: !opts.VolumeReadOnly, Emit: emitTrue}
	}
	rootProps := map[string]*fprop{
		"Identity":            {Value: opts.Identity, Emit: emitTrue},
		"CanQuit":             {Value: false, Emit: emitConst},
		"CanRaise":            {Value: false, Emit: emitConst},
		"HasTrackList":        {Value: false, Emit: emitConst},
		"SupportedUriSchemes": {Value: []string{}, Emit: emitConst},
		"SupportedMimeTypes":  {Value: []string{}, Emit: emitConst},
	}
	if opts.DesktopEntry != "" {
		rootProps["DesktopEntry"] = &fprop{Value: opts.DesktopEntry, Emit: emitConst}
	}
	props, err := exportProps(conn, map[string]map[string]*fprop{ifaceRoot: rootProps, ifacePlayer: playerProps})
	if err != nil {
		return nil, err
	}
	p.props = props
	node := &introspect.Node{
		Name: string(path),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			propsIntrospectData,
			{Name: ifaceRoot, Methods: introspect.Methods(root), Properties: props.Introspection(ifaceRoot)},
			{Name: ifacePlayer, Methods: introspect.Methods(&playerHandler{p}), Properties: props.Introspection(ifacePlayer)},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	reply, err := conn.RequestName(p.BusName(), dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, fmt.Errorf("name %s already taken", p.BusName())
	}
	return p, nil
}

// Stop releases the bus name (the player disappears).
func (p *Player) Stop() error {
	_, err := p.Conn.ReleaseName(p.BusName())
	return err
}

// Calls returns the methods called so far.
func (p *Player) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *Player) metadataLocked() map[string]dbus.Variant {
	if p.opts.RawMetadata != nil {
		return p.opts.RawMetadata
	}
	if len(p.opts.Tracks) == 0 {
		return map[string]dbus.Variant{"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/org/mpris/MediaPlayer2/TrackList/NoTrack"))}
	}
	t := p.opts.Tracks[p.trackIdx]
	md := map[string]dbus.Variant{
		"mpris:trackid": dbus.MakeVariant(dbus.ObjectPath("/org/example/track/" + t.ID)),
	}
	if t.Title != "" {
		md["xesam:title"] = dbus.MakeVariant(t.Title)
	}
	if len(t.Artists) > 0 {
		md["xesam:artist"] = dbus.MakeVariant(t.Artists)
	}
	if t.Album != "" {
		md["xesam:album"] = dbus.MakeVariant(t.Album)
	}
	if t.ArtURL != "" {
		md["mpris:artUrl"] = dbus.MakeVariant(t.ArtURL)
	}
	if t.URL != "" {
		md["xesam:url"] = dbus.MakeVariant(t.URL)
	}
	if t.LengthUS > 0 {
		md["mpris:length"] = dbus.MakeVariant(t.LengthUS)
	}
	if t.Lyrics != "" {
		md["xesam:asText"] = dbus.MakeVariant(t.Lyrics)
	}
	return md
}

// SetStatus changes PlaybackStatus (emitting PropertiesChanged).
func (p *Player) SetStatus(status string) {
	p.mu.Lock()
	p.updatePosLocked()
	p.status = status
	p.mu.Unlock()
	p.set(ifacePlayer, "PlaybackStatus", status)
}

// SetTrack switches to track i (emitting PropertiesChanged).
func (p *Player) SetTrack(i int) {
	p.mu.Lock()
	p.trackIdx = i
	p.posUS, p.posAt = 0, time.Now()
	md := p.metadataLocked()
	p.mu.Unlock()
	p.set(ifacePlayer, "Position", int64(0))
	p.set(ifacePlayer, "Metadata", md)
}

// SetProperty sets an arbitrary Player property.
func (p *Player) SetProperty(name string, v any) {
	p.set(ifacePlayer, name, v)
}

// Property returns the current value of a Player property.
func (p *Player) Property(name string) any {
	v, err := p.props.Get(ifacePlayer, name)
	if err != nil {
		return nil
	}
	return v.Value()
}

func (p *Player) updatePosLocked() {
	if p.status == "Playing" {
		p.posUS += time.Since(p.posAt).Microseconds()
	}
	p.posAt = time.Now()
	p.set(ifacePlayer, "Position", p.posUS)
}

// PositionUS returns the current (extrapolated) position.
func (p *Player) PositionUS() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.updatePosLocked()
	return p.posUS
}

func (p *Player) record(m string) *dbus.Error {
	if p.opts.MethodDelay > 0 {
		time.Sleep(p.opts.MethodDelay)
	}
	p.mu.Lock()
	p.calls = append(p.calls, m)
	p.mu.Unlock()
	return nil
}

type rootHandler struct{ p *Player }

func (r *rootHandler) Raise() *dbus.Error { return r.p.record("Raise") }
func (r *rootHandler) Quit() *dbus.Error  { return r.p.record("Quit") }

type playerHandler struct{ p *Player }

func (h *playerHandler) Play() *dbus.Error {
	h.p.record("Play")
	if !h.p.opts.IgnoreCommands && h.p.opts.CanPlay {
		h.p.SetStatus("Playing")
	}
	return nil
}

func (h *playerHandler) Pause() *dbus.Error {
	h.p.record("Pause")
	if !h.p.opts.IgnoreCommands && h.p.opts.CanPause {
		h.p.SetStatus("Paused")
	}
	return nil
}

func (h *playerHandler) PlayPause() *dbus.Error {
	h.p.record("PlayPause")
	if h.p.opts.IgnoreCommands {
		return nil
	}
	h.p.mu.Lock()
	st := h.p.status
	h.p.mu.Unlock()
	if st == "Playing" {
		h.p.SetStatus("Paused")
	} else {
		h.p.SetStatus("Playing")
	}
	return nil
}

func (h *playerHandler) Stop() *dbus.Error {
	h.p.record("Stop")
	if !h.p.opts.IgnoreCommands {
		h.p.SetStatus("Stopped")
	}
	return nil
}

func (h *playerHandler) Next() *dbus.Error {
	h.p.record("Next")
	if i := h.p.track(); !h.p.opts.IgnoreCommands && h.p.opts.CanGoNext && i+1 < len(h.p.opts.Tracks) {
		h.p.SetTrack(i + 1)
	}
	return nil
}

func (h *playerHandler) Previous() *dbus.Error {
	h.p.record("Previous")
	if i := h.p.track(); !h.p.opts.IgnoreCommands && h.p.opts.CanGoPrevious && i > 0 {
		h.p.SetTrack(i - 1)
	}
	return nil
}

func (h *playerHandler) SeekBy(offset int64) *dbus.Error {
	h.p.record(fmt.Sprintf("Seek(%d)", offset))
	if h.p.opts.IgnoreCommands || !h.p.opts.CanSeek {
		return nil
	}
	h.p.mu.Lock()
	h.p.updatePosLocked()
	h.p.posUS += offset
	if h.p.posUS < 0 {
		h.p.posUS = 0
	}
	pos := h.p.posUS
	h.p.set(ifacePlayer, "Position", pos)
	h.p.mu.Unlock()
	h.p.seeked(pos)
	return nil
}

func (h *playerHandler) SetPosition(track dbus.ObjectPath, pos int64) *dbus.Error {
	h.p.record(fmt.Sprintf("SetPosition(%s,%d)", track, pos))
	if h.p.opts.IgnoreCommands || !h.p.opts.CanSeek {
		return nil
	}
	h.p.mu.Lock()
	h.p.posUS, h.p.posAt = pos, time.Now()
	h.p.set(ifacePlayer, "Position", pos)
	h.p.mu.Unlock()
	h.p.seeked(pos)
	return nil
}

func (h *playerHandler) OpenUri(uri string) *dbus.Error { return h.p.record("OpenUri") }

func (p *Player) seeked(pos int64) {
	if p.opts.NoSeekedSignal {
		return
	}
	_ = p.Conn.Emit(path, ifacePlayer+".Seeked", pos)
}

func (p *Player) track() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.trackIdx
}

// set updates a property and emits PropertiesChanged; errors from a closed
// connection are ignored (a fake player may still be finishing a delayed
// method call after a test ends).
func (p *Player) set(iface, name string, v any) {
	p.props.set(iface, name, v)
}
